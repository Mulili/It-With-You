// Package tokenizer 是 **BGE-M3（xlm-roberta + Unigram）的纯 Go 分词器**。
//
// 为什么要自己写：我们要把嵌入从 Ollama 搬进进程内（见 operation.md 的
// 「嵌入运行时替换」一节），而 ONNX 图的输入是 token id —— 分词这一步必须自己解决。
// 调研过三条现成的路，都不能用：
//   - `github.com/sugarme/tokenizer`：`NewTokenizerFromFile` 是 TODO 空壳，读不了 tokenizer.json
//   - `gomlx/go-huggingface` 的 `hftokenizer`：纯 Go，但**不支持 Precompiled 归一化**
//     （那张全角→半角的表被整块跳过），而且 Unigram 的 Viterbi 分段与官方不一致
//     （实测：三条里三条对、另一些句子分段不同），属于"时对时错"，最不能用
//   - `daulet/tokenizers`：cgo 链一个 Rust 静态库，作者自己在源码里写着打包未解决
//
// 而分词错了**不会报错**，只会让向量悄悄偏掉。所以这里的做法是：
// 严格照上游实现（见各文件的注释指向），并用官方 `tokenizers` 产出的基准做**逐 token 对拍**。
package tokenizer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// maxTrieResults 与上游一致：一次 commonPrefixSearch 最多取 32 个结果。
//
// 上游（sentencepiece/normalizer.h）把它定为常量，注释说构建时会确保"共享前缀数"
// 小于它；超过的部分上游是直接截断的，这里同样截断。
const maxTrieResults = 32

// precompiled 就是 SentencePiece 那张 `precompiled_charsmap`：
// 全角→半角、全角数字→ASCII、兼容汉字归一这类映射全在里面。
type precompiled struct {
	// units 是 Darts 双数组，每项 4 字节（小端）。
	units []uint32
	// normalized 是所有替换串的拼接，**每个以 '\0' 结尾**；
	// trie 里存的值是它内部的字节偏移。这一点来自上游 builder.cc 的 CompileCharsMap：
	// 它把相同的目标串合并去重，记下位置作为 trie 的值。
	normalized []byte
}

// trieMatch 是一次前缀匹配：消耗了 length 个字节，替换串在 normalized 里的偏移是 value。
type trieMatch struct {
	value  uint32
	length int
}

// decodePrecompiled 解开 blob。布局（上游 normalizer.cc 的 DecodePrecompiledCharsMap）：
//
//	[uint32 小端: trie 字节数][trie 字节][归一化串表]
func decodePrecompiled(b64 string) (*precompiled, error) {
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("解 precompiled_charsmap 的 base64 失败: %w", err)
	}
	if len(blob) <= 4 {
		return nil, fmt.Errorf("precompiled_charsmap 太短（%d 字节）", len(blob))
	}
	trieSize := int(binary.LittleEndian.Uint32(blob[:4]))
	if trieSize <= 0 || trieSize > len(blob)-4 {
		return nil, fmt.Errorf("precompiled_charsmap 里的 trie 长度不合理: %d", trieSize)
	}
	if trieSize%4 != 0 {
		return nil, fmt.Errorf("precompiled_charsmap 的 trie 长度不是 4 的倍数: %d", trieSize)
	}
	norm := blob[4+trieSize:]
	if len(norm) == 0 || norm[len(norm)-1] != 0 {
		return nil, fmt.Errorf("precompiled_charsmap 的归一化串表没有以 \\0 结尾")
	}
	units := make([]uint32, trieSize/4)
	for i := range units {
		units[i] = binary.LittleEndian.Uint32(blob[4+i*4 : 4+i*4+4])
	}
	return &precompiled{units: units, normalized: norm}, nil
}

// Darts 双数组的位运算。逐字照抄上游 third_party/darts_clone/darts.h 的 DoubleArrayUnit。
//
// 为什么要写死这些移位常量：这是上游序列化格式的一部分，改了就读不出别人的表。
func unitOffset(u uint32) uint32 { return (u >> 10) << ((u & (1 << 9)) >> 6) }
func unitHasLeaf(u uint32) bool  { return (u>>8)&1 == 1 }
func unitValue(u uint32) uint32  { return u & ((1 << 31) - 1) }
func unitLabel(u uint32) uint32  { return u & ((1 << 31) | 0xFF) }

