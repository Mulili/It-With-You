package text

import (
	"strings"
)

// 中文标点 → 模型认识的那几个（与 GENIE 的 PUNCTUATION_REPLACEMENTS 一致）。
// 映射后只剩 6 个标点参与音素，其余（引号、括号、markdown 符号）一律丢掉。
var punctReplacements = map[rune]string{
	'：': ",", '；': ",", '，': ",", '。': ".", '！': "!", '？': "?", '\n': ".",
	'·': ",", '、': ",", '$': ".", '/': ",", '—': "-", '~': "…", '～': "…",
}

var allowedPunct = map[rune]bool{
	'!': true, '?': true, '…': true, ',': true, '.': true, '-': true,
}

// Normalize 把一段任意文本收拾成"只有汉字 + 6 个标点 + 阿拉伯数字已转汉字"。
//
// 英文与表情会被**整段丢掉**，这不是偷懒：中文模型不认识它们，留着只会让
// 音素表查不到、然后静默变成空白。真正的中英混排要做 §6 第 5 条（按语言切开各走一条），
// 那是下一步。
func Normalize(s string) string {
	s = strings.ReplaceAll(s, "...", "…")

	var b strings.Builder
	for _, r := range s {
		if rep, ok := punctReplacements[r]; ok {
			b.WriteString(rep)
			continue
		}
		if isHan(r) || allowedPunct[r] || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		// 百分号要留到数字归一化那一步（50% → 百分之五十），之后统一清掉
		if r == '%' || r == '％' {
			b.WriteRune('%')
		}
	}

	out := collapsePunct(b.String())
	out = normalizeNumbers(out)
	return strings.ReplaceAll(out, "%", "")
}

// collapsePunct 把连续相同的标点压成一个（"。。。" → "。"）。
func collapsePunct(s string) string {
	var b strings.Builder
	var last rune
	for _, r := range s {
		if r == last && allowedPunct[r] {
			continue
		}
		b.WriteRune(r)
		last = r
	}
	return b.String()
}

var (
	digitName = [...]string{"零", "一", "二", "三", "四", "五", "六", "七", "八", "九"}
	unitSmall = [...]string{"", "十", "百", "千"}
	unitBig   = [...]string{"", "万", "亿"}
)

// normalizeNumbers 把数字读法写出来：1998 → 一九九八，50% → 百分之五十，3.14 → 三点一四。
//
// 这是方案 §6 第 1 条里"最常被漏、对听感影响最大"的那一项。这里只覆盖常见形态：
// 整数 / 小数 / 百分数 / 四位年份。连写单位（"3 天"）、分数、负数都还没做。
func normalizeNumbers(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if rs[i] < '0' || rs[i] > '9' {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
			j++
		}
		intPart := string(rs[i:j])
		// 小数
		if j+1 < len(rs) && rs[j] == '.' && rs[j+1] >= '0' && rs[j+1] <= '9' {
			k := j + 1
			for k < len(rs) && rs[k] >= '0' && rs[k] <= '9' {
				k++
			}
			b.WriteString(readInt(intPart))
			b.WriteString("点")
			b.WriteString(readPerDigit(string(rs[j+1 : k])))
			i = k
			continue
		}
		// 百分数
		if j < len(rs) && rs[j] == '%' {
			b.WriteString("百分之")
			b.WriteString(readInt(intPart))
			i = j + 1
			continue
		}
		b.WriteString(readInt(intPart))
		i = j
	}
	return b.String()
}

func readPerDigit(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteString(digitName[r-'0'])
	}
	return b.String()
}

func readInt(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "零"
	}
	// 四位年份按位读（1998 → 一九九八）
	if len(s) == 4 && (strings.HasPrefix(s, "19") || strings.HasPrefix(s, "20")) {
		return readPerDigit(s)
	}
	if len(s) > 8 { // 亿以上不折腾，按位读
		return readPerDigit(s)
	}
	groups := make([]string, 0, 2)
	for len(s) > 4 {
		groups = append([]string{s[len(s)-4:]}, groups...)
		s = s[:len(s)-4]
	}
	groups = append([]string{s}, groups...)

	var b strings.Builder
	for i, g := range groups {
		part := readGroup(g)
		if part == "" {
			continue
		}
		b.WriteString(part)
		b.WriteString(unitBig[len(groups)-1-i])
	}
	out := b.String()
	// "一十三" → "十三"
	if strings.HasPrefix(out, "一十") {
		out = out[3:]
	}
	return out
}

func readGroup(g string) string {
	var b strings.Builder
	pendingZero := false
	for i, r := range g {
		d := int(r - '0')
		pos := len(g) - 1 - i
		if d == 0 {
			pendingZero = true
			continue
		}
		if pendingZero && b.Len() > 0 {
			b.WriteString("零")
		}
		pendingZero = false
		b.WriteString(digitName[d])
		b.WriteString(unitSmall[pos])
	}
	return b.String()
}

func isHan(r rune) bool { return r >= 0x4e00 && r <= 0x9fa5 }
