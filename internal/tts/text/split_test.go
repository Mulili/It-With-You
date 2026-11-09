package text

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 约定：每个句末标点就是一次推理任务；单次任务最多 80 个汉字。
func TestSplitByPunctuationMax80(t *testing.T) {
	s := &Splitter{MaxLen: 160, MinLen: 1}

	got := s.Split("你好。今天天气不错，我们一起出去走走吧。")
	want := []string{"你好。", "今天天气不错，我们一起出去走走吧。"}
	if len(got) != len(want) {
		t.Fatalf("切出 %d 段，期望 %d 段：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 段 = %q，期望 %q", i, got[i], want[i])
		}
	}

	// 没有标点的长文本也要能切（否则只能等流结束才出声）
	long := strings.Repeat("好", 100)
	parts := s.Split(long)
	if len(parts) != 2 {
		t.Fatalf("100 字应为 2 段，实际 %d 段", len(parts))
	}
	if n := utf8.RuneCountInString(parts[0]); n != 80 {
		t.Errorf("第一段 %d 字，期望 80 字", n)
	}
	if n := utf8.RuneCountInString(parts[1]); n != 20 {
		t.Errorf("第二段 %d 字，期望 20 字", n)
	}
}