// prefixMatches 找出 key 的所有**前缀**匹配。逻辑与上游 commonPrefixSearch 一致：
// 每前进一个字节就把 label 与它比对，命中叶子就记一条。
func (p *precompiled) prefixMatches(key []byte) []trieMatch {
	if len(p.units) == 0 {
		return nil
	}
	out := make([]trieMatch, 0, 4)
	nodePos := uint32(0)
	unit := p.units[0]
	nodePos ^= unitOffset(unit)

	for i := 0; i < len(key); i++ {
		nodePos ^= uint32(key[i])
		if int(nodePos) >= len(p.units) { // 数据坏了也要安全退出，不 panic
			return out
		}
		unit = p.units[nodePos]
		if unitLabel(unit) != uint32(key[i]) {
			return out
		}
		nodePos ^= unitOffset(unit)
		if unitHasLeaf(unit) {
			if len(out) < maxTrieResults && int(nodePos) < len(p.units) {
				out = append(out, trieMatch{value: unitValue(p.units[nodePos]), length: i + 1})
			}
		}
	}
	return out
}

// longestReplacement 返回**最长前缀**匹配到的替换串。
//
// ⚠️ 它**不要求整段都被覆盖** —— 调用方拿它替换掉**整个字素簇**。
// 这一点是实测出来的，不是推测：全角 `ｂ` + U+0308（分音符）在基准里输出的是 `b`，
// 那个分音符**跟着整簇一起没了**。所以"只替换匹配到的那几个字节"是错的。
func (p *precompiled) longestReplacement(gr string) (string, bool) {
	matches := p.prefixMatches([]byte(gr))
	var best *trieMatch
	for i := range matches {
		if best == nil || matches[i].length > best.length {
			best = &matches[i]
		}
	}
	if best == nil {
		return "", false
	}
	off := int(best.value)
	if off >= len(p.normalized) {
		return "", false
	}
	rest := p.normalized[off:]
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = rest[:i]
	}
	return string(rest), true
}

// 这几个字符**不**跟前面的字符并成同一个字素簇（而组合符会）。
//
// 依据是实测：`"Y" + U+200D` 在基准里输出 `Y` + 空格（说明 ZWJ 被当成独立一段查表），
// 而 `"ａ" + U+0301` 输出 `á`（说明组合符是并进簇里、按整簇查表的）。
// Go 的 uniseg 按 UAX#29 把 ZWJ 和变体选择符都并进簇，所以这里要**再把它们拆出来**，
// 否则 `"３" + U+FE0F` 会整簇查不到规则、白丢一次映射。
func splitsFromCluster(r rune) bool {
	if r == 0x200D { // ZERO WIDTH JOINER
		return true
	}
	if r == 0xFE0F || r == 0xFE0E { // 变体选择符（表情/文字呈现）
		return true
	}
	return r >= 0xE0100 && r <= 0xE01EF // 补充变体选择符
}

// normalize 逐段替换字符。三步判断，全部来自实测（见 operation.md 的对拍记录）：
//
//  1. 簇里含 ZWJ / 变体选择符 → **逐 rune** 处理（那几个字符不并簇）
//  2. 簇是 6 字节及以上且含多个 rune → 逐 rune 处理
//     （上游那条“看着很怪”的 `len() < 6` 规则：≥6 字节本该整块跳过，
//     但实测 `"３" + U+FE0F` 里的 `３` 仍然被映射，说明那边根本不会把这两者并成 6 字节的簇）
//  3. 其余 → 整簇查一次表；命中就用**最长前缀规则的值**替换**整簇**（尾部随之丢弃），
//     未命中就原样保留
func (p *precompiled) normalize(s string) string {
	if p == nil || s == "" {
		return s
	}
	g := uniseg.NewGraphemes(s)
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for g.Next() {
		gr := g.Str()

		split := len(gr) >= 6 && utf8.RuneCountInString(gr) > 1
		if !split {
			for _, r := range gr {
				if splitsFromCluster(r) {
					split = true
					break
				}
			}
		}

		if split {
			for _, r := range gr {
				one := string(r)
				if rep, ok := p.longestReplacement(one); ok {
					b.WriteString(rep)
					changed = true
					continue
				}
				b.WriteString(one)
			}
			continue
		}

		if rep, ok := p.longestReplacement(gr); ok {
			b.WriteString(rep)
			changed = true
			continue
		}
		b.WriteString(gr)
	}
	if !changed {
		return s
	}
	return b.String()
}
