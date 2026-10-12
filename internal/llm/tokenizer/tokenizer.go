package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// Tokenizer 是 BGE-M3 的分词器。它只依赖 tokenizer.json **一个文件**：
// 词表、分数、那张映射表、空白规则、模板全在里面。
//
// 使用顺序是固定的（与 HF 的 `tokenizer.json` 声明一致，见各方法注释）：
//
//	按 added_tokens 切开 → 归一化(Precompiled+Replace) → 空白(Metaspace)
//	→ 切分(Unigram Viterbi) → 模板(<s> A </s>)
type Tokenizer struct {
	normalizer normalizerChain
	unigram    *unigram

	// pieces 按 id 存 piece 文本，调试与断言用
	pieces []string

	// addedTokens 按内容长度降序，用于"先切开"那一步
	addedTokens []addedToken

	// Metaspace 的两条声明
	replacement   string
	prependScheme string

	// 模板里那两个特殊 token 的 id
	bosID int32
	eosID int32
}

// addedToken 是词表里那些"整块识别、不参与切分"的特殊标记。
type addedToken struct {
	content string
	id      int32
	lstrip  bool
	rstrip  bool
}

// JSON 结构。只声明我们真正用到的字段——tokenizer.json 里还有一大堆（padding、
// truncation、decoder…），这个分词器一个都用不上。
type tjRoot struct {
	Normalizer    tjNormalizer    `json:"normalizer"`
	PreTokenizer  tjPreTokenizer  `json:"pre_tokenizer"`
	PostProcessor tjPostProcessor `json:"post_processor"`
	Model         tjModel         `json:"model"`
	AddedTokens   []tjAddedToken  `json:"added_tokens"`
}

type tjNormalizer struct {
	Type                string         `json:"type"`
	Normalizers         []tjNormalizer `json:"normalizers"`
	PrecompiledCharsmap string         `json:"precompiled_charsmap"`
	Pattern             *struct {
		String string `json:"String"`
		Regex  string `json:"Regex"`
	} `json:"pattern"`
	Content *string `json:"content"`
}

type tjPreTokenizer struct {
	Type          string `json:"type"`
	Replacement   string `json:"replacement"`
	PrependScheme string `json:"prepend_scheme"`
}

type tjPostProcessor struct {
	Type          string `json:"type"`
	SpecialTokens map[string]struct {
		IDs []int32 `json:"ids"`
	} `json:"special_tokens"`
}

type tjModel struct {
	Type         string              `json:"type"`
	UnkID        *int32              `json:"unk_id"`
	ByteFallback bool                `json:"byte_fallback"`
	FuseUnk      *bool               `json:"fuse_unk"`
	Vocab        [][]json.RawMessage `json:"vocab"`
}

type tjAddedToken struct {
	ID      int32  `json:"id"`
	Content string `json:"content"`
	Special bool   `json:"special"`
	Lstrip  bool   `json:"lstrip"`
	Rstrip  bool   `json:"rstrip"`
}

// New 从 tokenizer.json 建一个分词器。
func New(path string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 tokenizer.json 失败: %w", err)
	}
	return NewFromBytes(raw)
}

