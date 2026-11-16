package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// ErrApprovalGone 表示审批请求已经失效（超时、已被处理，且没有可续跑的断点）。
var ErrApprovalGone = errors.New("审批请求已失效，可能已超时")

// ErrApprovalResumed 表示决策已落库，但当时没有执行体在等：
// 这一轮已经中断，服务正在从断点续跑。对调用方来说不是失败。
var ErrApprovalResumed = errors.New("执行已中断，正在从断点继续")

// approvalPollInterval 是等待期间的兜底轮询周期。
//
// 决策可能落在别的副本上（多实例），那边没有本进程的 channel 可唤醒；
// 库是唯一真相，等一会儿再看一眼就够了。等人本来就不差这两秒。
const approvalPollInterval = 2 * time.Second

// approvalHub 是进程内的唤醒通道。
//
// 审批的**事实**在 store（approval 表）：谁在等、结论是什么、什么时候失效。
// hub 只承担一件 store 做不到的事——把 HTTP 请求线程的决策尽快交给阻塞中的执行体。
// 没有 channel 也能工作（靠轮询），有 channel 只是不必等那两秒。
type approvalHub struct {
	mu      sync.Mutex
	pending map[string]chan tool.ApprovalDecision
	store   store.Store
	log     *slog.Logger
}

func newApprovalHub(st store.Store, log *slog.Logger) *approvalHub {
	if log == nil {
		log = slog.Default()
	}
	return &approvalHub{
		pending: map[string]chan tool.ApprovalDecision{},
		store:   st,
		log:     log,
	}
}

func (h *approvalHub) register(id string, ch chan tool.ApprovalDecision) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending[id] = ch
}

func (h *approvalHub) unregister(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pending, id)
}

// resolve 把决策交给等待中的执行体；没有等待者说明已经超时或进程换了。
func (h *approvalHub) resolve(id string, d tool.ApprovalDecision) bool {
	h.mu.Lock()
	ch, ok := h.pending[id]
	if ok {
		delete(h.pending, id)
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	// 带缓冲，保证 resolve 不会因为执行体刚好退出而阻塞 HTTP 处理
	ch <- d
	return true
}

// runApprover 绑定一条 run 的审批视图：事件往哪推、记录挂在哪个 run 上。
type runApprover struct {
	hub       *approvalHub
	runID     string
	sessionID string
	emit      func(agent.Event) error
	// parentRunID 是这一轮接替的上一次执行：续跑时审批记录挂在旧 run 上，
	// 不沿这条链找，「刚才批过的命令」会被重新问一遍。
	parentRunID string
	// beat 在等待期间维持心跳：等人点击不是「卡住」，不能触发 idle 看门狗。
	beat func()
}

// Decide 实现 tool.Approver：命中可直接复用的结论就返回，否则落 pending、推事件并阻塞。
func (a *runApprover) Decide(ctx context.Context, req tool.ApprovalRequest) (tool.ApprovalDecision, error) {
	if a.hub.store == nil {
		return tool.ApprovalDecision{}, errors.New("审批存储未配置")
	}
	// 已经做过同样的决定（本轮同类动作，或同一次调用的重放）：不必再问一遍
	if d, ok := a.reuse(ctx, req); ok {
		return d, nil
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = tool.DefaultApprovalTimeout
	}
	rec, err := a.hub.store.CreateApproval(ctx, store.Approval{
		ID:        req.ID,
		SessionID: a.sessionID,
		RunID:     a.runID,
		CallID:    req.CallID,
		Tool:      req.Tool,
		Kind:      req.Kind,
		ActionKey: req.Key,
		ArgsHash:  req.ArgsHash,
		Detail:    req.Detail,
		Reason:    req.Reason,
		Risk:      req.Risk,
		Status:    store.ApprovalPending,
		ExpireAt:  time.Now().Add(timeout),
	})
	if err != nil {
		return tool.ApprovalDecision{}, fmt.Errorf("登记审批失败：%w", err)
	}
	// 同 id 已存在（同一 call 重放）且已经是终态：照它的结论走
	if rec.Status != store.ApprovalPending {
		return decisionFrom(rec), nil
	}

	// 进入等待：前端能看出「这一轮卡在等人」，清扫也不会把它当成崩溃收掉
	a.setStatus(ctx, store.RunWaitingApproval)
	defer a.setStatus(ctx, store.RunRunning)

	ch := make(chan tool.ApprovalDecision, 1)
	a.hub.register(req.ID, ch)
	defer a.hub.unregister(req.ID)

	if a.emit != nil {
		if err := a.emit(agent.Event{Type: agent.EventApprovalReq, Approval: &agent.ApprovalState{
			ID:       req.ID,
			CallID:   req.CallID,
			Tool:     req.Tool,
			Kind:     req.Kind,
			Detail:   req.Detail,
			Reason:   req.Reason,
			Risk:     req.Risk,
			TimeoutS: int(timeout.Seconds()),
		}}); err != nil {
			// 事件推不出去，记录不能留在 pending：没有人会看到这条待办
			a.expire(req.ID)
			return tool.ApprovalDecision{}, err
		}
	}

	// 等用户点击期间定期续心跳：idle 看门狗按「没有事件」判卡住，
	// 而我们等的正是人。
	done := make(chan struct{})
	defer close(done)
	if a.beat != nil {
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					a.beat()
				case <-done:
					return
				}
			}
		}()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	poll := time.NewTicker(approvalPollInterval)
	defer poll.Stop()

	for {
		select {
		case d := <-ch:
			a.hub.log.Info("审批已处理",
				"run_id", a.runID, "tool", req.Tool, "kind", req.Kind,
				"allow", d.Allow, "remember", d.Remember)
			return d, nil
		case <-poll.C:
			// 兜底：决策可能落在别的副本上，那边唤不醒这里
			cur, ok, err := a.hub.store.GetApproval(ctx, req.ID)
			if err != nil {
				a.hub.log.Warn("读取审批状态失败", "approval_id", req.ID, "err", err)
				continue
			}
			if ok && cur.Status != store.ApprovalPending {
				return decisionFrom(cur), nil
			}
		case <-timer.C:
			a.expire(req.ID)
			// 决策可能刚好在超时后落库：以库里为准，避免「已批却被判超时」
			if cur, ok, err := a.hub.store.GetApproval(context.WithoutCancel(ctx), req.ID); err == nil && ok &&
				cur.Status != store.ApprovalPending && cur.Status != store.ApprovalExpired {
				return decisionFrom(cur), nil
			}
			a.hub.log.Info("审批等待超时", "run_id", a.runID, "tool", req.Tool, "kind", req.Kind)
			return tool.ApprovalDecision{}, tool.ErrApprovalTimeout
		case <-ctx.Done():
			a.expire(req.ID)
			return tool.ApprovalDecision{}, ctx.Err()
		}
	}
}

