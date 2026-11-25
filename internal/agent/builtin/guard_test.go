package builtin

import (
	"fmt"
	"strings"
	"testing"
)

func TestCallKeyNormalizesArguments(t *testing.T) {
	// 键顺序与空白不同必须得到同一个指纹：否则模型只要调一下格式就能绕过重复检测
	a := callKey("file_read", `{"path":"a.md","encoding":"utf-8"}`)
	b := callKey("file_read", `{ "encoding": "utf-8", "path": "a.md" }`)
	if a != b {
		t.Fatalf("等价参数应得到同一指纹：%s != %s", a, b)
	}
	if a == callKey("file_read", `{"path":"b.md"}`) {
		t.Fatal("不同参数不应共用指纹")
	}
	if a == callKey("file_list", `{"path":"a.md","encoding":"utf-8"}`) {
		t.Fatal("不同工具不应共用指纹")
	}
}

func TestGuardBlocksThirdIdenticalCall(t *testing.T) {
	g := newProgressGuard(2)
	call := pendingCall{ID: "c1", Name: "file_read", Args: `{"path":"a.md"}`}

	for i := 1; i <= 2; i++ {
		if rejected := g.inspect([]pendingCall{{ID: call.ID, Name: call.Name, Args: call.Args}}); len(rejected) != 0 {
			t.Fatalf("第 %d 次调用不应被拦截：%v", i, rejected)
		}
	}
	rejected := g.inspect([]pendingCall{{ID: "c3", Name: call.Name, Args: call.Args}})
	if _, ok := rejected["c3"]; !ok {
		t.Fatal("第 3 次相同调用应被拦截")
	}
	if !strings.HasPrefix(rejected["c3"], DuplicateBlockMarker) {
		t.Fatalf("拦截文案应带固定前缀：%q", rejected["c3"])
	}
	if g.blockedCount() != 1 {
		t.Fatalf("拦截计数应为 1，实际 %d", g.blockedCount())
	}
	// 换参数后重新计数，不能被永久封禁
	if rejected := g.inspect([]pendingCall{{ID: "c4", Name: call.Name, Args: `{"path":"b.md"}`}}); len(rejected) != 0 {
		t.Fatalf("换参数后不应被拦截：%v", rejected)
	}
}

func TestGuardTracksStall(t *testing.T) {
	g := newProgressGuard(2)

	// 第一轮拿到新结果：算进展
	if hint := g.afterRound([]callRecord{{Name: "file_read", Args: `{"path":"a.md"}`, Status: "ok", Output: "内容 A"}}); hint != "" {
		t.Fatalf("有进展时不应提醒，实际 %q", hint)
	}
	// 同样的结果连续出现：没有新信息，连续两轮后判定停滞
	for i := 0; i < 2; i++ {
		if hint := g.afterRound([]callRecord{{Name: "file_read", Args: `{"path":"a.md"}`, Status: "ok", Output: "内容 A"}}); hint == "" {
			t.Fatalf("第 %d 次重复结果应给出进度提醒", i+1)
		}
	}
	// 连续两轮无新信息后必须收尾
	if d := g.beforeRound(2, 8); !d.ForceFinal || d.Reason != "stalled" {
		t.Fatalf("连续无进展后应强制收尾，实际 %+v", d)
	}
}

func TestGuardEmptyResultsCountAsNoGain(t *testing.T) {
	g := newProgressGuard(2)
	rec := []callRecord{{Name: "plugin_search", Args: `{"query":"x"}`, Status: "ok", Output: ""}}
	if hint := g.afterRound(rec); hint == "" {
		t.Fatal("空结果不应算作进展")
	}
	// 报错同样不算进展
	rec = []callRecord{{Name: "shell_exec", Args: `{"cmd":"x"}`, Status: "error", Output: "boom"}}
	if hint := g.afterRound(rec); hint == "" {
		t.Fatal("报错不应算作进展")
	}
}

// 同参数 + 完全相同的结果就是「反复相同的命令调用」的确证：
// 第二次执行完发现结果没变，第三次调用直接拦掉。
func TestGuardBlocksRepeatedSameOutput(t *testing.T) {
	g := newProgressGuard(2)
	args := `{"cmd":"git status"}`
	step := func(id string) map[string]string {
		return g.inspect([]pendingCall{{ID: id, Name: "shell_exec", Args: args}})
	}
	record := func() {
		g.afterRound([]callRecord{{Name: "shell_exec", Args: args, Status: "ok", Output: "nothing to commit"}})
	}
	if r := step("c1"); len(r) != 0 {
		t.Fatalf("第 1 次不应拦截: %v", r)
	}
	record()
	if r := step("c2"); len(r) != 0 {
		t.Fatalf("第 2 次不应拦截（结果相同还没被观察到）: %v", r)
	}
	record()
	r := step("c3")
	reason, blocked := r["c3"]
	if !blocked {
		t.Fatalf("结果与上次完全相同，这一轮应被拦截: %v", r)
	}
	if !strings.HasPrefix(reason, DuplicateBlockMarker) || !strings.Contains(reason, "完全相同") {
		t.Fatalf("拦截文案应说明「结果完全相同」: %q", reason)
	}
}

// 结果一直在变的轮询是有效观察（等日志出现关键字、等构建结束），
// 不能因为「同参数调了很多次」就拦掉。
func TestGuardAllowsPollingWithChangingOutput(t *testing.T) {
	g := newProgressGuard(2)
	args := `{"cmd":"tail -n 20 build.log"}`
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("c%d", i)
		if r := g.inspect([]pendingCall{{ID: id, Name: "shell_exec", Args: args}}); len(r) != 0 {
			t.Fatalf("第 %d 轮结果在变，不应拦截: %v", i+1, r)
		}
		g.afterRound([]callRecord{{
			Name: "shell_exec", Args: args, Status: "ok",
			Output: fmt.Sprintf("round %d", i),
		}})
	}
}

// 拿不到可用回执（报错）时没有「结果是否相同」可比，仍按次数兜底。
func TestGuardBlocksRepeatedErrors(t *testing.T) {
	g := newProgressGuard(2)
	args := `{"cmd":"make"}`
	for i := 1; i <= 2; i++ {
		if r := g.inspect([]pendingCall{{ID: fmt.Sprintf("c%d", i), Name: "shell_exec", Args: args}}); len(r) != 0 {
			t.Fatalf("第 %d 次不应拦截: %v", i, r)
		}
		g.afterRound([]callRecord{{Name: "shell_exec", Args: args, Status: "error", Output: "boom"}})
	}
	if _, ok := g.inspect([]pendingCall{{ID: "c3", Name: "shell_exec", Args: args}})["c3"]; !ok {
		t.Fatal("同一命令连续失败 3 次应被拦截")
	}
}

func TestGuardForcesFinalOnLastRound(t *testing.T) {
	g := newProgressGuard(2)
	d := g.beforeRound(7, 8)
	if !d.ForceFinal || d.Reason != "budget" {
		t.Fatalf("最后一轮必须强制收尾，实际 %+v", d)
	}
	if d := g.beforeRound(0, 8); d.ForceFinal {
		t.Fatal("第一轮不应该收尾")
	}
}
