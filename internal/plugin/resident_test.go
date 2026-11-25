package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// counterPlugin 是一个假插件进程：每收到一个 invoke 就把计数加一并回包。
//
// 计数存在进程内，所以「第二次调用拿到 2」就等于证明了进程被复用——
// 重启过的进程只会从 1 重新开始。
const counterPlugin = `
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '{"op":"return","data":"%s"}\n' "$n"
done
`

// crashPlugin 收到 invoke 就退出，每次启动都是这样。
const crashPlugin = `
read -r line
exit 3
`

// crashOncePlugin 只在第一次启动时崩，之后正常回包。
// 用标记文件区分「第几次启动」——进程内变量随进程一起没了。
const crashOncePlugin = `
mark="$1"
if [ -f "$mark" ]; then
  n=0
  while IFS= read -r line; do
    n=$((n+1))
    printf '{"op":"return","data":"ok%s"}\n' "$n"
  done
fi
touch "$mark"
read -r line
exit 7
`

func startShell(t *testing.T, script string) func() (*sandbox.Proc, error) {
	t.Helper()
	return startShellArgs(t, script)
}

// startShellArgs 允许向脚本传位置参数（$1 起），供「第 N 次启动」这类场景用。
func startShellArgs(t *testing.T, script string, args ...string) func() (*sandbox.Proc, error) {
	t.Helper()
	dir := t.TempDir()
	cmd := append([]string{"/bin/sh", "-c", script, "wa"}, args...)
	return func() (*sandbox.Proc, error) {
		return sandbox.NewLocal().Start(context.Background(), sandbox.Spec{
			Cmd:      cmd,
			Dir:      dir,
			Resident: true,
		})
	}
}

func testRuntime() *toolRuntime {
	return &toolRuntime{logger: slog.Default()}
}

func invokeInputForTest() invokeInput {
	return invokeInput{SpaceID: "space-1", Slug: "demo", Dir: "/tmp/demo", Limits: Limits{TimeoutMS: 3000}}
}

// 模块加载期读配置/凭证必须落到启动参数上：这是「重启后 status 突然未配置」的根因。
//
// 预热只起进程、不发 invoke 帧，插件顶层 await wa.config() 时没有调用上下文；
// 没有这条兜底就会命中「没有正在执行的调用」，插件把空配置缓存进模块级状态，
// 之后调用多少次都不再重读——而安装页里配置一直是好的。
func TestPickHostCallInput(t *testing.T) {
	cur := invokeInputForTest()
	base := invokeInputForTest()
	ctx := context.Background()

	if got, gotCtx := pickHostCallInput(MethodHTTP, &cur, ctx, &base); got != &cur || gotCtx != ctx {
		t.Fatal("有正在执行的调用时必须用它，不能退回启动参数")
	}
	for _, m := range []string{MethodConfigGet, MethodSecretGet, MethodLog} {
		got, gotCtx := pickHostCallInput(m, nil, nil, &base)
		if got != &base {
			t.Fatalf("%s 在模块加载期应退回启动参数", m)
		}
		if gotCtx == nil {
			t.Fatalf("%s 需要一个可用的 context", m)
		}
	}
	if got, gotCtx := pickHostCallInput(MethodHTTP, nil, nil, &base); got != nil || gotCtx != nil {
		t.Fatal("出网在模块加载期必须仍然拒绝，不能借预热期绕过调用上下文")
	}
	if got, _ := pickHostCallInput(MethodConfigGet, nil, nil, nil); got != nil {
		t.Fatal("没有启动参数时不应凭空造上下文")
	}
}

// 同一个键必须复用同一个进程：常驻的全部意义在这里。
func TestResidentReusesProcess(t *testing.T) {
	pool := newResidentPool(4, 3, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	start := startShell(t, counterPlugin)

	p1, err := pool.acquire("s|a|d", rt, nil, start)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	out1, err := p1.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("第一次调用失败: %v", err)
	}
	if out1 != "1" {
		t.Fatalf("第一次应为 1，实际 %q", out1)
	}

	p2, err := pool.acquire("s|a|d", rt, nil, start)
	if err != nil {
		t.Fatalf("第二次 acquire: %v", err)
	}
	if p2 != p1 {
		t.Fatal("同一个键必须复用同一个进程")
	}
	out2, err := p2.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	// 进程内计数继续增长，说明没有重启
	if out2 != "2" {
		t.Fatalf("进程被重启了（拿到 %q）", out2)
	}
	if pool.size() != 1 {
		t.Fatalf("池里应该只有 1 个进程，实际 %d", pool.size())
	}
}

