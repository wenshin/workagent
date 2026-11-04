package llm

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("空串应为 0，得到 %d", got)
	}
	// 1000 个汉字约 600 tokens（0.6/字）
	zh := strings.Repeat("技", 1000)
	if got := EstimateTokens(zh); got < 550 || got > 650 {
		t.Fatalf("中文估算偏差过大：%d", got)
	}
	// 400 个 ASCII 字符约 100 tokens（0.25/字符）
	en := strings.Repeat("a", 400)
	if got := EstimateTokens(en); got < 90 || got > 120 {
		t.Fatalf("英文估算偏差过大：%d", got)
	}
}

func TestTruncateByTokens(t *testing.T) {
	short := "很短的一段话"
	if got, truncated := TruncateByTokens(short, 100); truncated || got != short {
		t.Fatalf("未超预算不应截断：%q %v", got, truncated)
	}

	long := strings.Repeat("这一行是技能正文内容。\n", 500)
	got, truncated := TruncateByTokens(long, 100)
	if !truncated {
		t.Fatal("超预算应截断")
	}
	if EstimateTokens(got) > 110 {
		t.Fatalf("截断后仍超预算：%d", EstimateTokens(got))
	}
	if !strings.HasSuffix(got, "。") {
		t.Fatalf("截断点应回退到行边界：%q", got[len(got)-10:])
	}
}
