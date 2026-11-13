package tts

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	ort "github.com/yalue/onnxruntime_go"

	"agent-for-you-love/internal/tts/sovits"
)

// BERT 的固定 token。
const (
	bertCLS = 101
	bertSEP = 102
	bertUNK = 100
)

// Bert 是中文 RoBERTa（chinese-roberta-wwm-ext-large）的一条会话 + 它的词表。
//
// 它提供的是 **音素级** 特征：输入里除了 input_ids / attention_mask，还有一个 `repeats`，
// 输出直接是 [总音素数, 1024]。这是 GPT-SoVITS 那份导出图的特别之处——"每个字重复几次"
// 由调用方给（中文一个字 2 个音素、标点 1 个），省掉 Go 侧再做一次对齐。
//
// 它决定的是**语调**：轻重音、停连、句末走势。没有它时 text_bert 全零，
// 模型只剩自回归先验 + 参考音频，输出会明显"一字一顿、平铺直叙"。
type Bert struct {
	session *sovits.Session
	vocab   map[string]int64
}

// LoadBert 打开 BERT 会话并读入词表。
func LoadBert(modelPath, vocabPath string, threads int) (*Bert, error) {
	vocab, err := loadBertVocab(vocabPath)
	if err != nil {
		return nil, err
	}
	s, err := sovits.OpenOne(modelPath, threads)
	if err != nil {
		return nil, err
	}
	return &Bert{session: s, vocab: vocab}, nil
}

// Close 释放会话。
func (b *Bert) Close() error {
	if b == nil || b.session == nil {
		return nil
	}
	err := b.session.Destroy()
	b.session = nil
	return err
}

// loadBertVocab 读 BERT 的 vocab.txt：**行号就是 id**。
func loadBertVocab(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 BERT 词表失败：%w", err)
	}
	defer f.Close()

	vocab := make(map[string]int64, 22000)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	idx := int64(0)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		// 第一行可能带 UTF-8 BOM
		line = strings.TrimPrefix(line, "\ufeff")
		if _, dup := vocab[line]; !dup && line != "" {
			vocab[line] = idx
		}
		idx++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取 BERT 词表失败：%w", err)
	}
	if len(vocab) == 0 {
		return nil, fmt.Errorf("%s 是空的", path)
	}
	return vocab, nil
}

// idOf 把一个字符映射成 token id。查不到就给 [UNK]。
//
// 为什么是**逐字符**而不是完整 WordPiece：中文里一个字基本就是一个 token，
// 而逐字符切分能保证"字符 ↔ token ↔ 音素数"三者一一对应——这是 repeats 对齐的前提。
// 完整分词一旦把某个字切成多段，对齐就会错位，而错位的症状是整句语调怪异，很难查。
func (b *Bert) idOf(r rune) int64 {
	if r >= 'A' && r <= 'Z' {
		r = r - 'A' + 'a' // 与 do_lower_case=true 一致
	}
	if id, ok := b.vocab[string(r)]; ok {
		return id
	}
	return bertUNK
}

