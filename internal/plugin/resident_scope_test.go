package plugin

import (
	"io"
	"log/slog"
	"testing"

	"github.com/workagent/auto-agent/internal/storage/localfs"
)

// 常驻与否只看清单，不看发布状态。
//
// 未发布的草稿被启用之后就是这个空间真正在用的插件；如果按「有没有发布」分成
// 两套执行形态，作者在启用后调两次会看到模块级状态丢失，与发布后完全不是一回事，
// 调试得到的所有结论都不可信。
func TestResidentEligibleIgnoresPublishState(t *testing.T) {
	tool := func(resident bool) Manifest {
		return Manifest{Kind: KindTool, Runtime: RuntimeSpec{Entry: "main.ts", Resident: resident}}
	}
	cases := []struct {
		name string
		inst *instance
		want bool
	}{
		{"已发布的常驻工具", &instance{Manifest: tool(true)}, true},
		{"未发布的常驻工具", &instance{Local: true, Version: "local", Manifest: tool(true)}, true},
		{"声明一次性", &instance{Manifest: tool(false)}, false},
		{"技能没有执行器", &instance{Manifest: Manifest{Kind: KindSkill}}, false},
		{"MCP 跑在远端", &instance{Manifest: Manifest{Kind: KindMCP}}, false},
		// 内置工具由 Go 实现，没有脚本进程可常驻
		{"内置工具", &instance{Manifest: Manifest{
			Kind: KindTool, Builtin: true, Runtime: RuntimeSpec{Resident: true},
		}}, false},
		{"空实例", nil, false},
	}
	for _, c := range cases {
		if got := residentEligible(c.inst); got != c.want {
			t.Errorf("%s：residentEligible=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// 本地草稿的调用参数不能因为「还没发布」被翻成一次性执行。
//
// 真的翻过去，症状是：启用后第一次调用起进程、第二次又起一个，模块变量与连接
// 全丢，而作者已经在清单里写了 runtime.resident。
func TestLocalDraftKeepsResidentInvokeInput(t *testing.T) {
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化本地存储失败: %v", err)
	}
	m := &Manager{o: Options{Storage: fs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	inst := &instance{
		SpaceID: "space-1", Slug: "demo", Version: "local", Local: true,
		Dir: DraftKey(fs, "space-1", "demo"),
		Manifest: Manifest{
			Kind: KindTool, Runtime: RuntimeSpec{Entry: "main.ts", Resident: true},
		},
	}
	in, err := m.toolInvokeInput(inst, "u1")
	if err != nil {
		t.Fatalf("组装调用参数失败: %v", err)
	}
	if in.OneShot {
		t.Fatal("草稿声明了 runtime.resident，不该走一次性执行")
	}
	// 草稿的 DENO_DIR 与发布件隔离：混在一起会拿到正式版本的编译产物
	if in.Version != "local" {
		t.Fatalf("草稿版本标记被改写成了 %q", in.Version)
	}
}
