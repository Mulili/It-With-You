package tokenizer

import (
	"fmt"
	"regexp"
)

// normalizer 是归一化链里的一环。
type normalizer interface {
	normalize(s string) string
}

// normalizerChain 按声明顺序依次施加。
//
// **顺序不能改**：这个模型是 `[Precompiled, Replace]` —— 先做字符映射、再折叠空格。
// 反过来的话，映射产生的空格就不会被折叠。
type normalizerChain []normalizer

func (c normalizerChain) normalize(s string) string {
	for _, n := range c {
		s = n.normalize(s)
	}
	return s
}

// replaceNormalizer 实现 HF 的 `Replace`：把匹配到的片段替换成给定内容（没给内容就是删掉）。
//
// 我们这个 tokenizer.json 用它来**折叠连续空格**（正则 ` {2,}` → 一个空格）——
// 这正是 SentencePiece 里 `remove_extra_whitespaces` 那件事在快速分词器里的等价形式。
//
// 只支持 `String` 与 `Regex` 两种 pattern，且正则用 Go 的 RE2 引擎。
// 对这条正则（` {2,}`）两者行为一致；将来若遇到 RE2 不支持的语法，**构造时会报错**
// 而不是悄悄跳过——"少做一步归一化"是最难查的那类 bug。
type replaceNormalizer struct {
	re      *regexp.Regexp
	content string
}

func (n *replaceNormalizer) normalize(s string) string {
	if n.re == nil {
		return s
	}
	return n.re.ReplaceAllString(s, n.content)
}

// newReplaceNormalizer 从 tokenizer.json 的声明建一个 Replace。
func newReplaceNormalizer(literal, regex string, content *string) (normalizer, error) {
	if regex == "" && literal == "" {
		return nil, fmt.Errorf("Replace 归一化器没有 pattern")
	}
	pattern := regex
	if pattern == "" {
		pattern = regexp.QuoteMeta(literal)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("Replace 的正则 %q 编译失败: %w", pattern, err)
	}
	n := &replaceNormalizer{re: re}
	if content != nil {
		// 显式给了内容就用它（含空串）。但 Go 的 ReplaceAllString 把 `$1` 当捕获组引用，
		// 而 HF 把内容当**字面量**——所以把 `$` 转义掉，
		// 免得"内容里恰好有个 $ 就被当成模板"这种静默偏差。
		n.content = escapeDollar(*content)
	}
	return n, nil
}

// escapeDollar 把 `$` 变成 `$$`：Go 的 ReplaceAll 里 `$$` 表示一个字面 `$`。
func escapeDollar(s string) string {
	b := make([]byte, 0, len(s)+4)
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			b = append(b, '$', '$')
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}
