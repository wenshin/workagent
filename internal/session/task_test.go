package session

import (
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/store"
)

// 回执触发的 run 必须有密度上限：否则十条回执同时到会催出十轮模型调用。
func TestAutoRunBudget(t *testing.T) {
	b := newAutoRunBudget()
	now := time.Now()

	for i := 0; i < autoRunPerMinute; i++ {
		if !b.allow("s1", now) {
			t.Fatalf("第 %d 次应当放行", i+1)
		}
	}
	if b.allow("s1", now) {
		t.Fatal("超过每分钟上限仍然放行")
	}
	// 不同会话各自计数，互不影响
	if !b.allow("s2", now) {
		t.Fatal("另一个会话不该被前一个会话的配额挡住")
	}
	// 窗口滑过后恢复
	if !b.allow("s1", now.Add(time.Minute+time.Second)) {
		t.Fatal("一分钟之后应当恢复配额")
	}
}

// 分享链接只存摘要，且过期与吊销都算不可用。
func TestShareTokenHashAndExpiry(t *testing.T) {
	if hashToken("token-a") != hashToken("token-a") {
		t.Fatal("同一 token 的摘要必须稳定")
	}
	if hashToken("token-a") == hashToken("token-b") {
		t.Fatal("不同 token 的摘要不应相同")
	}

	now := time.Now()
	live := store.Share{ExpiresAt: now.Add(time.Hour)}
	if live.Expired(now) {
		t.Fatal("未到期且未吊销的链接应可用")
	}
	overdue := store.Share{ExpiresAt: now.Add(-time.Second)}
	if !overdue.Expired(now) {
		t.Fatal("过期链接必须不可用")
	}
	revoked := now
	if !(store.Share{ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}).Expired(now) {
		t.Fatal("已吊销的链接必须不可用")
	}
}
