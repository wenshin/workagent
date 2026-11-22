package session

import (
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
)

// ClientClock 是客户端在请求里上报的时间语义（可选，缺失即回落服务器时间）。
//
// 两个字段分开给：Time 是用户看到的墙上时间（RFC3339，带偏移），Zone 是 IANA
// 时区名（如 Asia/Shanghai）。只有偏移时 DST 换算不回去，只有时区名时又依赖
// 服务端有没有 tzdata，两者一起给最稳；少一个也能用。
type ClientClock struct {
	Time string
	Zone string
}

// resolveClock 决定本轮注入系统提示词的「现在」。
//
// 优先用户端上报：用户在哪个时区、他看到的今天是几号，只有用户端知道——服务端按
// 自己的时区算，会把 UTC+8 的凌晨算成前一天，「今天」被答成「昨天」。用户端没上报
// （任务回执、外部集成）才回落服务器时间，并在提示词里标明来源，让模型必要时说明。
func resolveClock(clock ClientClock, serverNow func() time.Time) agent.ClockFacts {
	zone := strings.TrimSpace(clock.Zone)
	if raw := strings.TrimSpace(clock.Time); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return agent.ClockFacts{Now: shiftTo(t, zone), Zone: zone, Source: agent.ClockClient}
		}
	}
	now := serverNow()
	if zone != "" {
		// 有用户时区、没有用户时间：用服务器时钟换算到用户时区，日期至少对得上，
		// 不会出现「用户刚过零点、模型还在昨天」这种偏差
		if loc, err := time.LoadLocation(zone); err == nil {
			now = now.In(loc)
		}
	}
	return agent.ClockFacts{Now: now, Zone: zone, Source: agent.ClockServer}
}

// shiftTo 把时间点换到用户时区。加载不到时区就保留解析出来的偏移——
// 仍是正确的当地时间，只是没有时区名（提示词里只写数字偏移）。
func shiftTo(t time.Time, zone string) time.Time {
	if zone == "" {
		return t
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return t
	}
	return t.In(loc)
}
