package tokenizer

import (
	"math"
	"sort"
	"unicode/utf8"
)

// kUnkPenalty 是"未知字符"相对最低分 piece 的额外惩罚（上游 HF 的 K_UNK_PENALTY）。
//
// 分数本身是 log 概率（负数），再减 10 就是让它**远远更差**——于是只有在真的没有任何
// piece 能从当前位置开始时，Viterbi 才会走未知那条边。
const kUnkPenalty = 10.0

// unigram 是 SentencePiece 的 Unigram 模型（xlm-roberta / BGE-M3 用的就是它）。
//
// 编码 = 在"按位置铺开的图谱"上取总权最大的路径（Viterbi）：
// 每个位置、每条能从这里开始的 piece 都是一条候选边，边权是 piece 的 log 概率。
type unigram struct {
	// id 顺序的 piece 文本与分数（用于反向查表与调试）
	pieces []string
	scores []float64
	// pieceToID 是正向查找表：Viterbi 每个位置都要查它
	pieceToID map[string]int32
	// pieceRuneLens 是词表里出现过的 piece 长度（按 rune 数，升序去重）。
	//
	// 用它而不是"1..最长"逐个试：词表 25 万条，但**不同的长度只有几十种**，
	// 于是每个位置的候选查表次数从"最长 piece 的字节数"降到"几十次"。
	pieceRuneLens []int

	unkID        int32
	unkScore     float64
	fuseUnk      bool
	byteFallback bool
	// byteToID 只在 byteFallback 打开时才建：单字节 -> "<0xXX>" 的 id
	byteToID map[byte]int32
}

// newUnigram 用词表建模型。vocab 是 tokenizer.json 里 `model.vocab` 的 [[piece, score], ...]。
func newUnigram(pieces []string, scores []float64, unkID int32, fuseUnk, byteFallback bool) *unigram {
	u := &unigram{
		pieces:       pieces,
		scores:       scores,
		pieceToID:    make(map[string]int32, len(pieces)),
		unkID:        unkID,
		fuseUnk:      fuseUnk,
		byteFallback: byteFallback,
	}
	minScore := math.Inf(1)
	lens := make(map[int]struct{}, 32)
	for i, p := range pieces {
		if _, dup := u.pieceToID[p]; !dup {
			u.pieceToID[p] = int32(i)
		}
		if len(p) > 0 {
			lens[utf8.RuneCountInString(p)] = struct{}{}
		}
		if scores[i] < minScore {
			minScore = scores[i]
		}
	}
	u.pieceRuneLens = make([]int, 0, len(lens))
	for l := range lens {
		u.pieceRuneLens = append(u.pieceRuneLens, l)
	}
	sort.Ints(u.pieceRuneLens)
	if math.IsInf(minScore, 1) {
		minScore = 0
	}
	u.unkScore = minScore - kUnkPenalty

	if byteFallback {
		u.byteToID = make(map[byte]int32, 256)
		for b := 0; b < 256; b++ {
			name := "<0x" + hexUpper(byte(b)) + ">"
			if id, ok := u.pieceToID[name]; ok {
				u.byteToID[byte(b)] = id
			}
		}
	}
	return u
}

const hexDigits = "0123456789ABCDEF"

func hexUpper(b byte) string {
	return string([]byte{hexDigits[b>>4], hexDigits[b&0xF]})
}

// encode 把（已经过归一化与 Metaspace 的）文本切成 piece id。
//
// 逐条对齐上游 HF `unigram/model.rs` 的 populate_nodes + viterbi：
//   - 每个位置枚举所有能匹配上的 piece，边权取 piece 的分数
//   - **只有"不存在长度恰好等于一个字素的 piece"时**才补一条未知边
//     （上游是 `has_single_node` 这个判断，不是"完全没匹配上"）
//   - 未知边消耗**一个字素**，分数是 minScore - 10
func (u *unigram) encode(s string) []int32 {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	n := len(runes)

	negInf := math.Inf(-1)
	best := make([]float64, n+1)
	backID := make([]int32, n+1)
	backFrom := make([]int, n+1)
	for i := 1; i <= n; i++ {
		best[i] = negInf
	}

	for i := 0; i < n; i++ {
		if math.IsInf(best[i], -1) {
			continue
		}
		hasSingle := false
		for _, l := range u.pieceRuneLens {
			j := i + l
			if j > n {
				break // pieceRuneLens 升序，后面只会更长
			}
			id, ok := u.pieceToID[string(runes[i:j])]
			if !ok {
				continue
			}
			if l == 1 {
				hasSingle = true
			}
			if sc := best[i] + u.scores[id]; sc > best[j] {
				best[j] = sc
				backID[j] = id
				backFrom[j] = i
			}
		}
		if !hasSingle {
			if sc := best[i] + u.unkScore; sc > best[i+1] {
				best[i+1] = sc
				backID[i+1] = u.unkID
				backFrom[i+1] = i
			}
		}
	}

	// 回溯
	ids := make([]int32, 0, n)
	for pos := n; pos > 0; {
		ids = append(ids, backID[pos])
		pos = backFrom[pos]
	}
	// 反转
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}

	if u.fuseUnk {
		ids = fuseUnkIDs(ids, u.unkID)
	}
	return ids
}

// fuseUnkIDs 把连续的未知字符合并成一个 <unk>。
//
// 上游默认 fuse_unk=true，而这件事有实际影响：不合并的话一句生僻字会变成一长串 <unk>，
// 把向量的语义完全冲掉。
func fuseUnkIDs(ids []int32, unkID int32) []int32 {
	if unkID < 0 {
		return ids
	}
	out := ids[:0]
	for i, id := range ids {
		if i > 0 && id == unkID && out[len(out)-1] == unkID {
			continue
		}
		out = append(out, id)
	}
	return out
}