// PhoneFeatures 返回 [sum(repeats), 1024] 的音素级特征（行主序 float32）。
//
// text 必须是**归一化之后**的文本（与交给 Phones 的那一份逐字一致）；
// repeats[i] 是第 i 个字符贡献的音素数（0 表示这个字没有可用音素，直接跳过）。
func (b *Bert) PhoneFeatures(ctx context.Context, text string, repeats []int) ([]float32, error) {
	if b == nil || b.session == nil {
		return nil, fmt.Errorf("BERT 未加载")
	}
	runes := []rune(text)
	if len(runes) != len(repeats) {
		return nil, fmt.Errorf("BERT 对齐失败：文本 %d 字，repeats %d 个", len(runes), len(repeats))
	}

	ids := make([]int64, 0, len(runes)+2)
	ids = append(ids, bertCLS)
	reps := make([]int64, 0, len(runes))
	total := 0
	for i, r := range runes {
		if repeats[i] <= 0 {
			continue
		}
		ids = append(ids, b.idOf(r))
		reps = append(reps, int64(repeats[i]))
		total += repeats[i]
	}
	ids = append(ids, bertSEP)
	if total == 0 || len(reps) == 0 {
		return nil, fmt.Errorf("这段文本没有可对齐的音素")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	inputIDs := ids
	mask := make([]int64, len(ids))
	for i := range mask {
		mask[i] = 1
	}

	inIDs, err := ort.NewTensor(ort.NewShape(1, int64(len(ids))), inputIDs)
	if err != nil {
		return nil, fmt.Errorf("构造 input_ids 失败：%w", err)
	}
	defer inIDs.Destroy()
	inMask, err := ort.NewTensor(ort.NewShape(1, int64(len(ids))), mask)
	if err != nil {
		return nil, fmt.Errorf("构造 attention_mask 失败：%w", err)
	}
	defer inMask.Destroy()
	inReps, err := ort.NewTensor(ort.NewShape(int64(len(reps))), reps)
	if err != nil {
		return nil, fmt.Errorf("构造 repeats 失败：%w", err)
	}
	defer inReps.Destroy()

	in, err := feed(b.session, map[string]ort.Value{
		"input_ids": inIDs, "attention_mask": inMask, "repeats": inReps,
	})
	if err != nil {
		return nil, err
	}
	// 输出有两条路径，取决于模型精度：
	//   - float32（int8 量化后的图）：让 ORT 自动分配，直接读 Tensor[float32]；
	//   - float16（原始导出）：**必须自己预分配**。binding 对未知元素类型的自动分配
	//     只按 1 字节/元素算，ORT 会报 "not enough space: expected 86016, got 43008"。
	fp16Out := len(b.session.OutputTypes) > 0 &&
		b.session.OutputTypes[0] == ort.TensorElementDataTypeFloat16

	var outs []ort.Value
	var outTensor *ort.CustomDataTensor
	if fp16Out {
		outTensor, err = ort.NewCustomDataTensor(
			ort.NewShape(int64(total), bertDim), make([]byte, total*bertDim*2),
			ort.TensorElementDataTypeFloat16)
		if err != nil {
			return nil, fmt.Errorf("构造 BERT 输出张量失败：%w", err)
		}
		defer outTensor.Destroy()
		outs = []ort.Value{outTensor}
	} else {
		outs = make([]ort.Value, len(b.session.Outputs))
	}

	started := time.Now()
	if err := b.session.Run(in, outs); err != nil {
		return nil, fmt.Errorf("BERT 推理失败：%w", err)
	}
	if os.Getenv("TTS_DEBUG") != "" {
		log.Printf("tts-debug: bert %d 音素耗时 %dms", total, time.Since(started).Milliseconds())
	}

	var out []float32
	if fp16Out {
		raw := outTensor.GetData()
		if len(raw) != total*bertDim*2 {
			return nil, fmt.Errorf("BERT 输出尺寸不对：%d 字节，期望 %d（%d 音素 × %d × 2）",
				len(raw), total*bertDim*2, total, bertDim)
		}
		out = make([]float32, 0, total*bertDim)
		for i := 0; i+1 < len(raw); i += 2 {
			out = append(out, fp16ToFloat32(uint16(raw[i])|uint16(raw[i+1])<<8))
		}
	} else {
		t, ok := outs[0].(*ort.Tensor[float32])
		if !ok {
			destroyValues(outs)
			return nil, fmt.Errorf("BERT 输出类型不对：%T（期望 float32）", outs[0])
		}
		data := t.GetData()
		if len(data) != total*bertDim {
			destroyValues(outs)
			return nil, fmt.Errorf("BERT 输出元素数不对：%d，期望 %d（%d 音素 × %d）",
				len(data), total*bertDim, total, bertDim)
		}
		out = append([]float32(nil), data...)
		destroyValues(outs)
	}
	// 全零说明写回没生效（binding 对 CustomDataTensor 的写回行为依版本而变）。
	// 与其把零向量当"正常特征"喂下去，不如明确报错、退回零 BERT。
	nonZero := false
	for _, v := range out {
		if v != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		return nil, fmt.Errorf("BERT 输出全为 0（半精度张量没有写回）")
	}
	return out, nil
}

// fp16ToFloat32 把 IEEE 754 半精度展开成单精度。
//
// 标准做法：符号位左移到 31，尾数左移 13，指数偏移从 15 换成 127（差值 112）。
// 指数全 0 是次正规数（要先把尾数规格化），全 1 是 Inf/NaN。
func fp16ToFloat32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h) & 0x3ff

	var bits uint32
	switch exp {
	case 0:
		if mant == 0 {
			bits = sign // ±0
			break
		}
		// 次正规数：找到最高位，再规格化
		e := uint32(127 - 15 + 1)
		for mant&0x400 == 0 {
			mant <<= 1
			e--
		}
		bits = sign | (e << 23) | ((mant & 0x3ff) << 13)
	case 0x1f:
		bits = sign | 0x7f800000 | (mant << 13) // Inf / NaN
	default:
		bits = sign | ((exp + 112) << 23) | (mant << 13)
	}
	return math.Float32frombits(bits)
}
