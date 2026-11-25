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
var ErrApprovalGone = errors.New("审批请求已失效：这一轮已被新一轮对话取代，或断点已过保留期")

// ErrApprovalResumed 表示决策已落库，但当时没有执行体在等：
// 这一轮已经中断，服务正在从断点续跑。对调用方来说不是失败。
var ErrApprovalResumed = errors.New("执行已中断，正在从断点继续")

// approvalTTL 是待办的有效期，与断点同寿命（resumeTTL）。
//
// 审批没有「等一会儿就按拒绝处理」：不点，这一轮就一直停在那里。
// 但待办不能比断点活得久——断点清掉了，批了也接不上；反过来，待办过期而断点
// 还在，用户点同意只会得到「已失效」。两者同步，用户什么时候回来都能接着走。
const approvalTTL = resumeTTL

// approvalHub 是审批记录的落点：结论、有效期、谁批的都在 store（approval 表）。
//
// 执行体不再阻塞等人，所以这里没有进程内唤醒通道：决策落库之后，
// 由 ResolveApproval 从断点把这一轮续起来，不存在「唤醒」这一步。
type approvalHub struct {
	store store.Store
	log   *slog.Logger
}

func newApprovalHub(st store.Store, log *slog.Logger) *approvalHub {
	if log == nil {
		log = slog.Default()
	}
	return &approvalHub{store: st, log: log}
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

	// parked 记下这一轮挂起的待办 id。
	//
	// 用途只有一个：决策与「落终态」撞车时兜底。用户可能在我刚登记完、还没把
	// run 收尾的几毫秒里点了按钮，那时 ResolveApproval 看到执行体还活着就不续跑；
	// 收尾之后按这张表回查一遍，已经批了的立刻续，不会停在「批了却没动」。
	mu     sync.Mutex
	parked []string
}

func (a *runApprover) markParked(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.parked = append(a.parked, id)
}

// parkedIDs 返回这一轮挂起过的待办 id。
func (a *runApprover) parkedIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.parked...)
}

// Decide 实现 tool.Approver：命中可直接复用的结论就返回，否则登记待办、推事件，
// 并回 ErrApprovalParked 让调用方把这一轮停下来。
//
// 不阻塞是有意的：等人可以是几分钟，也可以是几天。占着 goroutine、SSE 连接与整份
// 上下文去等，只为了省掉「续跑冷启动」的那一两秒，不划算；而且进程重启、换副本、
// 前端断线这些情况本来就要走断点恢复，两条路合成一条，只有一种状态机。
func (a *runApprover) Decide(ctx context.Context, req tool.ApprovalRequest) (tool.ApprovalDecision, error) {
	if a.hub.store == nil {
		return tool.ApprovalDecision{}, errors.New("审批存储未配置")
	}
	// 已经做过同样的决定（本轮同类动作，或同一次调用的重放）：不必再问一遍
	if d, ok := a.reuse(ctx, req); ok {
		return d, nil
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
		RiskLabel: req.RiskLabel,
		Status:    store.ApprovalPending,
		ExpireAt:  time.Now().Add(approvalTTL),
	})
	if err != nil {
		return tool.ApprovalDecision{}, fmt.Errorf("登记审批失败：%w", err)
	}
	// 同 id 已存在（同一 call 重放）且已经是终态：照它的结论走
	if rec.Status != store.ApprovalPending {
		return decisionFrom(rec), nil
	}

	if a.emit != nil {
		if err := a.emit(agent.Event{Type: agent.EventApprovalReq, Approval: &agent.ApprovalState{
			ID:        req.ID,
			CallID:    req.CallID,
			Tool:      req.Tool,
			Kind:      req.Kind,
			Detail:    req.Detail,
			Reason:    req.Reason,
			Risk:      req.Risk,
			RiskLabel: req.RiskLabel,
		}}); err != nil {
			// 事件推不出去，记录不能留在 pending：没有人会看到这条待办
			a.expire(req.ID)
			return tool.ApprovalDecision{}, err
		}
	}

	a.markParked(req.ID)
	a.hub.log.Info("登记待办，本轮暂停",
		"run_id", a.runID, "approval_id", req.ID, "tool", req.Tool, "kind", req.Kind)
	return tool.ApprovalDecision{}, tool.ErrApprovalParked
}

