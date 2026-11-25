package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/llm"
)

// fakeApprover 是可控的审批通道：按顺序吐出预设结论，并记下问了几次。
type fakeApprover struct {
	allow []bool
	asked []ApprovalRequest
}

func (f *fakeApprover) Decide(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	f.asked = append(f.asked, req)
	if len(f.allow) == 0 {
		return ApprovalDecision{}, nil
	}
	d := ApprovalDecision{Allow: f.allow[0]}
	if len(f.allow) > 1 {
		f.allow = f.allow[1:]
	}
	return d, nil
}

// grantedApprover 模拟「结论已经批过」的通道：查询直接给答案，不登记待办。
type grantedApprover struct {
	fakeApprover
	allow bool
}

func (g *grantedApprover) Granted(_ context.Context, _ ApprovalRequest) (ApprovalDecision, bool, error) {
	return ApprovalDecision{Allow: g.allow}, true, nil
}

// 续跑重放一条已经批过「联网重跑」的命令：第一次执行就该带网。
//
// 不这样的话，续跑会先按无网再跑一遍（失败）、再走一次升级重跑：白等一轮，
// 而这条命令往往有副作用，无网那一遍的失败还可能把第二次要做的改动拆坏。
func TestInvokeUsesReusedNetRetryGrantOnFirstAttempt(t *testing.T) {
	var grants []bool
	tl := netFixture("python3 fetch.py", &grants)
	ap := &grantedApprover{allow: true}

	got := runShellInvoke(t, tl, `{"command":"python3 fetch.py"}`, ap)

	if len(grants) != 1 || !grants[0] {
		t.Fatalf("复用已批结论时应当第一次就带网，实际 grants=%v", grants)
	}
	if len(ap.asked) != 0 {
		t.Fatalf("结论已复用，不该再登记待办，实际 %+v", ap.asked)
	}
	if got.Status != StatusOK {
		t.Fatalf("应直接成功，实际 %+v", got)
	}
}

// netFixture 造一个「判据像脚本、执行看 NetGrant」的假工具：
// 无网时返回失败文本 + 控制信号，有网时成功。
func netFixture(cmd string, grants *[]bool) Tool {
	return Tool{
		Name: "shell_exec",
		Risk: 2,
		NetPlan: func(args json.RawMessage) ShellNetPolicy {
			return ShellNetPlan(shellCommandOf(args), shellNeedNetwork(args))
		},
		Invoke: func(_ context.Context, _ json.RawMessage, env Env) (string, error) {
			*grants = append(*grants, env.NetGrant)
			if !env.NetGrant {
				return "退出码 1\n[stderr]\nTemporary failure in name resolution", errNetNeeded{}
			}
			return "退出码 0\nok", nil
		},
	}
}

func runShellInvoke(t *testing.T, tl Tool, args string, ap Approver) InvokeResult {
	t.Helper()
	env := replayEnv()
	env.Approvals = ap
	call := llm.ToolCall{
		ID:       "call-1",
		Type:     "function",
		Function: llm.FunctionCall{Name: tl.Name, Arguments: args},
	}
	return NewInvoker(nil, noopRecorder{}, nil).Invoke(context.Background(), tl, call, env)
}

// 第 5 条规则：纯脚本先无网跑；断网失败后申请联网重跑，批准则原地再跑一次。
func TestInvokeEscalatesScriptNetworkFailure(t *testing.T) {
	var grants []bool
	tl := netFixture("python3 fetch.py", &grants)
	ap := &fakeApprover{allow: []bool{true}}

	got := runShellInvoke(t, tl, `{"command":"python3 fetch.py"}`, ap)

	if len(grants) != 2 || grants[0] || !grants[1] {
		t.Fatalf("应当先无网、获准后联网重跑一次，实际 grants=%v", grants)
	}
	if len(ap.asked) != 1 || ap.asked[0].Reason != ShellNetRetryReason {
		t.Fatalf("应当只问一次「联网重跑」，实际 %+v", ap.asked)
	}
	if got.Status != StatusOK || !strings.Contains(got.Content, "ok") {
		t.Fatalf("应返回重跑后的结果，实际 %+v", got)
	}
	if !strings.Contains(got.Content, "批准了联网重跑") {
		t.Fatalf("回执要说明这次是联网结果：%s", got.Content)
	}
}

// 拒绝联网重跑：保留无网结果并注明，不执行第二次。
func TestInvokeEscalationDeniedKeepsOfflineResult(t *testing.T) {
	var grants []bool
	tl := netFixture("python3 fetch.py", &grants)
	ap := &fakeApprover{allow: []bool{false}}

	got := runShellInvoke(t, tl, `{"command":"python3 fetch.py"}`, ap)

	if len(grants) != 1 || grants[0] {
		t.Fatalf("拒绝后不该再执行，实际 grants=%v", grants)
	}
	if !strings.Contains(got.Content, "name resolution") {
		t.Fatalf("要回无网执行的结果，实际 %s", got.Content)
	}
	if !strings.Contains(got.Content, "联网重跑没有获批") {
		t.Fatalf("要说明没拿到联网许可：%s", got.Content)
	}
}