// 并发调用必须串行化在进程内，不能拿到别人的返回值。
func TestResidentSerializesCalls(t *testing.T) {
	pool := newResidentPool(4, 3, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	start := startShell(t, counterPlugin)
	p, err := pool.acquire("s|a|d", rt, nil, start)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	const n = 5
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`))
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发调用失败: %v", errs[i])
		}
		if seen[results[i]] {
			t.Fatalf("拿到重复结果 %q，说明帧错位了: %v", results[i], results)
		}
		seen[results[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("应有 %d 个不同结果，实际 %d: %v", n, len(seen), results)
	}
}

// 崩溃的那一次调用必须报错（不自动重放，避免重复副作用），
// 但进程要由守护协程拉起来——后续调用不能一直撞死进程。
func TestResidentRestartsAfterCrash(t *testing.T) {
	pool := newResidentPool(4, 3, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	const key = "s|b|d"
	mark := filepath.Join(t.TempDir(), "started")
	start := startShellArgs(t, crashOncePlugin, mark)

	p1, err := pool.acquire(key, rt, nil, start)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := p1.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("进程退出时调用应报错")
	}

	// 守护重启是异步的（300ms 退避），轮询等到一个活的新进程
	var p2 *residentProc
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rp, err := pool.acquire(key, rt, nil, start)
		if err == nil && rp != p1 && !rp.dead() {
			p2 = rp
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p2 == nil {
		t.Fatal("崩溃后没有自动重启")
	}
	out, err := p2.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("重启后的进程应该能正常调用: %v", err)
	}
	// 第二次启动进入正常循环，进程内计数从 1 开始
	if out != "ok1" {
		t.Fatalf("拿到的是旧进程的结果: %q", out)
	}
}

// 连续崩溃超过上限就熔断：不再重启，调用直接报「插件不可用」，
// 避免每次调用都赔一遍冷启动。
func TestResidentStopsRestartAfterTooManyCrashes(t *testing.T) {
	pool := newResidentPool(4, 1, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	const key = "s|f|d"
	start := startShell(t, crashPlugin)

	var hit error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rp, err := pool.acquire(key, rt, nil, start)
		if err != nil {
			hit = err
			break
		}
		if _, err := rp.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`)); err != nil && strings.Contains(err.Error(), "停止自动重启") {
			hit = err
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil || !strings.Contains(hit.Error(), "停止自动重启") {
		t.Fatalf("连续崩溃后应熔断，实际 %v", hit)
	}
	if n, broken := pool.failed(key); !broken || n < 2 {
		t.Fatalf("熔断状态不对：attempts=%d broken=%v", n, broken)
	}
}

