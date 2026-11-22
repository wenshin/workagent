package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// heartbeatInterval 是刷新 run 心跳的最小间隔。
//
// 心跳只用来判活，不必每个 token 都写一次库；十几秒一次，足以让清扫与 active-run
// 在一个 idle 周期内确认「这一轮还活着」。
const heartbeatInterval = 15 * time.Second

// maxResumePayload 是断点快照的大小上限。
//
// 快照里是完整消息序列（含工具回执），长任务几十轮下来会很大；超过这个量级说明
// 上下文本身已经不正常了，宁可放弃续跑能力，也不要把主库写满。
const maxResumePayload = 8 << 20

// ErrNoResume 表示这个会话没有可接着走的执行。
var ErrNoResume = errors.New("没有可以继续的执行")

// ErrResumeUnsupported 表示当前 Agent 实现不支持从断点续跑。
var ErrResumeUnsupported = errors.New("当前 Agent 实现不支持继续执行")

// ErrResumeRunning 表示要续的那一轮其实还在跑。
var ErrResumeRunning = errors.New("上一次执行还在进行，无需继续")

// runSeed 是「这一轮要跑什么」：新对话给历史与输入，续跑给断点快照。
type runSeed struct {
	History     []llm.Message
	Query       string
	ActiveSkill *agent.ActiveSkill
	AgentPrompt string
	Resume      *agent.ResumeState

	// ClientClock 是用户端上报的时间与时区。零值表示这一轮没有用户端
	// （任务回执、续跑），注入时回落服务器时间。
	ClientClock ClientClock

	// PrevRunID 是被这一轮接替的执行（续跑时非空）：审批结论、工具结果
	// 都挂在旧 run 上，恢复要沿这条链去找。
	PrevRunID string
	// Replay 是旧 run 已经产生过的工具结果（key 为工具 + 参数指纹）。
	// 命中就不执行，直接回放：副作用不能因为续跑再发生一次。
	Replay map[string]tool.InvokeResult
	// Carry 是暂停那一轮已经写下的消息（续跑时非空）：
	// 这一次接着它写，正文、轨迹与用量都并回同一条，界面上看到的是一轮。
	Carry *store.Message
}

// runWatchdog 是「这一轮卡住了」的唯一判据。
//
// 总时长不该是成败判据：长任务本来就该跑很久，只要还在产出事件就不该被掐断。
// 真正说明问题的是「连续 idle 没有任何事件」——模型卡在一条流上、工具挂住、
// 或者进程半死。总上限只作为最后一道保险，防「确实在动但永远跑不完」。
type runWatchdog struct {
	idle  time.Duration
	idleT *time.Timer
	maxT  *time.Timer
	fire  func(reason string)
	// stopped 兼作「已经触发过」的闸：idle 与总上限两个计时器可能同时到点
	stopped atomic.Bool
}

// newRunWatchdog 起两个计时器：idle（无事件）与 max（总上限）。两者都可为 0（关闭）。
func newRunWatchdog(idle, max time.Duration, fire func(reason string)) *runWatchdog {
	w := &runWatchdog{idle: idle, fire: fire}
	if idle > 0 {
		w.idleT = time.AfterFunc(idle, func() { w.trigger(ReasonIdleTimeout) })
	}
	if max > 0 {
		w.maxT = time.AfterFunc(max, func() { w.trigger(ReasonWallClockExceeded) })
	}
	return w
}

// touch 重置 idle 计时：有事件就说明还在动。
func (w *runWatchdog) touch() {
	if w == nil || w.idleT == nil || w.stopped.Load() {
		return
	}
	w.idleT.Reset(w.idle)
}

// stop 关掉计时器（正常收尾时调用）。
func (w *runWatchdog) stop() {
	if w == nil {
		return
	}
	w.stopped.Store(true)
	if w.idleT != nil {
		w.idleT.Stop()
	}
	if w.maxT != nil {
		w.maxT.Stop()
	}
}

func (w *runWatchdog) trigger(reason string) {
	if w == nil || !w.stopped.CompareAndSwap(false, true) {
		return
	}
	// 这里不碰 idleT / maxT：AfterFunc 可能在字段赋值完成前就醒来（-race 会报
	// 「构造中的计时器字段被并发读写」）。两个计时器的互斥由 stopped 的 CAS 保证，
	// 关计时器统一在 stop() 里做。
	w.fire(reason)
}

// createRun 建档，并把实现声明的预算写进 run.budget（事后可以查当时的上限）。
func (s *Service) createRun(ctx context.Context, sessionID string) (store.Run, error) {
	budgetMap := s.agent.Budget()
	if budgetMap == nil {
		budgetMap = map[string]any{}
	}
	// 记录实际生效的实现，便于按实现定位问题（AGENT_IMPL 可切换）
	budgetMap["impl"] = s.agent.Name()
	budget, err := json.Marshal(budgetMap)
	if err != nil {
		budget = []byte(`{}`)
	}
	return s.store.CreateRun(ctx, store.Run{SessionID: sessionID, Budget: budget})
}

