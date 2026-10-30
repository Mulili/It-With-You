package app

import (
	"strings"
	"testing"
	"time"
)

// 2026-10-30 是周五。
func TestNowLine(t *testing.T) {
	at := time.Date(2026, 10, 30, 14, 32, 0, 0, time.Local).UnixMilli()
	got := nowLine(at)
	for _, want := range []string{"现在是", "2026-10-30", "周五", "14:32"} {
		if !strings.Contains(got, want) {
			t.Errorf("nowLine 缺少 %q：%s", want, got)
		}
	}
}

func TestChunkTimeRange(t *testing.T) {
	sameDay := chunkTimeRange(
		time.Date(2026, 10, 30, 14, 0, 0, 0, time.Local).UnixMilli(),
		time.Date(2026, 10, 30, 14, 32, 0, 0, time.Local).UnixMilli(),
	)
	if sameDay != "2026-10-30 14:00–14:32" {
		t.Errorf("同一天格式不对：%q", sameDay)
	}

	crossDay := chunkTimeRange(
		time.Date(2026, 10, 30, 23, 40, 0, 0, time.Local).UnixMilli(),
		time.Date(2026, 10, 31, 0, 20, 0, 0, time.Local).UnixMilli(),
	)
	if crossDay != "2026-10-30 23:40–2026-10-31 00:20" {
		t.Errorf("跨天格式不对：%q", crossDay)
	}

	if got := chunkTimeRange(0, 0); got != "" {
		t.Errorf("没有时间时应当返回空串，实际 %q", got)
	}
}

func TestChunkSummaryForRecall(t *testing.T) {
	// 新摘要：首行就是日期，原样返回（不要再叠加"3 天前"）
	dated := "2026-10-30 14:00–14:32\n主题：聊 TTS"
	if got := chunkSummaryForRecall(0, dated); got != dated {
		t.Errorf("新摘要应当原样返回，实际 %q", got)
	}

	// 老摘要：没有日期行，退回相对时间
	got := chunkSummaryForRecall(time.Now().UnixMilli(), "主题：聊 TTS")
	if !strings.HasPrefix(got, "今天，") {
		t.Errorf("老摘要应当带上相对时间，实际 %q", got)
	}
}
