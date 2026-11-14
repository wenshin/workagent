package golden

import "context"

// Runner 把黄金用例接到宿主上（插件发布门禁、CI、本地回归）。
//
// 用例不依赖真实模型：模型回合由用例脚本提供，跑的是平台当前的 harness 实现，
// 因此这里不需要注入 agent 实例，只需要平台预算。
type Runner struct {
	// Options 是跑用例时的预算；零值用默认值
	Options Options
}

// RunAgentCases 实现 plugin.EvalRunner。
func (r Runner) RunAgentCases(_ context.Context, suite Suite) []CaseResult {
	out := make([]CaseResult, 0)
	for _, c := range suite.CasesByKind(KindAgent) {
		out = append(out, RunAgentCase(c, r.Options))
	}
	return out
}