// Granted 实现 tool 包的「查结论」能力：只回答批过没有，不登记待办、不阻塞。
//
// 用途只有一个：续跑重放一条已经批过「联网重跑」的命令时，第一次执行就带网，
// 不再先按无网白跑一遍（那一遍失败，还可能把第二次要做的改动拆坏）。
func (a *runApprover) Granted(ctx context.Context, req tool.ApprovalRequest) (tool.ApprovalDecision, bool, error) {
	if a.hub.store == nil {
		return tool.ApprovalDecision{}, false, nil
	}
	d, ok := a.reuse(ctx, req)
	return d, ok, nil
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

// expire 把记录置为 expired；失败只记日志，不覆盖原始返回。
//
// 只剩一个用途：待办事件推不出去（连接已断且缓冲写失败），没有人会看到这张卡，
// 留着它只会变成幽灵待办。用户不点不再走这条路——那是暂停，不是超时。
func (a *runApprover) expire(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, ok, err := a.hub.store.DecideApproval(ctx, id, store.ApprovalExpired, "", ""); err != nil {
		a.hub.log.Warn("作废审批记录失败", "approval_id", id, "err", err)
	} else if ok {
		a.hub.log.Info("审批待办已作废", "approval_id", id)
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
// 决策落库之后，执行体一定已经不在跑了（审批即暂停），所以「接着走」的唯一
// 实现就是从断点续起来。唯一的例外是决策与收尾撞车：那时执行体还活着，它
// 收尾时会回查这张表并自己把续跑发起（见 execute 里的 parked 回查）。
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

	// 执行体还在跑：说明它刚登记完、还没来得及收尾。它会自己看到这条结论
	// （收尾时回查 parked），这里不必也不能再起一份执行。
	if s.runAlive(ctx, sessionID, rec.RunID) {
		return nil
	}
	return s.resumeAfterApproval(ctx, userID, sessionID)
}

// runAlive 判断这条 run 是否还有执行体在跑（本进程或别的副本）。
//
// 判据与 active-run 一致：心跳。暂停中的 run 已经落了终态，不算活着。
func (s *Service) runAlive(ctx context.Context, sessionID, runID string) bool {
	if s.runs.get(runID) != nil {
		return true
	}
	run, ok, err := s.store.GetActiveRun(ctx, sessionID, s.idleTimeout)
	return err == nil && ok && run.ID == runID
}

// expireSessionApprovals 作废会话里所有还挂着的待办。
//
// 用户又发话了，意味着上一轮已经翻篇：那一轮停在审批上，断点还在，
// 待办还挂着。留着它会有两个坏结果——卡片一直挂在界面上，点它等于把几天前的
// 半截执行另起一份继续跑；「继续」按钮也会跟新一轮抢同一个会话。
func (s *Service) expireSessionApprovals(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := s.store.ListPendingApprovals(ctx, sessionID)
	if err != nil {
		s.log.Warn("读取待办审批失败", "session_id", sessionID, "err", err)
		return
	}
	for _, a := range list {
		if _, ok, err := s.store.DecideApproval(ctx, a.ID, store.ApprovalExpired, "", ""); err != nil {
			s.log.Warn("作废待办审批失败", "approval_id", a.ID, "err", err)
		} else if ok {
			s.log.Info("新一轮开始，待办随上一轮作废", "session_id", sessionID, "approval_id", a.ID)
		}
		// 待办所属那一轮的断点一起清：那条没执行的调用再也不会被续上了
		if err := s.store.DeleteRunResume(ctx, a.RunID); err != nil {
			s.log.Warn("清理旧断点失败", "run_id", a.RunID, "err", err)
		}
	}
}

// resumeParkedDecisions 兜住「决策与收尾撞车」。
//
// 用户可能在我登记完待办、还没把 run 收尾的几毫秒里点了按钮：那时 ResolveApproval
// 看到执行体还活着，就只落库、不续跑（它也在跑，不能被起两份）。收尾之后按这张表
// 回查一遍，已经批好的立刻续起来，不会停在「批了却没动」。
func (s *Service) resumeParkedDecisions(ctx context.Context, userID, sessionID string, a *runApprover) {
	if a == nil {
		return
	}
	ids := a.parkedIDs()
	if len(ids) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, id := range ids {
		cur, ok, err := s.store.GetApproval(ctx, id)
		if err != nil || !ok || cur.Status == store.ApprovalPending {
			continue
		}
		s.log.Info("审批先于收尾到达，直接续跑", "session_id", sessionID, "approval_id", id)
		if err := s.resumeAfterApproval(ctx, userID, sessionID); err != nil && !errors.Is(err, ErrApprovalResumed) {
			s.log.Warn("审批后自动续跑失败", "session_id", sessionID, "err", err)
		}
		return
	}
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
	// 等新 run 真正登记上来再回响应：前端收到「已续跑」会立刻挂流，
	// 早于这一步它只能看到一个空窗口，然后退回「继续」按钮或报错。
	// 通常几十毫秒；等不到也不阻塞（前端会自己重试挂流）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if run, ok, err := s.store.GetActiveRun(ctx, sessionID, 0); err == nil && ok && run.ID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ErrApprovalResumed
}

// ApprovalView 是一条待处理的确认，刷新会话时据此补回卡片。
type ApprovalView struct {
	ID     string `json:"approval_id"`
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Reason string `json:"reason"`
	Risk   int    `json:"risk"`
	// RiskLabel 是具体动作标签（网络请求 / 写文件 / 脚本执行…），空串退回等级文案。
	RiskLabel string `json:"risk_label"`
}

// PendingApprovals 返回会话里所有还没失效的确认。
//
// 判据是「pending 且没过期」，不看 run 状态：这一轮可能已经暂停（它的终态就是
// 「等人」），也可能进程崩过被收尾过——待办都还在有效期内，点同意就会续起来。
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
		out = append(out, ApprovalView{
			ID:        a.ID,
			CallID:    a.CallID,
			Tool:      a.Tool,
			Kind:      a.Kind,
			Detail:    a.Detail,
			Reason:    a.Reason,
			Risk:      a.Risk,
			RiskLabel: a.RiskLabel,
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