// NewFromBytes 同上，但从内容建（便于测试内嵌数据）。
func NewFromBytes(raw []byte) (*Tokenizer, error) {
	var tj tjRoot
	if err := json.Unmarshal(raw, &tj); err != nil {
		return nil, fmt.Errorf("解析 tokenizer.json 失败: %w", err)
	}
	if tj.Model.Type != "Unigram" {
		return nil, fmt.Errorf("这个分词器只实现了 Unigram，tokenizer.json 声明的是 %q", tj.Model.Type)
	}

	// 归一化：这个模型是 `Sequence[Precompiled, Replace]`。
	// 链里出现**任何没实现的类型就报错**，不静默跳过——少做一步归一化是最难查的那类 bug。
	norm, err := buildNormalizer(tj.Normalizer)
	if err != nil {
		return nil, err
	}

	// 空白规则：只支持 Metaspace（xlm-roberta 就是它）
	if tj.PreTokenizer.Type != "Metaspace" {
		return nil, fmt.Errorf("未实现的 pre_tokenizer: %q", tj.PreTokenizer.Type)
	}
	replacement := tj.PreTokenizer.Replacement
	if replacement == "" {
		replacement = "▁"
	}
	scheme := tj.PreTokenizer.PrependScheme
	if scheme == "" {
		scheme = "always"
	}

	// 词表：`[[piece, score], ...]`
	pieces := make([]string, 0, len(tj.Model.Vocab))
	scores := make([]float64, 0, len(tj.Model.Vocab))
	for i, entry := range tj.Model.Vocab {
		if len(entry) < 2 {
			return nil, fmt.Errorf("词表第 %d 项不是 [piece, score]", i)
		}
		var piece string
		var score float64
		if err := json.Unmarshal(entry[0], &piece); err != nil {
			return nil, fmt.Errorf("词表第 %d 项的 piece 不是字符串: %w", i, err)
		}
		if err := json.Unmarshal(entry[1], &score); err != nil {
			return nil, fmt.Errorf("词表第 %d 项的 score 不是数字: %w", i, err)
		}
		pieces = append(pieces, piece)
		scores = append(scores, score)
	}
	if len(pieces) == 0 {
		return nil, fmt.Errorf("词表是空的")
	}

	unkID := int32(0)
	if tj.Model.UnkID != nil {
		unkID = *tj.Model.UnkID
	}
	fuseUnk := true
	if tj.Model.FuseUnk != nil {
		fuseUnk = *tj.Model.FuseUnk
	}

	// added_tokens：既是"先切开"的清单，也是模板里 <s> / </s> 的 id 来源。
	// **不写死 0 和 2** —— 这几个编号是模型自带的，写死就会在别的模型上静默错。
	added := make([]addedToken, 0, len(tj.AddedTokens))
	var bos, eos int32 = -1, -1
	for _, at := range tj.AddedTokens {
		if at.Content == "" {
			continue
		}
		added = append(added, addedToken{content: at.Content, id: at.ID, lstrip: at.Lstrip, rstrip: at.Rstrip})
		switch at.Content {
		case "<s>":
			bos = at.ID
		case "</s>":
			eos = at.ID
		}
	}
	if bos < 0 || eos < 0 {
		return nil, fmt.Errorf("tokenizer.json 的 added_tokens 里没有 <s> 或 </s>（模板要用）")
	}
	// 长的排前面：短名字若是长名字的前缀，先匹配短的会把长的切坏
	sort.SliceStable(added, func(i, j int) bool { return len(added[i].content) > len(added[j].content) })

	return &Tokenizer{
		normalizer:    norm,
		unigram:       newUnigram(pieces, scores, unkID, fuseUnk, tj.Model.ByteFallback),
		pieces:        pieces,
		addedTokens:   added,
		replacement:   replacement,
		prependScheme: scheme,
		bosID:         bos,
		eosID:         eos,
	}, nil
}

// buildNormalizer 把 tokenizer.json 里声明的归一化链建出来。
func buildNormalizer(n tjNormalizer) (normalizerChain, error) {
	switch n.Type {
	case "Sequence":
		chain := make(normalizerChain, 0, len(n.Normalizers))
		for _, sub := range n.Normalizers {
			one, err := buildNormalizer(sub)
			if err != nil {
				return nil, err
			}
			chain = append(chain, one)
		}
		return chain, nil
	case "Precompiled":
		if n.PrecompiledCharsmap == "" {
			return nil, fmt.Errorf("Precompiled 归一化器没有 precompiled_charsmap")
		}
		pc, err := decodePrecompiled(n.PrecompiledCharsmap)
		if err != nil {
			return nil, err
		}
		return normalizerChain{pc}, nil
	case "Replace":
		if n.Pattern == nil {
			return nil, fmt.Errorf("Replace 归一化器没有 pattern")
		}
		one, err := newReplaceNormalizer(n.Pattern.String, n.Pattern.Regex, n.Content)
		if err != nil {
			return nil, err
		}
		return normalizerChain{one}, nil
	default:
		return nil, fmt.Errorf("未实现的 normalizer: %q", n.Type)
	}
}

// VocabSize 返回词表大小。
func (t *Tokenizer) VocabSize() int { return len(t.pieces) }

// IDToPiece 返回某个 id 对应的 piece 文本（调试与测试用）。
func (t *Tokenizer) IDToPiece(id int32) string {
	if id < 0 || int(id) >= len(t.pieces) {
		return ""
	}
	return t.pieces[id]
}

