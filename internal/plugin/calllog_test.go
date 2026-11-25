package plugin

import (
	"strings"
	"testing"
	"time"
)

// 环形覆盖只能丢最旧的：排障看的是「刚才那次」，把最新的挤掉等于没用。
func TestCallLogKeepsNewest(t *testing.T) {
	l := newCallLog(3)
	base := time.Now()
	for i := 0; i < 5; i++ {
		l.add(callEntry{
			at:       base.Add(time.Duration(i) * time.Second),
			spaceID:  "s1",
			slug:     "bot",
			toolName: "bot.bot",
			ok:       true,
		})
	}

	got := l.recent("s1", "bot", 10)
	if len(got) != 3 {
		t.Fatalf("应只保留最近 3 条，实际 %d", len(got))
	}
	// 倒序：第一条是最新的那次（第 5 条，base+4s）。
	if !got[0].At.Equal(base.Add(4 * time.Second)) {
		t.Fatalf("第一条应是最新记录，实际 %s", got[0].At)
	}
	if !got[2].At.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("最后一条应是被保留下来的最旧记录，实际 %s", got[2].At)
	}
}

// 一个空间刷满不该把另一个空间的记录挤掉。
func TestCallLogIsolatesSpaces(t *testing.T) {
	l := newCallLog(2)
	for i := 0; i < 3; i++ {
		l.add(callEntry{at: time.Now(), spaceID: "s1", slug: "bot", toolName: "bot.bot", ok: true})
	}
	l.add(callEntry{at: time.Now(), spaceID: "s2", slug: "bot", toolName: "bot.bot", ok: true})

	if got := l.recent("s1", "", 10); len(got) != 2 {
		t.Fatalf("s1 应保留 2 条，实际 %d", len(got))
	}
	if got := l.recent("s2", "", 10); len(got) != 1 {
		t.Fatalf("s2 的记录不能被 s1 挤掉，实际 %d", len(got))
	}
}

func TestCallLogFiltersBySlug(t *testing.T) {
	l := newCallLog(10)
	l.add(callEntry{at: time.Now(), spaceID: "s1", slug: "bot", toolName: "bot.bot", ok: true})
	l.add(callEntry{at: time.Now(), spaceID: "s1", slug: "other", toolName: "other.other", ok: false, err: "boom"})

	got := l.recent("s1", "bot", 10)
	if len(got) != 1 || got[0].Tool != "bot.bot" {
		t.Fatalf("应按 slug 过滤，实际 %+v", got)
	}
	if got := l.recent("s1", "", 10); len(got) != 2 {
		t.Fatalf("slug 为空时应返回全部，实际 %d", len(got))
	}
}

// 错误摘要在写入时就截断：长堆栈留在内存里没有意义，回执也会被撑爆。
func TestCallLogClipsError(t *testing.T) {
	l := newCallLog(1)
	l.add(callEntry{
		at: time.Now(), spaceID: "s1", slug: "bot", toolName: "bot.bot",
		err: strings.Repeat("x", 2000),
	})

	got := l.recent("s1", "bot", 1)
	if len(got) != 1 {
		t.Fatalf("应有一条记录，实际 %d", len(got))
	}
	if len(got[0].Err) > callLogMaxErr+len("…") {
		t.Fatalf("错误应截断到 %d 字节以内，实际 %d", callLogMaxErr, len(got[0].Err))
	}
}