// execute 跑完一轮：起看门狗、推事件、落 checkpoint、收尾落库。
//
// Chat 与 Resume 共用这一份实现：两者只有「拿到哪些消息」不同，而中断判定、
// 落库与断点去留必须完全一致，否则续跑的收尾会和新对话的收尾对不上。
func (s *Service) execute(
	ctx context.Context,
	sess store.Session,
	sp store.Space,
	user store.User,
	userMsg store.Message,
	run store.Run,
	seed runSeed,
	out ChatOutcome,
	emit func(agent.Event, int) error,
) (ChatOutcome, error) {
	sessionID := sess.ID
	userID := user.ID

	// checkpoint 与事件推送共用一条链路，保证「推给前端的」与「落库的」一致
	seq := 0
	appendStep := func(typ string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		seq++
		stepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_, err = s.store.AppendRunStep(stepCtx, store.RunStep{
			RunID: run.ID, Seq: seq, Type: typ, Payload: data,
		})
		cancel()
		if err != nil {
			s.log.Warn("写入 run checkpoint 失败", "run_id", run.ID, "seq", seq, "err", err)
		}
	}

	// runCtx 剥离了请求的取消信号：客户端刷新、切页、断网都只解绑流，
	// 任务照旧跑完（§15.12.1）。只有显式取消、卡死或总上限能停它。
	//
	// 前置准备（读历史、落用户消息、建 run）仍然跟着请求走——客户端已经
	// 断开时没必要再为一个没人收的结果起任务，那才是真的浪费。
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	live := s.runs.add(run.ID, userID, cancelRun, s.stream, s.log)
	// approver 在下面才建好；这里只声明，收尾时用它回查「批了却没动」的待办
	var approver *runApprover
	defer func() {
		live.finish()
		s.runs.remove(run.ID, live)
		// 决策可能在我收尾之前就到了：那会儿 ResolveApproval 看到执行体还活着，
		// 只落库不续跑。摘掉登记之后再补一次，免得停在「批了却没动」。
		s.resumeParkedDecisions(context.WithoutCancel(ctx), userID, sessionID, approver)
	}()

	// 看门狗：连续没有事件才算卡住；总上限只是最后一道保险
	watch := newRunWatchdog(s.idleTimeout, s.runBudget, func(reason string) {
		if live.markCancelled(reason) {
			s.log.Warn("对话被中断", "session_id", sessionID, "run_id", run.ID, "reason", reason)
		}
		cancelRun()
	})
	defer watch.stop()

	// 事件先落内存缓冲再推客户端：客户端断了写入会失败，但缓冲还在，
	// 重连后按 offset 补齐（§15.12.2）。因此这里的错误一律吞掉——
	// 让「写不出去」去中断 Agent，正是刷新丢任务的老问题。
	// 首个正文增量（或首个工具调用）到达时占位一条 streaming 消息：刷新后列表能看出
	// 「这一轮真的在跑」，而不是空白。正文与帧由 live 每 1s 推到 Redis（§15.12.2），
	// 结束时才整段落库——中间不写 message 表，避免写放大。
	var (
		progressMu    sync.Mutex
		placeholderID string
	)

	ensurePlaceholder := func() {
		progressMu.Lock()
		defer progressMu.Unlock()
		if placeholderID != "" {
			return
		}
		// 续跑接上暂停那一轮的消息：这一轮还没写完，不是新的一条。
		if seed.Carry != nil {
			placeholderID = seed.Carry.ID
			return
		}
		placeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m, err := s.store.CreateStreamingMessage(placeCtx, sessionID)
		if err != nil {
			s.log.Warn("占位消息写入失败", "session_id", sessionID, "err", err)
			return
		}
		placeholderID = m.ID
	}

	// 心跳：只要还在出事件就刷新，清扫与 active-run 据此判活
	lastBeat := &atomic.Int64{}
	lastBeat.Store(time.Now().UnixNano())
	beat := func() {
		now := time.Now().UnixNano()
		prev := lastBeat.Load()
		if now-prev < int64(heartbeatInterval) {
			return
		}
		// CAS 失败说明别的协程刚刷过，不必重复写
		if !lastBeat.CompareAndSwap(prev, now) {
			return
		}
		beatCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := s.store.TouchRun(beatCtx, run.ID); err != nil {
			s.log.Warn("刷新执行心跳失败", "run_id", run.ID, "err", err)
		}
	}

	emitEvent := func(ev agent.Event) error {
		watch.touch()
		beat()
		// 先广播：正文/帧进内存缓冲，工具事件也进这一轮的帧序列
		offset := live.broadcast(ev)
		switch ev.Type {
		case agent.EventToolCall, agent.EventToolResult:
			// 工具调用也是「内容」：只有轨迹、没有正文的中断轮同样要能在列表里看到
			ensurePlaceholder()
			// 断点前强制推一次：工具可能跑几分钟，这段时间里切页要能看到已经说了什么
			live.flush(true)
			appendStep(ev.Type, ev.Tool.Payload())
		case agent.EventPlanUpdated:
			// 清单也是内容：只有轨迹、还没有正文的时刻，列表里也该看到这一轮在干什么。
			//
			// 只补占位与缓存，不落 run_step：清单是状态不是步骤，最终那份随结果进
			// message.parts，执行中的那份在 run_resume 里（plan_read 读的就是它）。
			// run_step 只留「发生过什么」，重复存一份既没人读，还会随覆盖次数一直涨。
			ensurePlaceholder()
			live.flush(true)
		case agent.EventApprovalReq:
			// 等人可能等满审批时限：进等待前先把已产出的正文推到缓存
			live.flush(true)
			appendStep(ev.Type, ev.Approval)
		case agent.EventMessageDelta:
			// 思考不占位：只有真的开始写正文了，才值得在列表里留一条记录
			if !ev.Reasoning {
				ensurePlaceholder()
			}
		}
		if emit != nil {
			_ = emit(ev, offset)
		}
		return nil
	}

	// run.started 由会话层补发：此时 run 已落库，前端可以据此做断线重连与取消
	emitEvent(agent.Event{Type: agent.EventRunStarted, RunID: run.ID})

	// 技能与智能体提示词：只有新对话需要，续跑已经固化在快照的 system 消息里
	if seed.Resume == nil {
		if sess.SkillPath != "" {
			skill, err := s.loadSkill(ctx, sess.SpaceID, sess.SkillPath)
			if err != nil {
				s.log.Warn("技能正文加载失败", "session_id", sessionID, "path", sess.SkillPath, "err", err)
			} else {
				seed.ActiveSkill = skill
			}
		}
	}

	// 智能体来自哪个空间：派发出来的子会话指向发起方空间，其余就是会话自己的空间。
	agentSpaceID := sess.SpaceID
	if sess.AgentSpaceID != "" {
		agentSpaceID = sess.AgentSpaceID
	}
	// 跨空间借用的插件引用：只借智能体自己声明的 requires，不摊来源空间的整份目录。
	// 工具执行仍按 sess.SpaceID，文件读写不受影响。
	var extraRefs []tool.PluginRef
	// 智能体自带技能：只并进本会话的检索池与提示词，空间索引里没有它们。
	var agentSkills []agent.SkillRef
	var agentInternals []tool.PluginEntry
	if sess.AgentID != "" && s.agentPlugins != nil {
		if seed.Resume == nil && seed.AgentPrompt == "" {
			if r, err := s.agentPlugins.ResolveAgentPlugin(ctx, agentSpaceID, sess.AgentID); err == nil {
				seed.AgentPrompt = r.Prompt
			} else {
				s.log.Warn("智能体设定加载失败",
					"session_id", sessionID, "agent_id", sess.AgentID, "agent_space", agentSpaceID, "err", err)
			}
		}
		// 续跑沿用快照里的系统提示词，不重新注入技能段落；但检索池要补上，
		// 否则「重启后 plugin_search 找不到刚才那个技能」会显得很怪。
		if list, err := s.agentPlugins.AgentInternals(ctx, agentSpaceID, sess.AgentID); err != nil {
			s.log.Warn("智能体内置技能加载失败",
				"session_id", sessionID, "agent_id", sess.AgentID, "agent_space", agentSpaceID, "err", err)
		} else {
			agentInternals = list
			if seed.Resume == nil {
				for _, e := range list {
					agentSkills = append(agentSkills, agent.SkillRef{
						Name: e.Name, Description: e.Description, Path: e.Path,
					})
				}
			}
		}
		if agentSpaceID != sess.SpaceID {
			deps, err := s.agentPlugins.AgentDependencies(ctx, agentSpaceID, sess.AgentID)
			if err != nil {
				s.log.Warn("智能体依赖加载失败",
					"session_id", sessionID, "agent_id", sess.AgentID, "agent_space", agentSpaceID, "err", err)
			}
			for _, slug := range deps {
				extraRefs = append(extraRefs, tool.PluginRef{SpaceID: agentSpaceID, Slug: slug})
			}
		}
	}

	// 整轮耗时：从进入实现到回答落库，含所有工具轮次与思考时间。
	// 用应用侧时钟而不是 run.started_at，避免与应用、数据库的时钟差导致耗时失真。
	startedAt := time.Now()

	// 审批待办**不**在 run 结束时就地作废：本轮停下来等人，待办必须活过这一轮。
	// 只有显式停止（CancelSessionRun）与续跑接替才清理它们。
	approver = &runApprover{
		hub:         s.approvals,
		runID:       run.ID,
		sessionID:   sessionID,
		emit:        emitEvent,
		parentRunID: seed.PrevRunID,
	}

	res, runErr := s.agent.Run(runCtx, agent.RunInput{
		Env: tool.Env{
			UserID:    userID,
			SpaceID:   sess.SpaceID,
			SessionID: sessionID,
			RunID:     run.ID,
			SpacePerm: sp.Perm,
			// 企业 wiki 的宿主：固定系统空间，与当前会话所在空间无关。
			// 未初始化时为空，wiki 工具会报「不可用」而不是写进当前空间。
			WikiSpaceID: s.wikiSpaceID(ctx),
			// wiki 再按智能体分区：每个智能体只读写自己的 files/wiki/<slug>/。
			// 会话没固化智能体（历史数据）时落到 base，避免这些会话忽然没有知识目录。
			AgentID: wikiAgentID(sess.AgentID),
			Store:   s.store,
			Storage: s.storage,
			Log:     s.log,
			// 插件目录：当前空间 + 系统空间 + 已安装插件（未安装的发布件按需检索，不做同名覆盖）。
			// published/... 路径也由空间服务转给插件层解析。
			// extraRefs 只补「发起方智能体及其 requires」，不含来源空间的其它插件。
			Plugins: pluginLoader{
				spaces:         s.spaces,
				spaceID:        sess.SpaceID,
				extraRefs:      extraRefs,
				agentInternals: agentInternals,
			},
			// 工具侧借用：插件内容来自发起方空间，执行仍落在本会话空间。
			ExtraPluginRefs: extraRefs,
			// 搜索：provider 与密钥都在装配层，工具只拿到注册表
			Search: s.search,
			// 命令执行：沙箱后端在装配层选定（bwrap / local），
			// 工具只拿到 Runner，无法自行降级到无隔离执行。
			Shell: s.shell,
			// 插件试跑：写完草稿直接在会话里验，不走「发布-安装-发现错」的长链路
			PluginTry: s.pluginTry,
			// 任务派发：主 Agent 把需要他人确认的事丢出去，自己继续干活（§15.12.4）
			Tasks: s,
			// 审批：命中出网写 / 需确认命令时登记待办并把本轮停下，
			// 用户点完按钮由决策驱动的续跑接着走
			Approvals: approver,
			// 续跑时把上一轮已经做完的结果带上：工具执行前先查它
			Replay: seed.Replay,
			// 凭证：只给「配过没有 + 指纹」，工具拿不到明文
			Secrets:         s.secrets,
			CanWriteSecrets: s.canWriteSecrets(user),
		},
		History: seed.History,
		Query:   seed.Query,
		// 顶层会话允许派生子 Agent；子 Agent 的深度与预算由实现自己管
		AllowDispatch: true,
		ActiveSkill:   seed.ActiveSkill,
		Dir:           sess.Dir,
		DirPlugin:     s.dirPluginContext(ctx, sess.SpaceID, sess.Dir),
		AgentPrompt:   seed.AgentPrompt,
		AgentSkills:   agentSkills,
		// 平台事实进系统提示词，不做成工具：租户、IM 这类信息必须恒真
		Platform: s.platformFacts(user, sp),
		// 本轮「现在」：优先用户端上报的本地时间与时区，取不到才用服务器时间。
		// 每次 run 都重新解析，注入的是这一轮的时刻，不是会话创建时的时刻。
		Clock:  resolveClock(seed.ClientClock, time.Now),
		Resume: seed.Resume,
		// 每轮工具执行完把「走到哪了」落库：中断后从这里接着走，
		// 而不是让模型凭半截正文重新规划、把工具再执行一遍
		Checkpoint: func(state agent.ResumeState) error {
			data, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if len(data) > maxResumePayload {
				s.log.Warn("断点快照过大，放弃续跑能力",
					"run_id", run.ID, "bytes", len(data))
				return nil
			}
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return s.store.SaveRunResume(saveCtx, run.ID, data)
		},
	}, emitEvent)

	// 落库不跟着请求走：run 已经跑完，客户端在不在都要把结果写进去
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelSave()

	// 中断原因：用户显式取消、长时间没有进展、总上限到点，以及实现自己的预算。
	// 少了这一步，被掐断的 run 会被记成 succeeded，事后完全看不出是截断的。
	reason := live.cancelledReason()
	switch {
	case reason != "":
		// 已经在 live 上记过，不再覆盖
	case errors.Is(runErr, agent.ErrPaused):
		// 停在审批上：这一轮的终态就是「等人」，断点留着那条没执行的调用
		reason = ReasonAwaitingApproval
	case errors.Is(runErr, agent.ErrStalled):
		// 循环守卫判定停滞：已经强制收尾并给出结论，重试同样不会更好
		reason = ReasonStalled
	case errors.Is(runErr, agent.ErrBudgetExceeded):
		// 实现自己声明的预算用尽（工具次数、累计 token、轮次上限）
		reason = ReasonBudgetExceeded
	case runCtx.Err() != nil:
		reason = ReasonWallClockExceeded
	}

	status := store.RunSucceeded
	switch {
	case reason == ReasonAwaitingApproval:
		// 复用 waiting_approval 这个状态，但它是终态（ended_at 由 FinishRun 写）：
		// active-run 与清扫都按 ended_at 判活，暂停中的 run 不会被当成还在跑
		status = store.RunWaitingApproval
	case reason != "":
		status = store.RunCancelled
	case runErr != nil:
		status = store.RunFailed
	}
	if reason != "" {
		s.log.Warn("对话被中断",
			"session_id", sessionID, "run_id", run.ID, "reason", reason,
			"chars", utf8.RuneCountInString(res.Content), "tools", len(res.Steps))
	}
	out.Reason = reason

	finish := func(errMsg string) {
		metrics := map[string]any{
			"in":  res.Usage.InputTokens,
			"out": res.Usage.OutputTokens,
		}
		// 档位跟着 run 一起落库：排查「这单为什么没派子 agent」时看 usage.mode，
		// 比回去翻当时的提示词快。只记录事实，不参与任何判定。
		if res.Mode != "" {
			metrics["mode"] = res.Mode
		}
		usage, err := json.Marshal(metrics)
		if err != nil {
			usage = []byte(`{}`)
		}
		if err := s.store.FinishRun(saveCtx, store.Run{
			ID: run.ID, Status: status, Usage: usage,
			Provider: res.Provider, Model: res.Model, Error: errMsg,
		}); err != nil {
			s.log.Warn("更新 run 状态失败", "run_id", run.ID, "err", err)
		}
		s.settleResume(saveCtx, run.ID, status, reason)
		// 用户主动停止就是放弃这一轮：待办一起收掉。留着的话卡片还在界面上，
		// 点它会把会话重新跑起来——而停止的意思正是「不要了」。
		if reason == ReasonUserCancelled {
			s.expireRunApprovals(run.ID)
		}
	}

	// 流式刷新点上的正文：被中断时 Result 里可能还没收尾，
	// 用缓冲里累积的内容兜底，别把已经说出口的话抹掉。
	streamedText, streamedReasoning := live.texts()
	content, reasoning := res.Content, res.Reasoning
	if content == "" {
		content = streamedText
	}
	if reasoning == "" {
		reasoning = streamedReasoning
	}

	if content == "" && len(res.Steps) == 0 && reason != ReasonAwaitingApproval {
		// 没有内容可存：取消不改判为失败，原因如实写进 run.error
		if reason == "" {
			status = store.RunFailed
		}
		// 状态要跟着带出去：思考阶段就被叫停时这里是唯一出口，不写的话 handler 会按
		// 「失败」广播 run.finished（reason 为空），前端弹一个本不该有的错误
		out.Status = status
		err := runErr
		if err == nil {
			err = errors.New("模型没有返回内容，请重试")
		}
		if reason != "" {
			finish(cancelDetail(reason, runErr))
		} else {
			finish(err.Error())
		}
		return out, err
	}

	parts := marshalParts(s.log, sessionID, res)

	// 这一轮的用量与耗时随消息落库：前端刷新后仍能看到 token 与总耗时
	stats := store.MessageStats{
		InTokens:   res.Usage.InputTokens,
		OutTokens:  res.Usage.OutputTokens,
		DurationMS: int(time.Since(startedAt).Milliseconds()),
	}
	// 续跑接上暂停那一轮的消息：一轮只有一条回答，暂停只是把它切成两段。
	// 另起一条会把前半段留在「已中断」里，用户点完同意就只剩后半段。
	content, reasoning, parts, stats = mergeCarried(seed.Carry, content, reasoning, parts, stats)

	var msg store.Message
	var err error
	progressMu.Lock()
	placeID := placeholderID
	progressMu.Unlock()
	if placeID == "" && seed.Carry != nil {
		placeID = seed.Carry.ID
	}
	if placeID != "" {
		// 占位消息已经存在：整段覆盖，并把 streaming 置回 false
		msg, err = s.store.FinishMessage(saveCtx, placeID,
			content, reasoning, parts, stats, reason != "")
		if err != nil && seed.Carry != nil && seed.Carry.ID == placeID {
			// 接续的那条被别人收掉了（例如清扫把空占位删了）：不能因此判这一轮失败，
			// 退回新建，正文与轨迹都在 parts/content 里，不会丢。
			s.log.Warn("接续消息已不存在，改为新建",
				"session_id", sessionID, "message_id", placeID, "err", err)
			msg, err = s.store.CreateAssistantMessage(saveCtx, sessionID, content, reasoning, parts, stats)
		}
	} else {
		msg, err = s.store.CreateAssistantMessage(saveCtx, sessionID, content, reasoning, parts, stats)
	}
	if err != nil {
		status = store.RunFailed
		finish(err.Error())
		return out, err
	}
	appendStep("message", map[string]any{
		"message_id":      msg.ID,
		"chars":           utf8.RuneCountInString(content),
		"reasoning_chars": utf8.RuneCountInString(reasoning),
		"tools":           len(res.Steps),
		"need_approval":   res.NeedApproval,
		// 循环是怎么收尾的：done / budget / stalled。
		// 与 reason 分开记——「答案被逼出来但可用」和「运行被打断」是两件事。
		"stop_reason": res.StopReason,
	})
	// cancelled 也写原因，便于事后区分「用户停止」与「服务端到点」
	finish(cancelDetail(reason, runErr))

	if err := s.store.TouchSession(saveCtx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	sess.LastMessageAt = &msg.CreatedAt
	if latest, err := s.store.GetSession(saveCtx, sessionID); err == nil {
		sess = latest
	}

	// 向量索引异步执行，失败不影响本次对话
	if s.memory.EmbeddingEnabled() {
		go func() {
			indexCtx, indexCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer indexCancel()
			batch := []store.Message{msg}
			if userMsg.ID != "" {
				batch = append([]store.Message{userMsg}, msg)
			}
			if err := s.memory.Index(indexCtx, sessionID, batch); err != nil {
				s.log.Warn("写入消息向量失败", "session_id", sessionID, "err", err)
			}
		}()
	}

	// 回执不再由平台自动回填：子会话的 Agent 要在用户确认结论后调 task_reply，
	// 否则「我先确认一下」这种中间态也会被当成结果（§15.12.4）。

	s.log.Info("对话完成",
		"session_id", sessionID, "space_id", sess.SpaceID, "by", userID, "run_id", run.ID,
		"status", status, "provider", res.Provider, "model", res.Model,
		"chars", utf8.RuneCountInString(res.Content), "tools", len(res.Steps))

	out.Message = msg
	out.Session = sess
	out.Usage = res.Usage
	out.Status = status
	if reason == ReasonAwaitingApproval {
		// 暂停不是错误：调用方不该按失败收尾，也不该弹错误提示
		return out, nil
	}
	return out, runErr
}

// timelinePart 是 message.parts 里思考 / 正文段的形状，与前端 MessagePart 对齐。
type timelinePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// planPart 是 message.parts 里的任务清单，与前端 MessagePart 对齐。
//
// 只留这一轮结束时的最终清单：清单是状态不是步骤，中途每一次覆盖都存进来
// 会把一条消息撑成十几份同名卡片，翻历史时反而看不出到底做完了什么。
type planPart struct {
	Type  string           `json:"type"`
	Goal  string           `json:"goal,omitempty"`
	Items []agent.PlanItem `json:"items"`
}

// pausedToolStatus 是「停在审批上、还没执行」的工具卡状态，与前端 ToolCard 的
// 标签表对齐（待审批）。它只是这一轮的证据，可点的卡片由待办列表单独渲染。
const pausedToolStatus = "approval"

// mergeCarried 把续跑这一轮的产出并回暂停前的那条消息。
//
// 一轮只有一条回答：暂停把同一轮切成两段，续跑要接着写，而不是另起一条。合并
// 只做「接着写」——正文与思考按段落接在后面，用量相加；工具轨迹按顺序追加，
// 暂停时留下的待审批卡被同一次调用的真实回执顶掉，免得界面上出现两张。
func mergeCarried(carry *store.Message, content, reasoning string, parts []byte, stats store.MessageStats) (string, string, []byte, store.MessageStats) {
	if carry == nil {
		return content, reasoning, parts, stats
	}
	content = joinParagraph(carry.Content, content)
	reasoning = joinParagraph(carry.Reasoning, reasoning)
	stats.InTokens += carry.Stats.InTokens
	stats.OutTokens += carry.Stats.OutTokens
	stats.DurationMS += carry.Stats.DurationMS
	return content, reasoning, mergeParts(carry.Parts, parts), stats
}

// joinParagraph 接两段正文：空的那边直接让位，都不空才补一个空行分段。
func joinParagraph(head, tail string) string {
	head, tail = strings.TrimSpace(head), strings.TrimSpace(tail)
	switch {
	case head == "":
		return tail
	case tail == "":
		return head
	default:
		return head + "\n\n" + tail
	}
}

// mergeParts 按顺序拼接两次执行的 parts。
//
// 暂停时留下的待审批卡（status=approval）在同名同参数的调用拿到真实回执后就让位，
// 否则同一步会既显示「待审批」又显示「成功」。
func mergeParts(head, tail []byte) []byte {
	var prev, next []map[string]any
	if len(head) > 0 {
		_ = json.Unmarshal(head, &prev)
	}
	if len(tail) > 0 {
		_ = json.Unmarshal(tail, &next)
	}
	if len(prev) == 0 {
		return tail
	}
	if len(next) == 0 {
		return head
	}
	carried := make(map[string]bool, len(next))
	for _, p := range next {
		if key := partKey(p); key != "" {
			carried[key] = true
		}
	}
	out := make([]map[string]any, 0, len(prev)+len(next))
	for _, p := range prev {
		if p["status"] == pausedToolStatus && carried[partKey(p)] {
			continue
		}
		out = append(out, p)
	}
	out = append(out, next...)
	data, err := json.Marshal(out)
	if err != nil {
		return tail
	}
	return data
}

// partKey 是「同一个工具调用」的判据：续跑时模型会重新生成 call id，
// 只有工具名加参数能证明这是刚才那一步。
func partKey(p map[string]any) string {
	name, _ := p["name"].(string)
	if name == "" {
		return ""
	}
	args, _ := p["args"].(string)
	return name + "\x00" + args
}

// marshalParts 把这一轮的时间线落成 message.parts。
//
// parts 是数组，按发生顺序排列：思考段与正文段带文本，工具段直接复用 Steps 的
// JSON 形状（参数与回执不重复写）。历史消息据此还原「想一段 → 调工具 → 再说一句」
// 的真实顺序，排障时才看得出结论是在哪一步之后得出的。
//
// 没有工具调用时正文位置是固定的（思考在前、正文在后），分组渲染已经够用，
// 不再把整段正文复制一份进 parts。
func marshalParts(log *slog.Logger, sessionID string, res agent.Result) []byte {
	if len(res.Steps) == 0 && len(res.Parked) == 0 && res.Plan.Empty() {
		return []byte("[]")
	}
	steps := make(map[string]agent.Step, len(res.Steps))
	for _, st := range res.Steps {
		steps[st.ID] = st
	}
	parts := make([]any, 0, len(res.Timeline)+len(res.Steps)+len(res.Parked)+1)
	// 清单排在最前：它是这一轮的执行计划，先看计划再看过程才读得懂后面的取舍。
	// 中途的每次覆盖不进 parts，只留最终状态（plan.updated 事件负责直播时的逐次刷新）。
	if !res.Plan.Empty() {
		parts = append(parts, planPart{Type: "plan", Goal: res.Plan.Goal, Items: res.Plan.Items})
	}
	placed := make(map[string]bool, len(res.Steps))
	for _, seg := range res.Timeline {
		if seg.Type == "tool" {
			st, ok := steps[seg.ToolID]
			if !ok {
				continue
			}
			placed[seg.ToolID] = true
			parts = append(parts, st)
			continue
		}
		if strings.TrimSpace(seg.Text) == "" {
			continue
		}
		parts = append(parts, timelinePart{Type: seg.Type, Text: seg.Text})
	}
	// 时间线缺失或不完整（第三方实现、续跑补齐）时工具轨迹不能丢
	for _, st := range res.Steps {
		if !placed[st.ID] {
			parts = append(parts, st)
		}
	}
	// 停在审批上的调用也要留在这一轮里：它们没有回执，状态如实标成待审批。
	// 不落的话「第一个动作就要确认」的那一轮就是一条空产出，界面看不见。
	for _, p := range res.Parked {
		if placed[p.ID] {
			continue
		}
		placed[p.ID] = true
		parts = append(parts, agent.Step{
			Type: "tool", ID: p.ID, Name: p.Name, Args: p.Args,
			Status: pausedToolStatus,
		})
	}
	data, err := json.Marshal(parts)
	if err != nil {
		log.Warn("序列化消息轨迹失败", "session_id", sessionID, "err", err)
		return []byte("[]")
	}
	return data
}

// keepResume 判断这一轮的断点值不值得留。
//
// 只留「系统侧中断」：卡死（idle）、到点（总时长）、以及非预算类失败。用户主动停止
// 是明确的意思表示；预算用尽与停滞再走一步也是同样的结局——这两种情况留着断点，
// 「继续」就会变成一个点了就再失败一次的按钮。
func keepResume(status, reason string) bool {
	if status == store.RunFailed {
		return true
	}
	// 停在审批上的断点是「恢复的唯一凭据」：待办还在，断点没了就只剩一张点不动的卡。
	return reason == ReasonIdleTimeout || reason == ReasonWallClockExceeded || reason == ReasonAwaitingApproval
}

// settleResume 按 keepResume 的结论落库。
func (s *Service) settleResume(ctx context.Context, runID, status, reason string) {
	if keepResume(status, reason) {
		return
	}
	if err := s.store.DeleteRunResume(ctx, runID); err != nil {
		s.log.Warn("清理断点失败", "run_id", runID, "err", err)
	}
}

// Resumable 报告这个会话有没有可以接着走的执行。
//
// 判据是「有没有断点」而不是「上一轮是不是失败」：进程崩溃时连失败都没来得及写，
// 但那正是最需要继续的时候。
func (s *Service) Resumable(ctx context.Context, userID, sessionID string) (store.Run, bool, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return store.Run{}, false, err
	}
	if s.agent == nil || !s.agent.Capabilities().Resume {
		return store.Run{}, false, nil
	}
	run, ok, err := s.store.ResumableRun(ctx, sessionID)
	if err != nil || !ok {
		return store.Run{}, false, err
	}
	// 还在本进程里跑的那一轮不需要「继续」，点它只会同时跑两份
	if s.runs.get(run.ID) != nil {
		return store.Run{}, false, nil
	}
	// 这一轮不在本进程：顺手把它的占位消息结算掉（心跳还新时 AbandonRun 自己会跳过）。
	// 用户回到会话时看到的就是「问题 + 中断前的工具轨迹」，而不是只剩一条问题。
	s.abandonRun(ctx, run)
	return run, true, nil
}

