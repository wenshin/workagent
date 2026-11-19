package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/tool"
)

// 任务清单工具名。
//
// 不叫 task_*：task_create / task_reply 已经被「跨人派发」占用，两者语义完全不同
// （一个在会话内规划，一个把事交给别人），同名会让模型选错工具。
const (
	planWriteToolName = "plan_write"
	planReadToolName  = "plan_read"

	// 清单规模上限：超过这个数量说明任务没拆好，模型该先收敛范围。
	maxPlanItems = 30
	// 单条标题上限：一行能读完才是清单，超过就该拆成两条。
	maxPlanItemRunes = 200
	maxPlanGoalRunes = 400
)

// planState 是一次运行内的清单状态。
//
// 必须闭包在 Run 里而不是放在 Agent 上：Agent 是进程内共享单例，
// 挂在结构体上会让两个并发会话的清单互相覆盖。
type planState struct {
	mu   sync.Mutex
	plan agent.Plan
}

func newPlanState(seed *agent.Plan) *planState {
	s := &planState{}
	if seed != nil {
		s.plan = *seed
		s.plan.Normalize()
	}
	return s
}

// snapshot 返回可安全落库的副本；空清单返回 nil（不往快照里塞空对象）。
func (s *planState) snapshot() *agent.Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan.Empty() {
		return nil
	}
	cp := s.plan
	cp.Items = append([]agent.PlanItem(nil), s.plan.Items...)
	return &cp
}

// replace 整份覆盖清单。用覆盖而不是增量更新：模型每次都给出完整清单，
// 结果只取决于最后一次调用，不依赖「之前到底写过什么」。
func (s *planState) replace(p agent.Plan) agent.Plan {
	p.Normalize()
	if len(p.Items) > maxPlanItems {
		p.Items = p.Items[:maxPlanItems]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plan = p
	return s.plan
}

// planTools 返回任务清单工具。
//
// 只给顶层会话：子 Agent 的任务边界由派发方给定，再建清单只会放大跑偏。
// 工具本身不改任何外部状态（只改本 run 的内存与 checkpoint），所以按只读处理，
// 不触发审批——规划不该被打断。
func (a *Agent) planTools(state *planState) []tool.Tool {
	return []tool.Tool{
		{
			Name:       planWriteToolName,
			Capability: "agent.plan",
			Description: "写下或更新本次任务的任务清单（整份覆盖）。复杂任务开始时先用它列清单，" +
				"之后每完成一步就重新调用一次并更新各条状态。简单任务不要建清单。" +
				"状态取值：pending（待做）/ doing（进行中）/ done（已完成）/ blocked（卡住，缺信息或缺权限）。",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "goal": {"type": "string", "description": "这次任务要达成的结果，一句话"},
    "items": {
      "type": "array",
      "description": "完整清单，按执行顺序；整份覆盖上一次的内容",
      "items": {
        "type": "object",
        "properties": {
          "title": {"type": "string", "description": "一步要做的事，动词开头"},
          "status": {"type": "string", "enum": ["pending", "doing", "done", "blocked"]},
          "note": {"type": "string", "description": "产物路径、结论要点或卡住的原因"}
        },
        "required": ["title"],
        "additionalProperties": false
      }
    }
  },
  "required": ["items"],
  "additionalProperties": false
}`),
			Risk:        policy.RiskReadOnly,
			SideEffects: policy.SideEffectsReadOnly,
			Runtime:     tool.RuntimeBuiltin,
			Invoke: func(_ context.Context, args json.RawMessage, _ tool.Env) (string, error) {
				return writePlan(state, args)
			},
		},
		{
			Name:       planReadToolName,
			Capability: "agent.plan",
			Description: "读回当前任务清单与进度。续跑或不确定做到哪一步时先读一次，" +
				"不要凭记忆复述进度。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Risk:        policy.RiskReadOnly,
			SideEffects: policy.SideEffectsReadOnly,
			Runtime:     tool.RuntimeBuiltin,
			Invoke: func(_ context.Context, _ json.RawMessage, _ tool.Env) (string, error) {
				p := state.snapshot()
				if p.Empty() {
					return "当前还没有任务清单。复杂任务请先用 plan_write 列出来；简单任务可以不用清单。", nil
				}
				return p.Render(), nil
			},
		},
	}
}

// writePlan 是 plan_write 的执行体。
func writePlan(state *planState, args json.RawMessage) (string, error) {
	var in struct {
		Goal  string           `json:"goal"`
		Items []agent.PlanItem `json:"items"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	if len(in.Items) == 0 {
		return "", fmt.Errorf("items 不能为空：清单至少要有一步")
	}
	if len([]rune(in.Goal)) > maxPlanGoalRunes {
		return "", fmt.Errorf("goal 过长（上限 %d 字）", maxPlanGoalRunes)
	}
	for i := range in.Items {
		title := strings.TrimSpace(in.Items[i].Title)
		if title == "" {
			return "", fmt.Errorf("items[%d].title 不能为空", i)
		}
		if len([]rune(title)) > maxPlanItemRunes {
			return "", fmt.Errorf("items[%d].title 过长（上限 %d 字），请拆成两条", i, maxPlanItemRunes)
		}
	}
	plan := state.replace(agent.Plan{Goal: in.Goal, Items: in.Items})

	var b strings.Builder
	b.WriteString("清单已更新：\n")
	b.WriteString(plan.Render())
	b.WriteString("\n\n按清单继续；每完成一步就再调用一次 plan_write 更新状态。收尾时未完成的条目要单独说明。")
	return b.String(), nil
}