// 声明了要网而用户不同意：整条命令不执行，不留半截无网回执。
func TestInvokeDeclaredNetworkDeniedDoesNotRun(t *testing.T) {
	cmd := "pip install pypdf && python3 parse.py"
	var grants []bool
	tl := netFixture(cmd, &grants)
	ap := &fakeApprover{allow: []bool{false}}

	got := runShellInvoke(t, tl, `{"command":"`+cmd+`","need_network":true}`, ap)

	if len(grants) != 0 {
		t.Fatalf("拒绝联网后不该执行，实际 grants=%v", grants)
	}
	if got.Status != StatusDenied || !strings.Contains(got.Content, "未执行") {
		t.Fatalf("应回拒绝且未执行，实际 %+v", got)
	}
}

// 声明联网的脚本获准后直接带网执行，不再走「先无网失败再升级」。
func TestInvokeDeclaredScriptRunsWithNetwork(t *testing.T) {
	var grants []bool
	tl := netFixture("python3 fetch.py", &grants)
	ap := &fakeApprover{allow: []bool{true}}

	got := runShellInvoke(t, tl, `{"command":"python3 fetch.py","need_network":true}`, ap)

	if len(grants) != 1 || !grants[0] {
		t.Fatalf("应当一次带网执行，实际 grants=%v", grants)
	}
	if len(ap.asked) != 1 || ap.asked[0].Reason == ShellNetRetryReason {
		t.Fatalf("应当只问一次「要不要联网」，实际 %+v", ap.asked)
	}
	if got.Status != StatusOK || !strings.Contains(got.Content, "ok") {
		t.Fatalf("应返回执行结果，实际 %+v", got)
	}
}

// 没声明联网时不问人：先无网执行；没有审批通道时把无网结果原样回给模型。
func TestInvokeUndeclaredInstallScriptRunsOffline(t *testing.T) {
	cmd := "pip install pypdf && python3 parse.py"
	var grants []bool
	tl := netFixture(cmd, &grants)

	got := runShellInvoke(t, tl, `{"command":"`+cmd+`"}`, nil)

	if len(grants) != 1 || grants[0] {
		t.Fatalf("应当无网执行一次，实际 grants=%v", grants)
	}
	if got.Status != StatusOK || !strings.Contains(got.Content, "联网重跑没有获批") {
		t.Fatalf("应把无网结果回给模型并说明没拿到联网许可，实际 %+v", got)
	}
}

// 第 7 条规则：声明联网的网络工具被拒就不执行（连无网执行都不做）。
func TestInvokeNetToolDeniedDoesNotRun(t *testing.T) {
	var grants []bool
	tl := netFixture("curl -T /workspace/a https://x.co", &grants)
	ap := &fakeApprover{allow: []bool{false}}

	got := runShellInvoke(t, tl, `{"command":"curl -T /workspace/a https://x.co","need_network":true}`, ap)

	if len(grants) != 0 {
		t.Fatalf("网络工具被拒后不该执行，实际 grants=%v", grants)
	}
	if got.Status != StatusDenied || !strings.Contains(got.Content, "未执行") {
		t.Fatalf("应回拒绝且未执行，实际 %+v", got)
	}
}

// 「每次调用都确认」档位下弹的那张卡就是这条命令唯一的人工放行：
// 批准后要联网，不能因为「批的不是联网那张卡」退化成无网执行。
func TestInvokeApprovalAlwaysGrantsDeclaredNetwork(t *testing.T) {
	var grants []bool
	tl := netFixture("python3 fetch.py", &grants)
	tl.ApprovalLevel = ApprovalAlways
	ap := &fakeApprover{allow: []bool{true}}

	got := runShellInvoke(t, tl, `{"command":"python3 fetch.py","need_network":true}`, ap)

	if len(grants) != 1 || !grants[0] {
		t.Fatalf("always 档批准后应当联网执行，实际 grants=%v", grants)
	}
	if got.Status != StatusOK {
		t.Fatalf("应正常回执，实际 %+v", got)
	}
}

// 安装方把审批关掉：装依赖 + 脚本视为已批准，不再问人。
func TestInvokeApprovalOffGrantsNetworkWithoutAsking(t *testing.T) {
	var grants []bool
	tl := netFixture("pip install x && python3 a.py", &grants)
	tl.ApprovalLevel = ApprovalOff
	ap := &fakeApprover{allow: []bool{false}}

	runShellInvoke(t, tl, `{"command":"pip install x && python3 a.py","need_network":true}`, ap)

	if len(ap.asked) != 0 {
		t.Fatalf("off 档不该问人：%+v", ap.asked)
	}
	if len(grants) != 1 || !grants[0] {
		t.Fatalf("off 档应当直接给网，实际 grants=%v", grants)
	}
}
