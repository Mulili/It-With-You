package text

import "strings"

// Splitter 是方案 §9 那套"两个阈值 + 有效长度"的分句器（照 GENIE 的 TextSplitter 移植）。
//
// 有效长度：标点不计入，ASCII 记 1、CJK 记 2。
// 终止符（。！？…!?.）→ 有效长度 ≥ MinLen 就切；
// 分隔符（，、；：——,;: 与引号）→ 有效长度 ≥ MaxLen 才切，否则继续攒。
type Splitter struct {
	MaxLen int
	MinLen int
}

// NewSplitter 用方案里的默认值（max 40 / min 5）。
func NewSplitter() *Splitter {
	return &Splitter{MaxLen: 40, MinLen: 5}
}

var (
	endChars = map[rune]bool{
		'。': true, '！': true, '？': true, '…': true, '!': true, '?': true, '.': true,
	}
	sepChars = map[rune]bool{
		'，': true, '、': true, '；': true, '：': true, '—': true,
		',': true, ';': true, ':': true,
	}
	quoteChars = map[rune]bool{
		'“': true, '”': true, '‘': true, '’': true, '"': true, '\'': true,
	}
)

func allPunct(r rune) bool {
	return endChars[r] || sepChars[r] || quoteChars[r]
}

// Split 把一段文本切成若干句。返回的句子保留自身标点。
func (s *Splitter) Split(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "\n", "")

	var sentences []string
	var buf strings.Builder

	for _, block := range splitPunctBlocks(text) {
		if block == "" {
			continue
		}
		if !allPunct([]rune(block)[0]) {
			// 一整块**自身**就超长（代码、无标点的长串）时按上限硬切；
			// 普通中文（逗号很多）不在这里切——否则会把词劈开，正是"吞字断句怪"的来源。
			blockEff := 0
			for _, r := range block {
				buf.WriteRune(r)
				blockEff += runeLen(r)
				if s.MaxLen > 0 && blockEff >= s.MaxLen {
					sentences = append(sentences, strings.TrimSpace(buf.String()))
					buf.Reset()
					blockEff = 0
				}
			}
			continue
		}

		buf.WriteString(block)
		eff := effectiveLen(buf.String())
		if blockHasEnd(block) {
			if eff >= s.MinLen {
				sentences = append(sentences, strings.TrimSpace(buf.String()))
				buf.Reset()
			}
			continue
		}
		if eff >= s.MaxLen {
			sentences = append(sentences, strings.TrimSpace(buf.String()))
			buf.Reset()
		}
	}

	if rest := strings.TrimSpace(buf.String()); rest != "" {
		if effectiveLen(rest) > 0 {
			sentences = append(sentences, rest)
		} else if len(sentences) > 0 {
			sentences[len(sentences)-1] += rest
		}
	}
	return sentences
}

// splitPunctBlocks 把文本切成"文本块 + 标点块"交替的片段（等价于 Python 的 re.split）。
func splitPunctBlocks(text string) []string {
	rs := []rune(text)
	var out []string
	start := 0
	inPunct := false
	for i, r := range rs {
		p := allPunct(r)
		if i == 0 {
			inPunct = p
			continue
		}
		if p != inPunct {
			out = append(out, string(rs[start:i]))
			start = i
			inPunct = p
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

func effectiveLen(s string) int {
	n := 0
	for _, r := range s {
		if allPunct(r) {
			continue
		}
		n += runeLen(r)
	}
	return n
}

func runeLen(r rune) int {
	if r < 128 {
		return 1
	}
	return 2
}

func blockHasEnd(block string) bool {
	for _, r := range block {
		if endChars[r] {
			return true
		}
	}
	return false
}
