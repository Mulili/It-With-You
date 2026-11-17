package text

import (
	"regexp"
	"strings"
)

// 朗读前的 markdown 清洗。
//
// 为什么必须有这一步：模型看到的是 markdown 源文，而**朗读不该听见排版符号**。
// 之前的做法是在 Normalize 里"把不认识的字符一个个丢掉"——后果是段落边界、列表边界
// 一起消失，"第 1 点……第 2 点……"连成一整句，听感就是"某一段直接连读"。
//
// 这里的原则是**先按结构改写，再交给 Normalize 丢符号**：
//   - 粗体/斜体/行内代码：去掉标记留文字（"**重要**"读作"重要"）
//   - 标题/列表/引用：去掉行首标记，行尾换成停顿
//   - 链接：留文字去地址；图片：整条丢掉（读 URL 毫无意义）
//   - 代码块：整段丢掉（读代码比不说话更糟）
//
// ⚠️ 已知边界：流式回复里，跨两个 chunk 的代码围栏可能被漏掉一段。这里不做状态机——
// 桌宠的回复里代码块极少，为它引入跨轮状态不划算。
var (
	reFence   = regexp.MustCompile("(?s)```[\\s\\S]*?```")
	reFenceLn = regexp.MustCompile("(?s)```[\\s\\S]*$") // 还没闭合的（流式截断）
	reImage   = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	reLink    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	reHeading = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]*`)
	reRule    = regexp.MustCompile(`(?m)^[ \t]{0,3}([-*_])(?:[ \t]*[-*_]){2,}[ \t]*$`)
	reTask    = regexp.MustCompile(`(?m)^[ \t]{0,3}[-*+][ \t]+\[[ xX]\][ \t]*`)
	reBullet  = regexp.MustCompile(`(?m)^[ \t]{0,3}[-*+][ \t]+`)
	reOrdered = regexp.MustCompile(`(?m)^[ \t]{0,3}\d{1,2}[.)][ \t]+`)
	reQuote   = regexp.MustCompile(`(?m)^[ \t]{0,3}>[ \t]?`)
	reSpace   = regexp.MustCompile(`[ \t]{2,}`)
)

// markdownMarks 是"只影响排版、不该被读出来"的字符。
// 用 Replacer 而不是逐字符判断：它们出现在正文里的概率极低，一并删掉最省事。
var markdownMarks = strings.NewReplacer(
	"**", "", "__", "", "~~", "", "*", "", "_", "", "`", "",
)

// CleanForSpeech 把一段 markdown 源文改写成"适合朗读的句子"。
//
// 它会**保留/补齐标点**（段落之间补句号、行内换行补逗号），因为停顿时长是按标点分级的
// （见 PauseAfterMs）——如果这里把边界吞掉，后面再好的停顿策略也没有落点。
func CleanForSpeech(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")

	// 顺序有讲究：代码块要走，图片/链接要先于"拆括号"处理，
	// 水平线要先于列表（`---` 也会被 `[-*+] ` 之外的形式误伤）。
	s = reFence.ReplaceAllString(s, "")
	s = reFenceLn.ReplaceAllString(s, "")
	s = reImage.ReplaceAllString(s, "")
	s = reLink.ReplaceAllString(s, "$1")
	s = reRule.ReplaceAllString(s, "")
	s = reHeading.ReplaceAllString(s, "")
	s = reTask.ReplaceAllString(s, "")
	s = reBullet.ReplaceAllString(s, "")
	s = reOrdered.ReplaceAllString(s, "")
	s = reQuote.ReplaceAllString(s, "")
	s = markdownMarks.Replace(s)

	// 表格竖线：读成"竖线"没有意义，换成停顿更自然
	s = strings.ReplaceAll(s, "|", "，")

	s = newlinesToPunct(s)
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// newlinesToPunct 把换行改写成停顿：单换行 → 逗号，空行（段落）→ 句号。
//
// 前面已经是标点时不补——否则"你好。\n"会变成"你好。。"，
// 而连续同标点会在 Normalize 里被压掉，等于白做。
func newlinesToPunct(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(rs))
	lastWritten := rune(0)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\n' {
			b.WriteRune(rs[i])
			if rs[i] != ' ' && rs[i] != '\t' {
				lastWritten = rs[i]
			}
			continue
		}
		j := i
		for j < len(rs) && (rs[j] == '\n' || rs[j] == ' ' || rs[j] == '\t') {
			j++
		}
		marker := '，'
		if j-i >= 2 {
			marker = '。'
		}
		if lastWritten != 0 && !pausePunct[lastWritten] {
			b.WriteRune(marker)
			lastWritten = marker
		}
		i = j - 1
	}
	return b.String()
}

// pausePunct 是"本身已经带停顿"的标点，后面不再补。
var pausePunct = map[rune]bool{
	'。': true, '！': true, '？': true, '…': true, '，': true, '、': true,
	'；': true, '：': true, '—': true, '－': true, '–': true,
	'!': true, '?': true, '.': true, ',': true, ';': true, ':': true,
}
