package session

import (
	"context"
	"encoding/json"
	"errors"
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
	if w.idleT != nil {
		w.idleT.Stop()
	}
	if w.maxT != nil {
		w.maxT.Stop()
	}
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
	live := s.runs.add(run.ID, userID, cancelRun)
	defer func() {
		live.finish()
		s.runs.remove(run.ID, live)
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
	// 首个正文增量到达时占位一条 streaming 消息：刷新后列表能看出「这一轮真的在跑」，
	// 而不是空白。正文仍然在结束时一次性覆盖，不逐 token 写库（§15.12.2）。
	var placeholderID string
	ensurePlaceholder := func() {
		if placeholderID != "" {
			return
		}
		placeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		m, err := s.store.CreateStreamingMessage(placeCtx, sessionID)
		cancel()
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
		switch ev.Type {
		case agent.EventToolCall, agent.EventToolResult:
			appendStep(ev.Type, ev.Tool.Payload())
		case agent.EventApprovalReq:
			appendStep(ev.Type, ev.Approval)
		case agent.EventMessageDelta:
			// 思考不占位：只有真的开始写正文了，才值得在列表里留一条记录
			if !ev.Reasoning {
				ensurePlaceholder()
			}
		}
		offset := live.broadcast(ev)
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
	if sess.AgentID != "" && s.agentPlugins != nil {
		if seed.Resume == nil && seed.AgentPrompt == "" {
			if r, err := s.agentPlugins.ResolveAgentPlugin(ctx, agentSpaceID, sess.AgentID); err == nil {
				seed.AgentPrompt = r.Prompt
			} else {
				s.log.Warn("智能体设定加载失败",
					"session_id", sessionID, "agent_id", sess.AgentID, "agent_space", agentSpaceID, "err", err)
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
			Store:       s.store,
			Storage:     s.storage,
			Log:         s.log,
			// 插件目录：当前空间 + 系统空间 + 已安装插件（未安装的发布件按需检索，不做同名覆盖）。
			// published/... 路径也由空间服务转给插件层解析。
			// extraRefs 只补「发起方智能体及其 requires」，不含来源空间的其它插件。
			Plugins: pluginLoader{
				spaces:    s.spaces,
				spaceID:   sess.SpaceID,
				extraRefs: extraRefs,
			},
			// 工具侧借用：插件内容来自发起方空间，执行仍落在本会话空间。
			ExtraPluginRefs: extraRefs,
			// 搜索：provider 与密钥都在装配层，工具只拿到注册表
			Search: s.search,
			// 命令执行：沙箱后端在装配层选定（bwrap / sandbox-exec / local），
			// 工具只拿到 Runner，无法自行降级到无隔离执行。
			Shell: s.shell,
			// 插件试跑：写完草稿直接在会话里验，不走「发布-安装-发现错」的长链路
			PluginTry: s.pluginTry,
			// 任务派发：主 Agent 把需要他人确认的事丢出去，自己继续干活（§15.12.4）
			Tasks: s,
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
		// 平台事实进系统提示词，不做成工具：租户、IM 这类信息必须恒真
		Platform: s.platformFacts(user, sp),
		Resume:   seed.Resume,
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
		usage, err := json.Marshal(map[string]int{
			"in":  res.Usage.InputTokens,
			"out": res.Usage.OutputTokens,
		})
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
	}

	if res.Content == "" && len(res.Steps) == 0 {
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

	// res.Steps 为空时 json.Marshal 会给出 null，而 parts 的契约是数组，这里显式落 []
	parts := []byte("[]")
	if len(res.Steps) > 0 {
		data, marshalErr := json.Marshal(res.Steps)
		if marshalErr != nil {
			s.log.Warn("序列化执行轨迹失败", "session_id", sessionID, "err", marshalErr)
		} else {
			parts = data
		}
	}

	// 这一轮的用量与耗时随消息落库：前端刷新后仍能看到 token 与总耗时
	stats := store.MessageStats{
		InTokens:   res.Usage.InputTokens,
		OutTokens:  res.Usage.OutputTokens,
		DurationMS: int(time.Since(startedAt).Milliseconds()),
	}
	var msg store.Message
	var err error
	if placeholderID != "" {
		// 占位消息已经存在：整段覆盖，并把 streaming 置回 false
		msg, err = s.store.FinishMessage(saveCtx, placeholderID,
			res.Content, res.Reasoning, parts, stats, reason != "")
	} else {
		msg, err = s.store.CreateAssistantMessage(saveCtx, sessionID, res.Content, res.Reasoning, parts, stats)
	}
	if err != nil {
		status = store.RunFailed
		finish(err.Error())
		return out, err
	}
	appendStep("message", map[string]any{
		"message_id":      msg.ID,
		"chars":           utf8.RuneCountInString(res.Content),
		"reasoning_chars": utf8.RuneCountInString(res.Reasoning),
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
	return out, runErr
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
	return reason == ReasonIdleTimeout || reason == ReasonWallClockExceeded
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
	// 同一轮不能跑两份：会写重复文件、发重复请求，还争同一条会话的上下文
	if s.runs.get(prev.ID) != nil {
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

	// 上一轮如果还挂着 running（进程崩溃留下的），由这次续跑接替它收尾：
	// 不置终态的话 active-run 会一直说「在跑」，前端永远等不到结果。
	if s.runs.get(prev.ID) == nil {
		if err := s.store.FinishRun(ctx, store.Run{
			ID: prev.ID, Status: store.RunFailed,
			Error: "执行已中断，由续跑接替",
		}); err != nil {
			s.log.Warn("收尾上一轮执行失败", "run_id", prev.ID, "err", err)
		}
	}

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
		Query:  state.Query,
		Resume: &state,
	}, out, emit)
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
	n, err := s.store.SweepStaleRuns(ctx, threshold, "服务已重启，这一轮执行被中断")
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Info("已收尾上次未完成的执行", "runs", n)
	}
	return n, nil
}
