package session

import (
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/store"
)

// 连续没有事件才算卡住；touch 一下就要重新计时，否则长任务会被误杀。
func TestRunWatchdogIdleTimeout(t *testing.T) {
	fired := make(chan string, 4)
	w := newRunWatchdog(120*time.Millisecond, 0, func(reason string) {
		fired <- reason
	})
	defer w.stop()

	// 每 60ms 喂一次事件：总时长早已超过 idle，但不该触发
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.touch()
		time.Sleep(60 * time.Millisecond)
	}

	select {
	case reason := <-fired:
		t.Fatalf("持续有事件时不该触发，却收到 %q", reason)
	default:
	}

	// 停止喂事件后，idle 到点必须触发且只触发一次
	select {
	case reason := <-fired:
		if reason != ReasonIdleTimeout {
			t.Fatalf("原因应为 %q，实际 %q", ReasonIdleTimeout, reason)
		}
	case <-time.After(time.Second):
		t.Fatal("idle 到点没有触发")
	}

	select {
	case <-fired:
		t.Fatal("只该触发一次")
	default:
	}
}

// 总时长上限是最后一道保险：即使事件不断，到点也要停。
func TestRunWatchdogMaxDuration(t *testing.T) {
	fired := make(chan string, 2)
	w := newRunWatchdog(0, 100*time.Millisecond, func(reason string) {
		fired <- reason
	})
	defer w.stop()

	for i := 0; i < 5; i++ {
		w.touch()
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case reason := <-fired:
		if reason != ReasonWallClockExceeded {
			t.Fatalf("原因应为 %q，实际 %q", ReasonWallClockExceeded, reason)
		}
	case <-time.After(time.Second):
		t.Fatal("总上限到点没有触发")
	}
}

// stop 之后计时器不能再回头把已经收尾的一轮判成中断。
func TestRunWatchdogStops(t *testing.T) {
	fired := make(chan struct{}, 1)
	w := newRunWatchdog(50*time.Millisecond, 80*time.Millisecond, func(string) {
		fired <- struct{}{}
	})
	w.stop()

	select {
	case <-fired:
		t.Fatal("停止后不该再触发")
	case <-time.After(200 * time.Millisecond):
	}
}

// 断点去留是产品语义：只有系统侧中断值得「继续」。
func TestKeepResume(t *testing.T) {
	cases := []struct {
		name   string
		status string
		reason string
		want   bool
	}{
		{"卡死保留", store.RunCancelled, ReasonIdleTimeout, true},
		{"到点保留", store.RunCancelled, ReasonWallClockExceeded, true},
		{"非预算类失败保留", store.RunFailed, "", true},
		{"用户停止不留", store.RunCancelled, ReasonUserCancelled, false},
		{"预算用尽不留", store.RunCancelled, ReasonBudgetExceeded, false},
		{"停滞不留", store.RunCancelled, ReasonStalled, false},
		{"成功不留", store.RunSucceeded, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keepResume(c.status, c.reason); got != c.want {
				t.Fatalf("keepResume(%q, %q) = %v，期望 %v", c.status, c.reason, got, c.want)
			}
		})
	}
}

// 三种中断的含义不同，文案必须分开：混成一句会让用户以为是同一件事。
func TestUserFacingCancelError(t *testing.T) {
	if got := userFacingCancelError(ReasonIdleTimeout, nil); got != RunIdleTimeoutMessage {
		t.Fatalf("卡死文案不对: %q", got)
	}
	if got := userFacingCancelError(ReasonWallClockExceeded, nil); got != RunBudgetExceededMessage {
		t.Fatalf("到点文案不对: %q", got)
	}
	if RunIdleTimeoutMessage == RunBudgetExceededMessage {
		t.Fatal("卡住与到点必须有各自的文案")
	}
}
