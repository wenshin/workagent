package agent

import (
	"testing"
	"time"
)

// 思考耗时按「首段增量到换段」结算：工具调用插进来与一轮收尾各结算一次。
//
// 续写同一段（逐个 token 到达）不能重置起点，否则耗时只剩最后一个 token 的时间；
// 正文段不记耗时——整轮耗时在 message.duration_ms，同一段时间不记两处。
func TestSegmentTracksReasoningDuration(t *testing.T) {
	var res Result
	res.AppendSegment("reasoning", "先想")
	time.Sleep(5 * time.Millisecond)
	res.AppendSegment("reasoning", "再想一点") // 续写同一段：起点保持第一次到达
	time.Sleep(5 * time.Millisecond)
	res.AppendToolSegment("c1")
	time.Sleep(2 * time.Millisecond)
	res.AppendSegment("reasoning", "收个尾")
	time.Sleep(5 * time.Millisecond)
	res.CloseTimeline()

	if len(res.Timeline) != 3 {
		t.Fatalf("时间线应为 思考 → 工具 → 思考 三段，实际 %+v", res.Timeline)
	}
	if d := res.Timeline[0].DurationMS; d < 8 {
		t.Fatalf("第一段思考应含两次增量的时间（≥8ms），实际 %dms", d)
	}
	if d := res.Timeline[1].DurationMS; d != 0 {
		t.Fatalf("工具段不记思考耗时，实际 %dms", d)
	}
	// 最后一段没人换段，只有 CloseTimeline 能结算
	if d := res.Timeline[2].DurationMS; d < 4 {
		t.Fatalf("收尾那段思考应由 CloseTimeline 结算（≥4ms），实际 %dms", d)
	}
}

// 正文段不结算耗时：换段时只认思考段。
func TestSegmentLeavesTextWithoutDuration(t *testing.T) {
	var res Result
	res.AppendSegment("reasoning", "想")
	time.Sleep(3 * time.Millisecond)
	res.AppendSegment("text", "答")
	res.CloseTimeline()

	if len(res.Timeline) != 2 {
		t.Fatalf("时间线应为 思考 → 正文 两段，实际 %+v", res.Timeline)
	}
	if res.Timeline[1].DurationMS != 0 {
		t.Fatalf("正文段不该带耗时，实际 %dms", res.Timeline[1].DurationMS)
	}
}
