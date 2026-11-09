package text

import (
	"strings"

	"github.com/mozillazg/go-pinyin"
)

// 与 GENIE/opencpop 一致的几张小表。
var (
	vRepMap  = map[string]string{"uei": "ui", "iou": "iu", "uen": "un"}
	pyRepMap = map[string]string{
		"ing": "ying", "i": "yi", "in": "yin", "u": "wu",
	}
	singleRepMap = map[string]string{"v": "yu", "e": "e", "i": "y", "u": "w"}
)

// initials 按长度降序：先试 zh/ch/sh，再试单字母，否则会把 "zhi" 拆成 z + hi。
var initials = []string{
	"zh", "ch", "sh",
	"b", "p", "m", "f", "d", "t", "n", "l", "g", "k", "h",
	"j", "q", "x", "r", "z", "c", "s", "y", "w",
}

type syllable struct {
	ch rune   // 汉字或标点
	py string // 汉字：Tone3 拼音（如 hao3）；标点：原字符
}

// syllables 把汉字串切成"每个字一个拼音"，多音字用词表覆盖。
func (f *Frontend) syllables(text string) []syllable {
	rs := []rune(text)
	out := make([]syllable, 0, len(rs))
	for i := 0; i < len(rs); {
		r := rs[i]
		if !isHan(r) {
			out = append(out, syllable{ch: r, py: string(r)})
			i++
			continue
		}
		if n, py := f.matchWord(rs, i); n > 0 {
			for k := 0; k < n; k++ {
				out = append(out, syllable{ch: rs[i+k], py: withNeutralTone(py[k])})
			}
			i += n
			continue
		}
		out = append(out, syllable{ch: r, py: withNeutralTone(f.charPinyin(r))})
		i++
	}
	applySandhi(out)
	applyThreeSandhi(out)
	return out
}

// applyThreeSandhi 做三声连读变调：同一段连续的三声里，只有最后一个读三声，其余读二声。
//
// 这里**没有分词**，所以是"按连续区间"而不是"按词"：好/喜欢 这种跨词边界的组合会被
// 一并变调。听感上差别很小（普通话的连读本来就会跨词），而换 jieba 进来要多背一个
// 词典与词性标注，收益不成比例。标点会打断区间，所以跨句不会串。
func applyThreeSandhi(syls []syllable) {
	for i := 0; i < len(syls); {
		if !isHan(syls[i].ch) || toneOf(syls[i].py) != '3' {
			i++
			continue
		}
		j := i
		for j < len(syls) && isHan(syls[j].ch) && toneOf(syls[j].py) == '3' {
			j++
		}
		if j-i >= 2 {
			for k := i; k < j-1; k++ {
				syls[k].py = withTone(syls[k].py, '2')
			}
		}
		i = j
	}
}

func withTone(py string, tone byte) string {
	if py == "" {
		return py
	}
	return py[:len(py)-1] + string(tone)
}

// withNeutralTone 给**没有声调数字**的拼音补上 5（轻声）。
//
// go-pinyin 对"的/了/着/吗"这类字返回的是 "de"、"le"——不带调号。而音素表里只有
// "e5"（轻声）没有 "e"，不补这一步整字的拼音会被 splitPhone 判成"没有声调"直接丢掉，
// 表现出来的就是**"的""了"全被吞了**（这是实测踩到的坑，不是理论问题）。
// pypinyin 的 neutral_tone_with_five=True 就是同一个约定。
func withNeutralTone(py string) string {
	if py == "" {
		return py
	}
	c := py[len(py)-1]
	if c >= '1' && c <= '5' {
		return py
	}
	return py + "5"
}

