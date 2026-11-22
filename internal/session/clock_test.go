package session

import (
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
)

// serverNowUTC 固定成一个「UTC 还在 11-21」的时刻：UTC+8 已经是 11-22 的凌晨。
// 「今天」被答成「昨天」正是这个错位。
func serverNowUTC() time.Time {
	return time.Date(2026, 11, 21, 16, 30, 0, 0, time.UTC)
}

// 用户端上报了本地时间就按用户端的来。
func TestResolveClockPrefersClient(t *testing.T) {
	// 即使服务端还在 11-21，用户看到的也已经是 11-22
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		t.Skip("缺少 tzdata，跳过时区换算断言")
	}
	got := resolveClock(ClientClock{
		Time: "2026-11-22T00:30:00+08:00",
		Zone: "Asia/Shanghai",
	}, serverNowUTC)

	if got.Source != agent.ClockClient {
		t.Fatalf("有用户端时间就应该按用户端处理，实际 %q", got.Source)
	}
	if date := got.Now.Format("2006-01-02"); date != "2026-11-22" {
		t.Fatalf("应按用户时区取日期（11-22），实际 %s", date)
	}
	if got.Zone != "Asia/Shanghai" {
		t.Fatalf("时区名应原样带出，实际 %q", got.Zone)
	}
}

// 只给时间不给时区名也要能用：偏移本身已经是正确的当地时间。
func TestResolveClockKeepsOffsetWithoutZone(t *testing.T) {
	got := resolveClock(ClientClock{Time: "2026-11-22T00:30:00+08:00"}, serverNowUTC)

	if got.Source != agent.ClockClient {
		t.Fatalf("应识别为用户端时间，实际 %q", got.Source)
	}
	if got.Now.Format("2006-01-02 15:04") != "2026-11-22 00:30" {
		t.Fatalf("应保留上报的墙上时间，实际 %s", got.Now.Format("2006-01-02 15:04"))
	}
	if _, offset := got.Now.Zone(); offset != 8*3600 {
		t.Fatalf("应保留上报的偏移 +08:00，实际 %d 秒", offset)
	}
}

// 用户端没上报（任务回执、外部集成）才回落服务器时间，并标明来源。
func TestResolveClockFallsBackToServer(t *testing.T) {
	got := resolveClock(ClientClock{}, serverNowUTC)

	if got.Source != agent.ClockServer {
		t.Fatalf("没有用户端时间应回落服务器时间，实际 %q", got.Source)
	}
	if !got.Now.Equal(serverNowUTC()) {
		t.Fatalf("应使用服务器时间，实际 %s", got.Now)
	}
}

// 只有用户时区、没有用户时间：用服务器时钟换算到用户时区，日期至少对得上。
func TestResolveClockUsesZoneForServerTime(t *testing.T) {
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		t.Skip("缺少 tzdata，跳过时区换算断言")
	}
	got := resolveClock(ClientClock{Zone: "Asia/Shanghai"}, serverNowUTC)

	if got.Source != agent.ClockServer {
		t.Fatalf("没有用户端时间时应标明来源是服务器，实际 %q", got.Source)
	}
	if date := got.Now.Format("2006-01-02"); date != "2026-11-22" {
		t.Fatalf("服务器时间应换算到用户时区（11-22），实际 %s", date)
	}
}

// 时间串解析不了就当没上报：宁可回落服务器时间，也不能把脏值当成「现在」。
func TestResolveClockIgnoresUnparsableTime(t *testing.T) {
	got := resolveClock(ClientClock{Time: "昨天晚上"}, serverNowUTC)

	if got.Source != agent.ClockServer {
		t.Fatalf("非法时间应回落服务器时间，实际 %q", got.Source)
	}
	if !got.Now.Equal(serverNowUTC()) {
		t.Fatalf("应使用服务器时间，实际 %s", got.Now)
	}
}
