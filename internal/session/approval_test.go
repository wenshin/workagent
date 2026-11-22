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

func approvalReq(id, key string) tool.ApprovalRequest {
	return approvalReqWithArgs(id, key, "")
}

func approvalReqWithArgs(id, key, argsHash string) tool.ApprovalRequest {
	return tool.ApprovalRequest{
		ID:       id,
		RunID:    "r1",
		CallID:   "c-" + id,
		Tool:     "shell_exec",
		Kind:     "shell",
		Key:      key,
		ArgsHash: argsHash,
		Detail:   "rm -rf data/",
	}
}

// 命中审批不再阻塞：登记待办、回「已暂停」，由调用方结束本轮。
func TestDecideParksInsteadOfBlocking(t *testing.T) {
	st := newFakeApprovalStore()
	a := newTestApprover(st, "r1")

	done := make(chan error, 1)
	go func() {
		_, err := a.Decide(context.Background(), approvalReq("ap1", "shell:rm"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, tool.ErrApprovalParked) {
			t.Fatalf("期望暂停，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Decide 不该阻塞等人")
	}

	got, ok, _ := st.GetApproval(context.Background(), "ap1")
	if !ok || got.Status != store.ApprovalPending {
		t.Fatalf("应留下一条 pending 待办，实际 %+v", got)
	}
	// 待办与断点同寿命：过期就把「点了同意却接不上」做成了常态
	if left := time.Until(got.ExpireAt); left < approvalTTL-time.Minute {
		t.Fatalf("待办有效期应与断点一致，实际剩 %s", left)
	}
	if ids := a.parkedIDs(); len(ids) != 1 || ids[0] != "ap1" {
		t.Fatalf("应记下这一轮挂起的待办，实际 %v", ids)
	}
}

// 「本轮都同意」落库后，同一键的后续调用直接放行，不再弹第二次。
func TestDecideReusesTurnGrant(t *testing.T) {
	st := newFakeApprovalStore()
	ctx := context.Background()
	a := newTestApprover(st, "r1")

	if _, err := a.Decide(ctx, approvalReq("ap1", "shell:rm")); !errors.Is(err, tool.ErrApprovalParked) {
		t.Fatalf("第一次应暂停，实际 %v", err)
	}
	// 模拟 HTTP 决策路径：结论落库（scope=turn）
	if _, ok, _ := st.DecideApproval(ctx, "ap1", store.ApprovalApproved, "turn", "u1"); !ok {
		t.Fatal("决策应落库")
	}

	// 续跑后同键的新调用靠库里结论直接放行：不再暂停、不再问第二遍
	second, err := a.Decide(ctx, approvalReq("ap2", "shell:rm"))
	if err != nil || !second.Allow || !second.Remember {
		t.Fatalf("本轮已同意应直接放行: %+v err=%v", second, err)
	}
}

// 单次同意不产生本轮记忆：下一次同键仍要确认。
func TestDecideOnceDoesNotGrantTurn(t *testing.T) {
	st := newFakeApprovalStore()
	ctx := context.Background()
	a := newTestApprover(st, "r1")

	if _, err := a.Decide(ctx, approvalReq("ap1", "shell:rm")); !errors.Is(err, tool.ErrApprovalParked) {
		t.Fatalf("第一次应暂停，实际 %v", err)
	}
	if _, ok, _ := st.DecideApproval(ctx, "ap1", store.ApprovalApproved, "once", "u1"); !ok {
		t.Fatal("决策应落库")
	}
	// 不同参数的下一次调用：单次同意不该被复用，仍需确认
	if _, err := a.Decide(ctx, approvalReqWithArgs("ap2", "shell:rm", "h-other")); !errors.Is(err, tool.ErrApprovalParked) {
		t.Fatalf("单次同意不应复用，期望再次暂停，实际 %v", err)
	}
}

// 续跑换了 run：审批记录挂在旧 run 上，靠参数指纹也要能复用。
func TestDecideReusesApprovedByArgsAcrossRuns(t *testing.T) {
	st := newFakeApprovalStore()
	ctx := context.Background()
	a := newTestApprover(st, "r1")

	if _, err := a.Decide(ctx, approvalReqWithArgs("ap1", "shell:rm", "h-rm")); !errors.Is(err, tool.ErrApprovalParked) {
		t.Fatalf("第一次应暂停，实际 %v", err)
	}
	if _, ok, _ := st.DecideApproval(ctx, "ap1", store.ApprovalApproved, "once", "u1"); !ok {
		t.Fatal("决策应落库")
	}

	// 续跑：新 run 接替 r1，模型重新生成了调用（新 id），但参数一字不差
	next := &runApprover{
		hub:         a.hub,
		runID:       "r2",
		parentRunID: "r1",
		sessionID:   "s1",
	}
	d, err := next.Decide(ctx, approvalReqWithArgs("ap2", "shell:rm", "h-rm"))
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