// interjectionPinyin 是语气词的读音补丁。
//
// go-pinyin 对"嗯"给的是 "n2"、"呣"给的是 "m2"——opencpop 里没有 n / m 这两个音节，
// 结果整字被 splitPhone 丢掉，表现出来就是"嗯。"只剩一个句号、合成出来 40 毫秒。
// 这两个音就近映射到 en / mu（听感最接近，且两音节都在表里），声调交给
// withNeutralTone 补成轻声 5。
var interjectionPinyin = map[rune]string{
	'嗯': "en",
	'呣': "mu",
}

func (f *Frontend) charPinyin(r rune) string {
	if py, ok := interjectionPinyin[r]; ok {
		return py
	}
	res := pinyin.Pinyin(string(r), f.pyArgs)
	if len(res) == 0 || len(res[0]) == 0 || strings.TrimSpace(res[0][0]) == "" {
		return string(r)
	}
	return res[0][0]
}

// Phones 是包外唯一入口：一段文本 → GPT-SoVITS 的音素序列。
//
// 查不到读音的音节会被**跳过**（与 GENIE 的 KeyError → continue 一致）：
// 少一个字只是口齿不清，整句报错会让助手一句话都说不出来。
func (f *Frontend) Phones(s string) []string {
	syls := f.syllables(Normalize(s))
	phones := make([]string, 0, len(syls)*2)
	for _, sy := range syls {
		if !isHan(sy.ch) {
			phones = append(phones, string(sy.ch))
			continue
		}
		c, v, ok := f.splitPhone(sy.py)
		if !ok {
			continue
		}
		phones = append(phones, c, v)
	}
	return phones
}

// splitPhone 把 "hao3" 变成 ("h", "ao3")。
func (f *Frontend) splitPhone(py string) (string, string, bool) {
	if len(py) < 2 {
		return "", "", false
	}
	tone := py[len(py)-1]
	if tone < '1' || tone > '5' {
		return "", "", false
	}
	base := py[:len(py)-1]

	c, v := "", base
	for _, in := range initials {
		if strings.HasPrefix(base, in) {
			c, v = in, base[len(in):]
			break
		}
	}

	var key string
	if c != "" {
		final := v
		if rep, ok := vRepMap[v]; ok {
			final = rep
		}
		key = c + final
	} else if v != "" {
		if k, ok := pyRepMap[v]; ok {
			key = k
		} else if rep, ok := singleRepMap[string(v[0])]; ok {
			key = rep + v[1:]
		} else {
			key = v
		}
	}
	if key == "" {
		return "", "", false
	}

	phoneStr, ok := f.opencpop[key]
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(phoneStr, " ", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1] + string(tone), true
}

// applySandhi 只做两条**不依赖分词**的变调：不 / 一。
//
// 三声连读（"你好" → ni2 hao3）需要词边界与词性，这一版没做——它只影响自然度，
// 不影响能不能听懂。做之前先听一遍，别为了它提前把 jieba 搬进来。
func applySandhi(syls []syllable) {
	for i := range syls {
		cur := syls[i]
		if !isHan(cur.ch) {
			continue
		}
		tone := toneOf(cur.py)
		nextHan := i+1 < len(syls) && isHan(syls[i+1].ch)
		nextTone := byte(0)
		if nextHan {
			nextTone = toneOf(syls[i+1].py)
		}

		switch {
		case cur.py == "bu4" && nextHan && nextTone == '4':
			syls[i].py = "bu2"
		case cur.py == "yi1":
			// 第一 → 保持一声
			if i > 0 && syls[i-1].ch == '第' {
				continue
			}
			// 看一看 → 轻声
			if i > 0 && i+1 < len(syls) && syls[i-1].ch == syls[i+1].ch && isHan(syls[i+1].ch) {
				syls[i].py = "yi5"
				continue
			}
			if nextHan {
				if nextTone == '4' {
					syls[i].py = "yi2"
				} else {
					syls[i].py = "yi4"
				}
			}
		}
		_ = tone
	}
}

func toneOf(py string) byte {
	if py == "" {
		return 0
	}
	c := py[len(py)-1]
	if c >= '1' && c <= '5' {
		return c
	}
	return 0
}
