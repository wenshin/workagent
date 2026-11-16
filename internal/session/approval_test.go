package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// fakeApprovalStore 只实现审批相关的六条 SQL 语义，其余走嵌入的 nil 接口。
type fakeApprovalStore struct {
	store.Store
	mu       sync.Mutex
	rows     map[string]store.Approval
	statuses []string
}

func newFakeApprovalStore() *fakeApprovalStore {
	return &fakeApprovalStore{rows: map[string]store.Approval{}}
}

func (f *fakeApprovalStore) CreateApproval(_ context.Context, a store.Approval) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.rows[a.ID]; ok {
		return old, nil
	}
	if a.Status == "" {
		a.Status = store.ApprovalPending
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	f.rows[a.ID] = a
	return a, nil
}

func (f *fakeApprovalStore) DecideApproval(_ context.Context, id, status, scope, decidedBy string) (store.Approval, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.rows[id]
	if !ok || a.Status != store.ApprovalPending {
		return store.Approval{}, false, nil
	}
	a.Status = status
	if scope != "" {
		a.Scope = scope
	}
	a.DecidedBy = decidedBy
	now := time.Now()
	a.DecidedAt = &now
	f.rows[id] = a
	return a, true, nil
}

func (f *fakeApprovalStore) GetApproval(_ context.Context, id string) (store.Approval, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.rows[id]
	return a, ok, nil
}

func (f *fakeApprovalStore) FindApprovalGrant(_ context.Context, runID, actionKey, argsHash string) (store.Approval, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out store.Approval
	found := false
	for _, a := range f.rows {
		if a.RunID != runID {
			continue
		}
		if a.Status != store.ApprovalApproved && a.Status != store.ApprovalDenied {
			continue
		}
		byTurn := a.Scope == "turn" && a.ActionKey == actionKey
		byArgs := argsHash != "" && a.ArgsHash == argsHash
		if !byTurn && !byArgs {
			continue
		}
		if !found || a.CreatedAt.After(out.CreatedAt) {
			out, found = a, true
		}
	}
	return out, found, nil
}

// SetRunStatus 记录状态变化，用来断言「等人的时候真的落成 waiting_approval」。
func (f *fakeApprovalStore) SetRunStatus(_ context.Context, runID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeApprovalStore) MarkApprovalExpired(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.rows[id]; ok && (a.Status == store.ApprovalApproved || a.Status == store.ApprovalDenied) {
		a.Status = store.ApprovalExpired
		f.rows[id] = a
	}
	return nil
}

func (f *fakeApprovalStore) ExpireRunApprovals(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, a := range f.rows {
		if a.RunID == runID && a.Status == store.ApprovalPending {
			a.Status = store.ApprovalExpired
			f.rows[id] = a
		}
	}
	return nil
}

func newTestApprover(st *fakeApprovalStore, runID string) *runApprover {
	return &runApprover{
		hub:       newApprovalHub(st, nil),
		runID:     runID,
		sessionID: "s1",
	}
}

func approvalReq(id, key string, timeout time.Duration) tool.ApprovalRequest {
	return approvalReqWithArgs(id, key, "", timeout)
}

func approvalReqWithArgs(id, key, argsHash string, timeout time.Duration) tool.ApprovalRequest {
	return tool.ApprovalRequest{
		ID:       id,
		RunID:    "r1",
		CallID:   "c-" + id,
		Tool:     "shell_exec",
		Kind:     "shell",
		Key:      key,
		ArgsHash: argsHash,
		Detail:   "rm -rf data/",
		Timeout:  timeout,
	}
}

// 超时按拒绝：记录要落成 expired，不能留在 pending 变成幽灵待办。
func TestDecideTimeoutExpiresRecord(t *testing.T) {
	st := newFakeApprovalStore()
	a := newTestApprover(st, "r1")

	_, err := a.Decide(context.Background(), approvalReq("ap1", "shell:rm", 30*time.Millisecond))
	if !errors.Is(err, tool.ErrApprovalTimeout) {
		t.Fatalf("期望超时，实际 %v", err)
	}
	got, ok, _ := st.GetApproval(context.Background(), "ap1")
	if !ok || got.Status != store.ApprovalExpired {
		t.Fatalf("超时后记录应为 expired，实际 %+v", got)
	}
}

