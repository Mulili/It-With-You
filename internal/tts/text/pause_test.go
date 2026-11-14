package text

import "testing"

// 停顿分级是"听起来像人"最便宜的一档：所有标点一个价时，
// 句子之间没有呼吸，听感就是念稿。这张表就是分级规则本身。
func TestPauseAfterMs(t *testing.T) {
	cases := []struct {
		in   string
		want int
		why  string
	}{
		{"你好。", PauseSentence, "句号"},
		{"真的吗？", PauseSentence, "问号"},
		{"快走！", PauseSentence, "叹号"},
		{"他——", PauseDash, "破折号要长于句号"},
		{"我……", PauseEllipsis, "省略号最长"},
		{"注意：", PauseClause, "冒号是一个意群结束"},
		{"第一，", PauseComma, "逗号最小"},
		{"好的、", PauseComma, "顿号同逗号"},
		{"没标点", PauseDefault, "没有标点就用默认值"},
		{"他说：「走吧。」", PauseSentence, "收尾引号要跳过，由句号决定"},
		{"（完）", PauseDefault, "只有括号时回退默认"},
		{"  ", PauseDefault, "空白"},
	}
	for _, c := range cases {
		if got := PauseAfterMs(c.in); got != c.want {
			t.Errorf("PauseAfterMs(%q) = %d，期望 %d（%s）", c.in, got, c.want, c.why)
		}
	}
	if !(PauseEllipsis > PauseDash && PauseDash > PauseSentence && PauseSentence > PauseClause && PauseClause > PauseComma && PauseComma > PauseDefault) {
		t.Error("分级必须严格递减：省略号 > 破折号 > 句末 > 冒号分号 > 逗号 > 默认")
	}
}