// 主动关闭（停用/卸载/空间重载）不能触发守护重启，否则停用就成了摆设。
func TestResidentNoRestartAfterClose(t *testing.T) {
	pool := newResidentPool(4, 3, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	const key = "s1|k|d"
	if _, err := pool.acquire(key, rt, nil, startShell(t, counterPlugin)); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pool.closePrefix("s1|")
	time.Sleep(500 * time.Millisecond)
	if pool.size() != 0 {
		t.Fatalf("主动关闭后被重启了，池内 %d", pool.size())
	}
	// 清掉的是启动闭包：下一次调用按新参数重新拉起
	if _, err := pool.acquire(key, rt, nil, startShell(t, counterPlugin)); err != nil {
		t.Fatalf("重新调用应能起新进程: %v", err)
	}
	if pool.size() != 1 {
		t.Fatalf("应该只有一个进程，实际 %d", pool.size())
	}
}

// 停用 / 卸载 / 空间重载都走 closePrefix：前缀必须能精确匹配到目标插件。
func TestResidentClosePrefix(t *testing.T) {
	pool := newResidentPool(8, 3, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	start := startShell(t, counterPlugin)

	if _, err := pool.acquire("s1|a|d", rt, nil, start); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.acquire("s1|b|d", rt, nil, start); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.acquire("s2|a|d", rt, nil, start); err != nil {
		t.Fatal(err)
	}
	if pool.size() != 3 {
		t.Fatalf("应有 3 个进程，实际 %d", pool.size())
	}

	// 只关 s1|a，其它两个不受影响
	pool.closePrefix("s1|a|")
	if pool.size() != 2 {
		t.Fatalf("关掉一个后应剩 2 个，实际 %d", pool.size())
	}
	// 空间级失效关掉 s1 的全部
	pool.closePrefix("s1|")
	if pool.size() != 1 {
		t.Fatalf("空间失效后应剩 1 个，实际 %d", pool.size())
	}
	pool.CloseAll()
	if pool.size() != 0 {
		t.Fatalf("CloseAll 后应为 0，实际 %d", pool.size())
	}
}

// 常驻进程不能有墙钟：超过 Timeout 仍然要活着，否则「常驻」退化成「一次一进程」。
func TestResidentProcIgnoresTimeout(t *testing.T) {
	dir := t.TempDir()
	start := func() (*sandbox.Proc, error) {
		return sandbox.NewLocal().Start(context.Background(), sandbox.Spec{
			Cmd:      []string{"/bin/sh", "-c", counterPlugin},
			Dir:      dir,
			Resident: true,
			Timeout:  50 * time.Millisecond,
		})
	}
	pool := newResidentPool(2, 3, slog.Default())
	defer pool.CloseAll()
	p, err := pool.acquire("s|c|d", testRuntime(), nil, start)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	in := invokeInputForTest()
	if out, err := p.invoke(context.Background(), in, json.RawMessage(`{}`)); err != nil || out != "1" {
		t.Fatalf("常驻进程被超时误杀了: out=%q err=%v", out, err)
	}
}

// fs 白名单：相对路径与 .. 必须在发布前就被挡住。
func TestFSValidation(t *testing.T) {
	cases := []struct {
		name string
		fs   *FSSpec
		ok   bool
	}{
		{"空", nil, true},
		{"正常相对路径", &FSSpec{Read: []string{"data"}, Write: []string{"output/x"}}, true},
		// 整个空间不允许放进声明：读默认就是整个空间，写不给默认权限
		{"当前目录", &FSSpec{Read: []string{"."}}, false},
		{"绝对路径", &FSSpec{Read: []string{"/etc"}}, false},
		{"上跳", &FSSpec{Write: []string{"../other"}}, false},
		{"中间上跳", &FSSpec{Write: []string{"a/../../b"}}, false},
		{"空串", &FSSpec{Read: []string{"  "}}, false},
	}
	for _, c := range cases {
		if err := validateFS(c.fs); (err == nil) != c.ok {
			t.Errorf("%s: 期望 ok=%v，实际 err=%v", c.name, c.ok, err)
		}
	}
}

// resolveUnder 返回的是「相对空间工作区的干净路径」：它是挂载 key 的尾段，
// 不再是宿主绝对路径——绝对路径只在挂载那一侧拼出来。
func TestResolveUnder(t *testing.T) {
	root := t.TempDir()
	got, err := resolveUnder(root, "data/sub")
	if err != nil {
		t.Fatalf("正常路径不该报错: %v", err)
	}
	if got != "data/sub" {
		t.Fatalf("解析结果不对: %s", got)
	}
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../b", "", " ", "//", "."} {
		if _, err := resolveUnder(root, bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

// 常驻进程写到 stdout 的帧不能无限增长：畸形帧必须让进程停下来，
// 否则读循环会一直被垃圾数据喂饱。
func TestResidentRejectsGarbageFrame(t *testing.T) {
	dir := t.TempDir()
	start := func() (*sandbox.Proc, error) {
		return sandbox.NewLocal().Start(context.Background(), sandbox.Spec{
			Cmd:      []string{"/bin/sh", "-c", "echo not-json; sleep 30"},
			Dir:      dir,
			Resident: true,
		})
	}
	pool := newResidentPool(2, 3, slog.Default())
	defer pool.CloseAll()
	p, err := pool.acquire("s|e|d", testRuntime(), nil, start)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !p.dead() {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.dead() {
		t.Fatal("畸形帧应导致进程被关闭")
	}
	if _, err := p.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("死进程的调用应报错")
	}
}
