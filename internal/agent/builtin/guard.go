package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
)

// 进度守卫（§5.7.2 主循环）。
//
// 长任务失败的主因不是「轮次不够」，而是循环在做无用功：同一个工具用同样的参数反复调用、
// 每轮都拿到空结果、写了文件又读回来确认。这些动作会稳定地消耗预算，并且在轮次上限处
// 让用户收到一条空回答。守卫在每一轮结束后回答三个问题：
//
//	1. 这一步有没有带来新信息（gain）
//	2. 下一步还有没有价值（stalled 计数）
//	3. 这一轮该不该强制收尾（ForceFinal）
//
// 它的判断全部基于可观测的事实（调用指纹、结果摘要），不需要额外一次模型调用，
// 因此不会把「判断有没有进展」变成又一轮成本。

// maxDuplicateCalls 是「同一工具 + 相同参数」允许执行的次数。
// 第 2 次仍放行：模型换一种问法前先重试一次是合理的；第 3 次起拦截——
// 结果不会变，只会烧上下文和预算。
const maxDuplicateCalls = 2

// DuplicateBlockMarker 是拦截重复调用时回填文本的固定前缀。
// 黄金用例与前端据此识别「这一步没有真正执行」，而不是把它当成工具报错。
const DuplicateBlockMarker = "重复调用已拦截"

// guardDecision 是守卫对当前轮次的裁决。
//
// 只有一种动作：强制收尾（不再提供工具）。不直接终止循环，
// 因为终止循环的代价一定是「用户收到一条空回答」；而强制收尾还有机会拿到结论。
type guardDecision struct {
	// ForceFinal 表示本轮不再提供工具，要求模型直接给出结论
	ForceFinal bool
	// Reason 是收尾原因：budget（轮次用尽）/ stalled（连续无新信息）
	Reason string
	// Detail 是给模型和日志看的具体说明
	Detail string
}

// progressGuard 记录一次运行内的调用指纹与产出，判断是否还在前进。
type progressGuard struct {
	// stallLimit 是连续多少轮没有任何新信息后判定停滞
	stallLimit int
	// calls 是调用指纹 → 已执行次数
	calls map[string]int
	// outputs 是调用指纹 → 上次成功产出（用于识别「同样的调用、同样的结果」）
	outputs map[string]string
	// stalled 是连续无新信息的轮数
	stalled int
	// blocked 是累计拦截次数，供测试与日志使用
	blocked int
}

func newProgressGuard(stallLimit int) *progressGuard {
	if stallLimit <= 0 {
		stallLimit = 2
	}
	return &progressGuard{
		stallLimit: stallLimit,
		calls:      map[string]int{},
		outputs:    map[string]string{},
	}
}

// beforeRound 在调用模型前裁决：这一轮还给不给工具。
//
// 两种收尾触发条件：
//  1. 连续 stallLimit 轮没有任何新信息——继续给工具只会重复同样的动作；
//  2. 已经是最后一轮——再给工具也来不及用它产出结论。
//
// 两者都只「收回工具」，不终止运行：先让模型用现有信息作答，
// 拿不到内容才由调用方判定为 stalled / budget。
func (g *progressGuard) beforeRound(round, maxRounds int) guardDecision {
	if g.stalled >= g.stallLimit {
		return guardDecision{
			ForceFinal: true,
			Reason:     "stalled",
			Detail:     fmt.Sprintf("连续 %d 轮工具调用没有带来新信息", g.stalled),
		}
	}
	if maxRounds > 0 && round == maxRounds-1 {
		return guardDecision{
			ForceFinal: true,
			Reason:     "budget",
			Detail:     "已到最后一次模型调用",
		}
	}
	return guardDecision{}
}

// pendingCall 是模型返回的一次工具调用的精简视图，避免守卫依赖协议结构。
type pendingCall struct {
	ID   string
	Name string
	Args string
}

// inspect 在拿到模型返回的工具调用后登记指纹，返回被拦截的调用（callID → 原因）。
// 被拦截的调用不执行，直接把原因回填给模型。
func (g *progressGuard) inspect(calls []pendingCall) map[string]string {
	rejected := map[string]string{}
	for _, call := range calls {
		key := callKey(call.Name, call.Args)
		g.calls[key]++
		if g.calls[key] > maxDuplicateCalls {
			g.blocked++
			rejected[call.ID] = fmt.Sprintf(
				"%s：%s 用相同参数已执行 %d 次，结果不会变化。请换一种方法（换参数、换工具），或直接根据已有结果作答。",
				DuplicateBlockMarker, call.Name, g.calls[key]-1)
		}
	}
	return rejected
}

// callRecord 是一轮内一次工具调用的结果摘要，用于判定本轮有没有新信息。
type callRecord struct {
	Name   string
	Args   string
	Status string
	Output string
}

// afterRound 在一轮工具执行完成后更新进展状态，返回应当附加给模型的提醒（可为空）。
func (g *progressGuard) afterRound(records []callRecord) string {
	gain := false
	for _, r := range records {
		if r.Status != "ok" || strings.TrimSpace(r.Output) == "" {
			continue
		}
		key := callKey(r.Name, r.Args)
		prev, seen := g.outputs[key]
		if !seen || prev != r.Output {
			g.outputs[key] = r.Output
			gain = true
		}
	}
	if gain {
		g.stalled = 0
		return ""
	}
	g.stalled++
	return fmt.Sprintf(
		"[进度提醒] 上一轮的 %d 次工具调用没有带来任何新信息（结果为空、报错，或与之前完全相同）。"+
			"不要用同样的方式再试一次：换工具、换参数，或者直接用已有信息给出结论。", len(records))
}

// blockedCount 返回累计拦截次数。
func (g *progressGuard) blockedCount() int { return g.blocked }

// CountBlocked 统计一次运行里被守卫拦截的重复调用数。
// 拦截不走 Invoker，因此不会出现在 tool_call 表里，只能从执行轨迹判断。
func CountBlocked(steps []agent.Step) int {
	n := 0
	for _, s := range steps {
		if s.Type == "tool" && strings.HasPrefix(s.Result, DuplicateBlockMarker) {
			n++
		}
	}
	return n
}

// callKey 是「工具名 + 规范化参数」的指纹：等价参数必须得到同一个指纹，
// 否则模型只要调整空格或键顺序就能绕过重复检测。
func callKey(name, args string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + normalizeArgs(args)))
	return hex.EncodeToString(sum[:8])
}

// normalizeArgs 把参数压成稳定形式：JSON 对象的键顺序由 encoding/json 统一排序。
func normalizeArgs(args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return trimmed // 非法 JSON 原样参与指纹，报错交给 Invoker
	}
	out, err := json.Marshal(v)
	if err != nil {
		return trimmed
	}
	return string(out)
}
