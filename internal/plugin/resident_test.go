package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
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

// crashPlugin 收到 invoke 就退出，用来验证「进程死了能从池里摘掉」。
const crashPlugin = `
read -r line
exit 3
`

func startShell(t *testing.T, script string) func() (*sandbox.Proc, error) {
	t.Helper()
	dir := t.TempDir()
	return func() (*sandbox.Proc, error) {
		return sandbox.NewLocal().Start(context.Background(), sandbox.Spec{
			Cmd:      []string{"/bin/sh", "-c", script},
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

// 同一个键必须复用同一个进程：常驻的全部意义在这里。
func TestResidentReusesProcess(t *testing.T) {
	pool := newResidentPool(time.Minute, 4, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	start := startShell(t, counterPlugin)

	p1, err := pool.acquire("s|a|d", rt, start)
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

	p2, err := pool.acquire("s|a|d", rt, start)
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
	pool := newResidentPool(time.Minute, 4, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	start := startShell(t, counterPlugin)
	p, err := pool.acquire("s|a|d", rt, start)
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

// 进程崩溃后必须报错，并且能从池里摘掉，而不是每个后续调用都撞同一个死进程。
func TestResidentCrashIsDropped(t *testing.T) {
	pool := newResidentPool(time.Minute, 4, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	const key = "s|b|d"
	p, err := pool.acquire(key, rt, startShell(t, crashPlugin))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := p.invoke(context.Background(), invokeInputForTest(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("进程退出时调用应报错")
	}
	// 等待读循环发现 EOF
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !p.dead() {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.dead() {
		t.Fatal("进程已退出但状态没更新")
	}
	pool.drop(key, p)
	if pool.size() != 0 {
		t.Fatalf("死进程没被摘掉，池内 %d", pool.size())
	}
}

// 停用 / 卸载 / 空间重载都走 closePrefix：前缀必须能精确匹配到目标插件。
func TestResidentClosePrefix(t *testing.T) {
	pool := newResidentPool(time.Minute, 8, slog.Default())
	defer pool.CloseAll()
	rt := testRuntime()
	start := startShell(t, counterPlugin)

	if _, err := pool.acquire("s1|a|d", rt, start); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.acquire("s1|b|d", rt, start); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.acquire("s2|a|d", rt, start); err != nil {
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
	pool := newResidentPool(time.Minute, 2, slog.Default())
	defer pool.CloseAll()
	p, err := pool.acquire("s|c|d", testRuntime(), start)
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

func TestResolveUnder(t *testing.T) {
	root := t.TempDir()
	got, err := resolveUnder(root, "data/sub")
	if err != nil {
		t.Fatalf("正常路径不该报错: %v", err)
	}
	if got != filepath.Join(root, "data", "sub") {
		t.Fatalf("解析结果不对: %s", got)
	}
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../b", ""} {
		if _, err := resolveUnder(root, bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	// . 表示空间根（macOS 上 /var 会展开成 /private/var，所以比 realPath）
	if got, err := resolveUnder(root, "."); err != nil || got != realPath(root) {
		t.Errorf("`.` 应解析为空间根，得到 %q err=%v", got, err)
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
	pool := newResidentPool(time.Minute, 2, slog.Default())
	defer pool.CloseAll()
	p, err := pool.acquire("s|e|d", testRuntime(), start)
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