// Resume 从断点接着跑上一次执行。
//
// 语义是「接着走」而不是「重新问一遍」：断点里是完整消息序列（含工具回执），
// 已经执行过的工具不会再跑一遍，预算计数也一起继承。
func (s *Service) Resume(
	ctx context.Context,
	userID, sessionID string,
	emit func(agent.Event, int) error,
) (ChatOutcome, error) {
	out := ChatOutcome{}
	if s.agent == nil || !s.agent.Capabilities().Resume {
		return out, ErrResumeUnsupported
	}
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return out, err
	}

	prev, ok, err := s.store.ResumableRun(ctx, sessionID)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrNoResume
	}
	// 同一轮不能跑两份：会写重复文件、发重复请求，还争同一条会话的上下文。
	//
	// 判据是心跳，不只是本进程的登记表——多实例部署时，原先那个执行体
	// 可能正在别的副本上跑（比如它只是连不上前端了）。
	if s.runAlive(ctx, sessionID, prev.ID) {
		return out, ErrResumeRunning
	}
	payload, found, err := s.store.GetRunResume(ctx, prev.ID)
	if err != nil {
		return out, err
	}
	var state agent.ResumeState
	if !found || json.Unmarshal(payload, &state) != nil || len(state.Messages) == 0 {
		return out, ErrNoResume
	}

	// 断点用掉就删：重复点「继续」不该把同一段执行跑两遍。
	// 新的 run 会自己写自己的断点，失败时仍然可以继续。
	if err := s.store.DeleteRunResume(ctx, prev.ID); err != nil {
		s.log.Warn("清理旧断点失败", "run_id", prev.ID, "err", err)
	}

	// 上一轮已经做完的工具结果：续跑时按「工具 + 参数」回放，副作用不重来。
	// 读不到不算致命——最坏情况是多执行一次，比整轮跑不起来强。
	replay, err := s.buildReplay(ctx, prev.ID)
	if err != nil {
		s.log.Warn("读取上一轮工具结果失败，续跑可能重复执行", "run_id", prev.ID, "err", err)
	}
	// 旧 run 的待办作废：这一轮遇到同样的动作会重新登记，
	// 结论按参数从旧 run 复用，用户不会被问第二遍。
	s.expireRunApprovals(prev.ID)

	// 上一轮还挂着 running / waiting_approval（进程崩溃留下的），由这次续跑接替它收尾：
	// 不置终态的话 active-run 会一直说「在跑」，前端永远等不到结果。
	if err := s.store.FinishRun(ctx, store.Run{
		ID: prev.ID, Status: store.RunFailed,
		Error: "执行已中断，由续跑接替",
	}); err != nil {
		s.log.Warn("收尾上一轮执行失败", "run_id", prev.ID, "err", err)
	}
	// 占位消息也要一起结算：用 run_step 还原中断前的工具轨迹再标中断。
	// 不结算的话它会永远挂在 streaming 上，用户点完「继续」也只看得到结论，
	// 看不到中断前做过什么（历史记录从列表里消失的正是这一段）。
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	// 上一轮的正文在缓存里（最多丢最后 1s），和工具轨迹一起写回同一条消息
	if err := s.store.InterruptStreamingMessage(settleCtx, prev.ID, sessionID, s.recoverContent(prev.ID)); err != nil {
		s.log.Warn("结算中断轮占位消息失败", "run_id", prev.ID, "err", err)
	}
	// 暂停那一轮已经写下的消息：续跑接着它写，一轮只有一条回答。
	// 结算之后再读，拿到的是补齐了正文与工具轨迹的最终形态。
	var carry *store.Message
	if prevMsg, ok, err := s.store.MessageOfRun(settleCtx, prev.ID); err != nil {
		s.log.Warn("读取续跑接续消息失败", "run_id", prev.ID, "err", err)
	} else if ok {
		carry = &prevMsg
	}
	cancelSettle()

	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		s.log.Warn("读取用户资料失败", "user_id", userID, "err", err)
	}
	if err := s.store.TouchSession(ctx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	run, err := s.createRun(ctx, sessionID)
	if err != nil {
		return out, err
	}
	out.RunID = run.ID
	s.log.Info("继续上一次执行",
		"session_id", sessionID, "run_id", run.ID, "from", prev.ID,
		"round", state.Round, "tools", state.ToolCalls)

	return s.execute(ctx, sess, sp, user, store.Message{}, run, runSeed{
		Query:     state.Query,
		Resume:    &state,
		PrevRunID: prev.ID,
		Replay:    replay,
		Carry:     carry,
	}, out, emit)
}