// reuse 查已经做过、可以直接沿用的结论。
//
// 先看当前 run，再看被它接替的那一轮：续跑等于换了个 run 接着走，
// 审批记录还在旧 run 上。
func (a *runApprover) reuse(ctx context.Context, req tool.ApprovalRequest) (tool.ApprovalDecision, bool) {
	for _, runID := range []string{a.runID, a.parentRunID} {
		if runID == "" {
			continue
		}
		grant, ok, err := a.hub.store.FindApprovalGrant(ctx, runID, req.Key, req.ArgsHash)
		if err != nil {
			a.hub.log.Warn("读取审批结论失败", "run_id", runID, "err", err)
			continue
		}
		if ok {
			return decisionFrom(grant), true
		}
	}
	return tool.ApprovalDecision{}, false
}

// setStatus 把这一轮在 running / waiting_approval 之间切换。
//
// 用 WithoutCancel：等审批时 run 被取消，状态也要落成 running，
// 否则终态写不进去、清扫又不敢动它。
func (a *runApprover) setStatus(ctx context.Context, status string) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := a.hub.store.SetRunStatus(c, a.runID, status); err != nil {
		a.hub.log.Warn("切换执行状态失败", "run_id", a.runID, "status", status, "err", err)
	}
}

// expire 把记录置为 expired；失败只记日志，不覆盖原始返回。
func (a *runApprover) expire(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, ok, err := a.hub.store.DecideApproval(ctx, id, store.ApprovalExpired, "", ""); err != nil {
		a.hub.log.Warn("标记审批超时失败", "approval_id", id, "err", err)
	} else if ok {
		a.hub.log.Info("审批已超时", "approval_id", id)
	}
}

// decisionFrom 把库里的记录翻译成执行体要的结论。
func decisionFrom(a store.Approval) tool.ApprovalDecision {
	d := tool.ApprovalDecision{
		Allow:    a.Status == store.ApprovalApproved,
		Remember: a.Scope == "turn",
	}
	if a.Status == store.ApprovalDenied {
		d.Reason = "用户拒绝了这次调用"
	}
	return d
}