// 「本轮都同意」落库后，同一键的后续调用直接放行，不再弹第二次。
func TestDecideReusesTurnGrant(t *testing.T) {
	st := newFakeApprovalStore()
	a := newTestApprover(st, "r1")

	// 模拟 HTTP 决策路径：先落库（scope=turn），再唤醒执行体
	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(5 * time.Millisecond)
			if _, ok, _ := st.GetApproval(context.Background(), "ap1"); ok {
				_, _, _ = st.DecideApproval(context.Background(), "ap1", store.ApprovalApproved, "turn", "u1")
				a.hub.resolve("ap1", tool.ApprovalDecision{Allow: true, Remember: true})
				return
			}
		}
	}()

	first, err := a.Decide(context.Background(), approvalReq("ap1", "shell:rm", 2*time.Second))
	if err != nil || !first.Allow || !first.Remember {
		t.Fatalf("第一次应被同意并记住本轮: %+v err=%v", first, err)
	}

	// 第二次是不同 id 的新调用，靠库里的 turn 结论直接返回，不会阻塞
	second, err := a.Decide(context.Background(), approvalReq("ap2", "shell:rm", 50*time.Millisecond))
	if err != nil || !second.Allow {
		t.Fatalf("本轮已同意应直接放行: %+v err=%v", second, err)
	}
}

// 单次同意不产生本轮记忆：下一次同键仍要确认。
func TestDecideOnceDoesNotGrantTurn(t *testing.T) {
	st := newFakeApprovalStore()
	a := newTestApprover(st, "r1")

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(5 * time.Millisecond)
			if _, ok, _ := st.GetApproval(context.Background(), "ap1"); ok {
				_, _, _ = st.DecideApproval(context.Background(), "ap1", store.ApprovalApproved, "once", "u1")
				a.hub.resolve("ap1", tool.ApprovalDecision{Allow: true, Remember: false})
				return
			}
		}
	}()

	if d, err := a.Decide(context.Background(), approvalReq("ap1", "shell:rm", 2*time.Second)); err != nil || !d.Allow {
		t.Fatalf("单次同意应放行: %+v err=%v", d, err)
	}
	if _, err := a.Decide(context.Background(), approvalReq("ap2", "shell:rm", 30*time.Millisecond)); !errors.Is(err, tool.ErrApprovalTimeout) {
		t.Fatalf("单次同意不应复用，期望再次等待超时，实际 %v", err)
	}
}

// 等人的时候状态要落成 waiting_approval：前端据此显示「等待确认」，
// 清扫也不会把这一轮当成崩溃收掉；拿到决策后必须切回 running。
func TestDecideMarksWaitingApproval(t *testing.T) {
	st := newFakeApprovalStore()
	a := newTestApprover(st, "r1")

	seen := make(chan []string, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		st.mu.Lock()
		seen <- append([]string{}, st.statuses...)
		st.mu.Unlock()
	}()

	_, err := a.Decide(context.Background(), approvalReq("ap1", "shell:rm", 60*time.Millisecond))
	if !errors.Is(err, tool.ErrApprovalTimeout) {
		t.Fatalf("期望超时，实际 %v", err)
	}

	got := <-seen
	if len(got) == 0 || got[0] != store.RunWaitingApproval {
		t.Fatalf("等待期间应为 waiting_approval，实际 %v", got)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if last := st.statuses[len(st.statuses)-1]; last != store.RunRunning {
		t.Fatalf("等待结束后应切回 running，实际 %s", last)
	}
}

// 续跑换了 run：审批记录挂在旧 run 上，靠参数指纹也要能复用。
func TestDecideReusesApprovedByArgsAcrossRuns(t *testing.T) {
	st := newFakeApprovalStore()
	a := newTestApprover(st, "r1")

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(5 * time.Millisecond)
			if _, ok, _ := st.GetApproval(context.Background(), "ap1"); ok {
				_, _, _ = st.DecideApproval(context.Background(), "ap1", store.ApprovalApproved, "once", "u1")
				a.hub.resolve("ap1", tool.ApprovalDecision{Allow: true})
				return
			}
		}
	}()

	if d, err := a.Decide(context.Background(),
		approvalReqWithArgs("ap1", "shell:rm", "h-rm", 2*time.Second)); err != nil || !d.Allow {
		t.Fatalf("第一次应被同意: %+v err=%v", d, err)
	}

	// 续跑：新 run 接替 r1，模型重新生成了调用（新 id），但参数一字不差
	next := &runApprover{
		hub:         a.hub,
		runID:       "r2",
		parentRunID: "r1",
		sessionID:   "s1",
	}
	d, err := next.Decide(context.Background(),
		approvalReqWithArgs("ap2", "shell:rm", "h-rm", 30*time.Millisecond))
	if err != nil || !d.Allow {
		t.Fatalf("同参调用应复用旧结论，实际 %+v err=%v", d, err)
	}
}

// run 结束把待办收尾：没有人再等结果了。
func TestExpireRunApprovals(t *testing.T) {
	st := newFakeApprovalStore()
	ctx := context.Background()
	if _, err := st.CreateApproval(ctx, store.Approval{ID: "ap1", RunID: "r1", Status: store.ApprovalPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.ExpireRunApprovals(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.GetApproval(ctx, "ap1")
	if got.Status != store.ApprovalExpired {
		t.Fatalf("run 收尾后应为 expired，实际 %s", got.Status)
	}
}