// buildReplay 把上一轮已经产生结果的工具调用整理成回放表。
//
// key 是「工具 + 规范化参数」而不是 call_id：续跑时模型重新生成的调用 id 是新的，
// 只有参数能证明「这就是刚才那一次」。
func (s *Service) buildReplay(ctx context.Context, runID string) (map[string]tool.InvokeResult, error) {
	records, err := s.store.ListToolResults(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]tool.InvokeResult, len(records))
	for _, r := range records {
		out[tool.ArgsHash(r.ToolName, r.Args)] = tool.InvokeResult{
			ToolName: r.ToolName,
			Status:   r.Status,
			Content:  r.Result,
			Risk:     r.RiskLevel,
			Cached:   true,
		}
	}
	return out, nil
}

// SweepStaleRuns 收尾上一个进程留下的执行。
//
// 只在启动时调用一次：心跳停了的 run 不会再有人来写终态，留着它们只会让
// 前端一直显示「生成中」。断点保留，用户打开会话就能继续。
func (s *Service) SweepStaleRuns(ctx context.Context) (int, error) {
	threshold := s.idleTimeout
	if threshold <= 0 {
		threshold = defaultIdleTimeout
	}
	n, err := s.store.SweepStaleRuns(ctx, threshold, "服务已重启，这一轮执行被中断", func(runID, _ string) store.RunContent {
		return s.recoverContent(runID)
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Info("已收尾上次未完成的执行", "runs", n)
	}
	// 断点有保质期：一周前的快照接上去，模型看到的是一个早就变了的文件树，
	// 「继续」只会得到一份基于过期世界的结论。
	if purged, err := s.store.PurgeStaleResumes(ctx, resumeTTL); err != nil {
		s.log.Warn("清理过期断点失败", "err", err)
	} else if purged > 0 {
		s.log.Info("已清理过期断点", "resumes", purged)
	}
	return n, nil
}