// Normalize 只做第一步（Precompiled 那张映射表）。单独暴露是为了**能和对齐基准逐字符对拍**：
// 归一化错了会让后面全错，而它的表现只是"某些标点落进 <unk>"，不看归一化结果很难定位。
func (t *Tokenizer) Normalize(text string) string {
	return t.normalizer.normalize(text)
}

// Metaspace 是第二步：把空格换成 `▁`，并在合适的时候于开头补一个 `▁`。
//
// **什么时候补、什么时候不补**（这条是拿 73 条基准试出来的，不是猜的）：
// 输入**已经以空格开头**时不补 —— 那个空格换成 `▁` 之后，位置和"补一个"完全重合，
// 再补就变成 `▁▁` 了（实测基准里 `"  前后都有空格  "` 只有一个 `▁`）。
// 换句话说它等价于 SentencePiece 的"先补一个空格、再折叠连续空格"，
// 只是 HF 把它拆成了 Replace(折叠) + Metaspace(替换)。
//
// ⚠️ 它**只替换空格**：`\t` / `\n` / `\r` 原样保留（基准里那两条也是这么定的）。
// 也不做"去掉首尾空格"——首尾空格会各自变成 `▁`，这在基准里同样得到确认。
func (t *Tokenizer) Metaspace(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(t.replacement))
	prepend := t.prependScheme == "always" || (t.prependScheme == "first" && s != "")
	if prepend && len(t.replacement) > 0 && !strings.HasPrefix(s, " ") {
		b.WriteString(t.replacement)
	}
	b.WriteString(strings.ReplaceAll(s, " ", t.replacement))
	return b.String()
}

// EncodeRaw 走完 added_tokens 切分 + 归一化 + Metaspace + Unigram，但**不加**模板的 <s>/</s>。
func (t *Tokenizer) EncodeRaw(text string) []int32 {
	if text == "" {
		return nil
	}
	return t.encodeSegments(text)
}

// Encode 是完整链路，返回**含 <s>/</s>** 的 id 序列 —— 这正是 ONNX 图要的输入。
func (t *Tokenizer) Encode(text string) []int32 {
	body := t.EncodeRaw(text)
	out := make([]int32, 0, len(body)+2)
	out = append(out, t.bosID)
	out = append(out, body...)
	out = append(out, t.eosID)
	return out
}

// encodePlain 是"文字部分"那一半：归一化 → Metaspace → Unigram。
func (t *Tokenizer) encodePlain(s string) []int32 {
	if s == "" {
		return nil
	}
	return t.unigram.encode(t.Metaspace(t.Normalize(s)))
}

// matchAdded 报告 text 的**开头**是不是一个 added_token。
//
// 按内容长度降序找：短名字若是长名字的前缀（例如 "<s>" 与 "<s>x"），
// 先匹配短的就会把长的切坏。
func (t *Tokenizer) matchAdded(text string) *addedToken {
	for i := range t.addedTokens {
		if strings.HasPrefix(text, t.addedTokens[i].content) {
			return &t.addedTokens[i]
		}
	}
	return nil
}

// encodeSegments 先按 added_tokens 把文本切开：命中的直接给它的 id，
// 中间的文字照常走那三步。
//
// 为什么必须切：**不切会改变结果**，而且差别不在那个 token 本身
// （`<s>` 本来就在词表里，Unigram 自己也能切出 id 0），而在**两侧的分词边界**。
// 这不是假设：对拍里真撞上了——operation.md 里那段 markdown 含字面量 `<s>`，
// 900 条里就它一条不一致。用户贴一段带 `<mask>` 的文本是同样的事。
//
// lstrip / rstrip 是 HF 的语义：`<mask>` 声明了 lstrip，它前面的空白会被吃掉。
func (t *Tokenizer) encodeSegments(text string) []int32 {
	var out []int32
	trimLeft := false
	start := 0
	for i := 0; i < len(text); {
		at := t.matchAdded(text[i:])
		if at == nil {
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
			continue
		}
		seg := text[start:i]
		if at.lstrip {
			seg = strings.TrimRight(seg, " ")
		}
		if trimLeft {
			seg = strings.TrimLeft(seg, " ")
		}
		out = append(out, t.encodePlain(seg)...)
		out = append(out, at.id)
		i += len(at.content)
		start = i
		trimLeft = at.rstrip
	}
	seg := text[start:]
	if trimLeft {
		seg = strings.TrimLeft(seg, " ")
	}
	return append(out, t.encodePlain(seg)...)
}