// ResolveApproval 收下前端的一次审批决策。
//
// scope 只有两种：once（只这一次）与 turn（本轮内同类动作都照办）。
// 「本轮」指当前这次 run，run 结束即失效——下一轮必须重新确认。
//
// 决策落库是唯一的必要条件：本进程有等待者就直接唤醒；没有的话说明执行体
// 在别的副本上（它自己会轮询到）或者已经没了（那就从这里把这一轮续起来）。
func (s *Service) ResolveApproval(ctx context.Context, userID, sessionID, approvalID, decision, scope string) error {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return err
	}
	if decision != "approve" && decision != "deny" {
		return errors.New("decision 只能是 approve 或 deny")
	}
	if scope != "turn" {
		scope = "once"
	}
	status := store.ApprovalApproved
	if decision == "deny" {
		status = store.ApprovalDenied
	}
	rec, ok, err := s.store.DecideApproval(ctx, approvalID, status, scope, userID)
	if err != nil {
		return err
	}
	if !ok {
		// 重复点击：结论已经落过，按成功返回
		if cur, found, err := s.store.GetApproval(ctx, approvalID); err == nil && found && cur.Status == status {
			return nil
		}
		return ErrApprovalGone
	}

	s.audit(ctx, userID, "tool.approval", sessionID, map[string]any{
		"approval_id": approvalID,
		"decision":    decision,
		"scope":       scope,
	})

	if s.approvals.resolve(approvalID, decisionFrom(rec)) {
		return nil
	}
	// 本进程没有等待者。执行体还在别处活着的话，它会自己轮询到这条结论；
	// 已经不在了的话，光落库等于「批了却没人执行」。
	if s.runAlive(ctx, sessionID, rec.RunID) {
		return nil
	}
	return s.resumeAfterApproval(ctx, userID, sessionID)
}

// runAlive 判断这条 run 是否还有执行体在跑（本进程或别的副本）。
//
// 判据与 active-run 一致：心跳。心跳停了才是真的没人跑了。
func (s *Service) runAlive(ctx context.Context, sessionID, runID string) bool {
	if s.runs.get(runID) != nil {
		return true
	}
	run, ok, err := s.store.GetActiveRun(ctx, sessionID, s.idleTimeout)
	return err == nil && ok && run.ID == runID
}

// resumeAfterApproval 在「批准了一个已经没人等的待办」时把这一轮续起来。
//
// 异步执行：审批接口不该等一整轮跑完。同一会话只允许一个自动续跑，
// 连点两次批准不会起两份执行。
func (s *Service) resumeAfterApproval(ctx context.Context, userID, sessionID string) error {
	if _, ok, err := s.store.ResumableRun(ctx, sessionID); err != nil || !ok {
		return ErrApprovalGone
	}
	if _, loaded := s.resuming.LoadOrStore(sessionID, true); loaded {
		return ErrApprovalResumed
	}
	bg := context.WithoutCancel(ctx)
	go func() {
		defer s.resuming.Delete(sessionID)
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("审批后自动续跑崩溃", "session_id", sessionID, "panic", p)
			}
		}()
		if _, err := s.Resume(bg, userID, sessionID, nil); err != nil {
			s.log.Warn("审批后自动续跑失败", "session_id", sessionID, "err", err)
		}
	}()
	return ErrApprovalResumed
}

// ApprovalView 是一条待处理的确认，刷新会话时据此补回卡片。
type ApprovalView struct {
	ID       string `json:"approval_id"`
	CallID   string `json:"call_id"`
	Tool     string `json:"tool"`
	Kind     string `json:"kind"`
	Detail   string `json:"detail"`
	Reason   string `json:"reason"`
	Risk     int    `json:"risk"`
	TimeoutS int    `json:"timeout_s"`
}

// PendingApprovals 返回会话里所有还没失效的确认。
//
// 判据是「pending 且没过期」，不看 run 状态：进程崩过之后 run 已经被收尾，
// 但那条待办还在有效期内——它仍然可以被批准，批准会顺带把这一轮续起来。
func (s *Service) PendingApprovals(ctx context.Context, userID, sessionID string) ([]ApprovalView, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return nil, err
	}
	list, err := s.store.ListPendingApprovals(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalView, 0, len(list))
	for _, a := range list {
		left := int(time.Until(a.ExpireAt).Seconds())
		if left < 0 {
			left = 0
		}
		out = append(out, ApprovalView{
			ID:       a.ID,
			CallID:   a.CallID,
			Tool:     a.Tool,
			Kind:     a.Kind,
			Detail:   a.Detail,
			Reason:   a.Reason,
			Risk:     a.Risk,
			TimeoutS: left,
		})
	}
	return out, nil
}

// expireRunApprovals 收尾一条 run 留下的待办：run 结束，没有人再等结果。
func (s *Service) expireRunApprovals(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.ExpireRunApprovals(ctx, runID); err != nil {
		s.log.Warn("清理审批待办失败", "run_id", runID, "err", err)
	}
}

// ApprovalID 由 run 与 call 派生稳定 ID：前端点按钮时原样回传。
func ApprovalID(runID, callID string) string {
	if callID == "" {
		callID = "anonymous"
	}
	if runID == "" {
		return "ap_" + callID
	}
	return "ap_" + runID + "_" + callID
}
