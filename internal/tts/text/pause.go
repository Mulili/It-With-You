package text

// 句间停顿的分级（毫秒）。它是"这句话读完到下一句开口"之间的静音量。
//
// 为什么要在播放端补，而不是让模型去"读"标点：模型看到的标点只是一个音素符号，
// 它给不出可控的时长；而停顿是**最容易让人觉得"像人在说话"**的东西，必须可调、可验证。
// 音素流里保留一个句内断开（破折号已映射成逗号），真正的长短由这里决定。
//
// 数值是起点，判据是听感：连续两句之间若显得"抢话"就加大，若显得"发呆"就减小。
const (
	// PauseSentence 句末（。！？）：一句话说完了，留一口气
	PauseSentence = 320
	// PauseEllipsis 省略号：比句末更长，因为它本身就是"话没说完"
	PauseEllipsis = 450
	// PauseDash 破折号：转折/拉长，长于句末、短于省略号
	PauseDash = 420
	// PauseClause 冒号、分号：一个意群结束，但话还在继续
	PauseClause = 240
	// PauseComma 逗号、顿号：最小的一次换气
	PauseComma = 150
	// PauseDefault 句尾没有标点（流式被截断、或单纯没写）
	PauseDefault = 80
)

// PauseAfterMs 返回"这句话读完之后应该停多久"。
//
// 只看**句末那个有效标点**：句子里有逗号不影响，那是句内的事。
// 收尾的引号、右括号要跳过——"他说：「走吧。」" 的停顿由句号决定，不是由引号决定。
func PauseAfterMs(sentence string) int {
	r, ok := lastSignificantRune(sentence)
	if !ok {
		return PauseDefault
	}
	switch r {
	case '。', '！', '？', '!', '?', '.':
		return PauseSentence
	case '…':
		return PauseEllipsis
	case '—', '－', '–':
		return PauseDash
	case '：', '；', ':', ';':
		return PauseClause
	case '，', '、', ',':
		return PauseComma
	}
	return PauseDefault
}

// lastSignificantRune 从尾部找第一个"算数的字符"，跳过空白与成对的收尾符号。
func lastSignificantRune(s string) (rune, bool) {
	rs := []rune(s)
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if quoteChars[r] || closerChars[r] {
			continue
		}
		return r, true
	}
	return 0, false
}

// closerChars 是"句末标点之后可能出现的收尾符号"，它们不决定停顿。
var closerChars = map[rune]bool{
	'）': true, ')': true, '】': true, '》': true, '」': true, '』': true, '〕': true,
}
