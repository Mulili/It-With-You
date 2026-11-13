package tts

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"

	ort "github.com/yalue/onnxruntime_go"

	"agent-for-you-love/internal/tts/sovits"
	"agent-for-you-love/internal/tts/text"
)

const (
	bertDim       = 1024 // ref_bert / text_bert 的特征维度（零 BERT 路径也要给满）
	refSampleRate = 16000
	maxSteps      = 500  // 自回归上限，与 GENIE 的 range(0, 500) 一致
	eosToken      = 1024 // >= 1024 视为非法 / EOS（方案 §7.2）
)

// RefFeatures 是一个音色在某个情绪下的"参考侧"张量。
//
// 它只跟"参考音频 + 它的文本"有关，与要说的内容无关；所以每个 (音色, 情绪) 算一次就该缓存，
// 每次请求只算文本侧——这是首延迟能压下来的关键（方案 §7.2）。
type RefFeatures struct {
	Seq        *ort.Tensor[int64]   // ref_seq [1, R]
	Bert       *ort.Tensor[float32] // [R, 1024]，零 BERT 路径就是全零
	SSL        *ort.Tensor[float32] // hubert 输出 [1, 768, L]
	GE         *ort.Tensor[float32] // prompt_encoder 输出
	GEAdvanced *ort.Tensor[float32]
}

// Close 释放参考侧张量。
func (r *RefFeatures) Close() {
	if r == nil {
		return
	}
	destroyValues([]ort.Value{r.Seq, r.Bert, r.SSL, r.GE, r.GEAdvanced})
	r.Seq, r.Bert, r.SSL, r.GE, r.GEAdvanced = nil, nil, nil, nil, nil
}

// PrepareReference 算出一个 (音色, 情绪) 的参考侧张量。
//
// refText 是参考音频对应的文本，走与正文同一条文本前端；算出来的 ref_seq 与 ssl 一起
// 决定"这个音色听起来是谁"。
func (e *Engine) PrepareReference(ctx context.Context, wavPath, refText string) (*RefFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pcm, rate, err := DecodeWAV(wavPath)
	if err != nil {
		return nil, fmt.Errorf("读参考音频失败：%w", err)
	}
	audio32k, err := Resample(pcm, rate, e.backbone.SampleRate)
	if err != nil {
		return nil, fmt.Errorf("参考音频重采样到 %d 失败：%w", e.backbone.SampleRate, err)
	}
	audio16k, err := Resample(pcm, rate, refSampleRate)
	if err != nil {
		return nil, fmt.Errorf("参考音频重采样到 %d 失败：%w", refSampleRate, err)
	}

	// hubert → ssl_content（16k）
	ssl, err := e.runHuBERT(audio16k)
	if err != nil {
		return nil, err
	}
	// speaker → sv_emb（16k）
	sv, err := e.runSpeaker(audio16k)
	if err != nil {
		ssl.Destroy()
		return nil, err
	}
	// prompt_encoder(ref_audio 32k, sv_emb) → ge / ge_advanced
	ge, geAdv, err := e.runPromptEncoder(audio32k, sv)
	sv.Destroy()
	if err != nil {
		ssl.Destroy()
		return nil, err
	}

	phones, counts := e.frontend.PhonesWithCounts(refText)
	kept, keptCounts, _ := e.filterPhones(phones, counts)
	if len(kept) == 0 {
		ssl.Destroy()
		ge.Destroy()
		geAdv.Destroy()
		return nil, errors.New("参考文本没有可用音素（文本前端没认出任何字）")
	}
	ids, err := e.symbols.IDs(kept)
	if err != nil {
		ssl.Destroy()
		ge.Destroy()
		geAdv.Destroy()
		return nil, err
	}
	seq, err := ort.NewTensor(ort.NewShape(1, int64(len(ids))), ids)
	if err != nil {
		ssl.Destroy()
		ge.Destroy()
		geAdv.Destroy()
		return nil, fmt.Errorf("构造 ref_seq 失败：%w", err)
	}
	refBertData := e.bertDataLocked(ctx, text.Normalize(refText), keptCounts, len(ids))
	bert, err := ort.NewTensor(ort.NewShape(int64(len(ids)), bertDim), refBertData)
	if err != nil {
		seq.Destroy()
		ssl.Destroy()
		ge.Destroy()
		geAdv.Destroy()
		return nil, fmt.Errorf("构造 ref_bert 失败：%w", err)
	}
	return &RefFeatures{Seq: seq, Bert: bert, SSL: ssl, GE: ge, GEAdvanced: geAdv}, nil
}

// bertDataLocked 算出 [rows, 1024] 的音素级 BERT 特征；拿不到就返回全零。
//
// 全零不是"降级提示"，而是一条**曾经唯一**的路径：BERT 缺席时模型也能出声，
// 只是语调平。所以这里不返回错误——把"没有 BERT"和"这句合成失败"分开。
// 调用方必须持有 e.mu。
func (e *Engine) bertDataLocked(ctx context.Context, norm string, counts []int, rows int) []float32 {
	out := make([]float32, rows*bertDim)
	b := e.bertLocked()
	if b == nil {
		return out
	}
	feat, err := b.PhoneFeatures(ctx, norm, counts)
	if err != nil {
		e.warnBertOnce(err)
		return out
	}
	if len(feat) != len(feat)/bertDim*bertDim || len(feat)%bertDim != 0 {
		e.warnBertOnce(fmt.Errorf("特征长度 %d 不是 %d 的整数倍", len(feat), bertDim))
		return out
	}
	// rows 应当等于 len(feat)/bertDim（都按音素数算）。对不上说明对齐出错，
	// 宁可退回零向量，也不要把错位的数据喂给模型——那会产出怪音而不是变平。
	if len(feat)/bertDim != rows {
		e.warnBertOnce(fmt.Errorf("BERT 音素数 %d 与音素序列 %d 不一致", len(feat)/bertDim, rows))
		return out
	}
	copy(out, feat)
	return out
}

// Synthesize 把一条音素序列合成成 PCM（float32、单声道、voice.SampleRate）。
//
// 流程与 GENIE 的 tts()/t2s_cpu() 一一对应，包括它的三处细节：
// 文本前补一个'。'、EOS 取**最后一个**越界位置、循环结束后把最后一个 token 置 0。
//
// norm 是**归一化之后**的文本（与 phones 同源），counts 是每个字的音素数——
// 两者一起用于 text_bert；传空则表示这段没有 BERT 可用（例如直接从音素构造的测试）。
func (e *Engine) Synthesize(ctx context.Context, ref *RefFeatures, norm string, phones []string, counts []int) ([]float32, error) {
	if ref == nil {
		return nil, errors.New("参考特征为空：先调用 PrepareReference")
	}

	// GENIE 在文本前补一个'。'，中文前端归一化后会把它变成 "." —— 这里补的必须是
	// 归一化之后的符号，否则音素表里查不到（符号表里没有'。'，只有 "."）。
	// 作用同样是"防止漏第一句"（方案 §7.2）。
	ids, err := e.symbols.IDs(append([]string{"."}, phones...))
	if err != nil {
		return nil, err
	}
	textSeq, err := ort.NewTensor(ort.NewShape(1, int64(len(ids))), ids)
	if err != nil {
		return nil, fmt.Errorf("构造 text_seq 失败：%w", err)
	}
	defer textSeq.Destroy()

	// text_bert 必须是**音素级**的：一行对应一个音素，长度与 text_seq 严格相等。
	// 第一行留给上面补的那个 "."，所以把它前面的位置空出来（零向量），
	// 后面接 BERT 给出的 [音素数, 1024]。
	//
	// 这一段就是"语气平不平"的分水岭：全零时模型没有词义上下文，
	// 只能靠自回归先验把每个音素均匀地读出来。
	textBertData := make([]float32, len(ids)*bertDim)
	if len(counts) > 0 {
		feat := e.bertDataLocked(ctx, norm, counts, len(phones))
		copy(textBertData[bertDim:], feat)
	}
	textBert, err := ort.NewTensor(ort.NewShape(int64(len(ids)), bertDim), textBertData)
	if err != nil {
		return nil, fmt.Errorf("构造 text_bert 失败：%w", err)
	}
	defer textBert.Destroy()

	// 1) encoder
	encIn, err := feed(e.set.Encoder, map[string]ort.Value{
		"ref_seq": ref.Seq, "text_seq": textSeq,
		"ref_bert": ref.Bert, "text_bert": textBert,
		"ssl_content": ref.SSL,
	})
	if err != nil {
		return nil, err
	}
	encOut, err := runSession(e.set.Encoder, encIn)
	if err != nil {
		return nil, fmt.Errorf("t2s_encoder 失败：%w", err)
	}

	// 2) first stage decoder：一次给出初始 KV-Cache
	fsIn, err := feed(e.set.FirstStageDec, map[string]ort.Value{
		"x": encOut[0], "prompts": encOut[1],
	})
	if err != nil {
		destroyValues(encOut)
		return nil, err
	}
	cur, err := runSession(e.set.FirstStageDec, fsIn)
	destroyValues(encOut)
	if err != nil {
		return nil, fmt.Errorf("t2s_fsdec 失败：%w", err)
	}
	defer func() { destroyValues(cur) }()

	// fsdec 给出的 y 里**已经包含参考音频的 prompt token**（长度 = prompts+1），
	// 后面每一步 sdec 再往后追加一个。所以要记住这个边界，最后只取边界之后的。
	initLen := len(cur[0].(*ort.Tensor[int64]).GetData())

	// 3) 自回归：上一轮的全部输出直接作为下一轮输入
	runs := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if runs >= maxSteps {
			break
		}
		if len(cur) != len(e.set.StageDec.Inputs) {
			return nil, fmt.Errorf("sdec 输入数不匹配：上一轮给了 %d 个，模型要 %d 个",
				len(cur), len(e.set.StageDec.Inputs))
		}
		out, err := runSession(e.set.StageDec, cur)
		if err != nil {
			return nil, fmt.Errorf("t2s_sdec 第 %d 步失败：%w", runs, err)
		}
		destroyValues(cur)
		cur = out
		runs++

		// 模型的 stop_condition_tensor 是标量，binding 建不出零维张量（见 sovits.openOne），
		// 所以这里用等价判断：最新 token 越过词表上限就是该停了（方案 §7.2 的 EOS 规则）。
		if y := out[0].(*ort.Tensor[int64]).GetData(); len(y) > 0 {
			last := y[len(y)-1]
			if os.Getenv("TTS_DEBUG") != "" {
				log.Printf("tts-debug: step=%d ylen=%d last=%d", runs, len(y), last)
			}
			if last >= eosToken {
				break
			}
		}
	}

	// 4) 取语义 token。**必须只取 fsdec 之后新生成的那一段**：
	// y 的前 initLen 个是参考音频的 prompt token，把它们喂给 vocoder 就会把
	// 参考音频（"示例那句话"）一起念出来——这是实测踩到的 bug，不是理论风险。
	yData := cur[0].(*ort.Tensor[int64]).GetData()
	if len(yData) == 0 {
		return nil, errors.New("sdec 没有产出 token")
	}
	yData[len(yData)-1] = 0
	tokens := semanticSlice(yData, initLen)
	if len(tokens) == 0 {
		return nil, errors.New("sdec 没有生成任何 token")
	}

	if os.Getenv("TTS_DEBUG") != "" {
		log.Printf("tts-debug: initLen=%d runs=%d ylen=%d tokens=%d",
			initLen, runs, len(yData), len(tokens))
	}

	tokens = trimEOS(tokens)
	if len(tokens) == 0 {
		return nil, errors.New("剔除 EOS 后没有可用的语义 token")
	}

	// 5) vocoder
	pred, err := ort.NewTensor(ort.NewShape(1, 1, int64(len(tokens))), tokens)
	if err != nil {
		return nil, fmt.Errorf("构造 pred_semantic 失败：%w", err)
	}
	defer pred.Destroy()
	vIn, err := feed(e.set.VITS, map[string]ort.Value{
		"text_seq": textSeq, "pred_semantic": pred,
		"ge": ref.GE, "ge_advanced": ref.GEAdvanced,
	})
	if err != nil {
		return nil, err
	}
	vOut, err := runSession(e.set.VITS, vIn)
	if err != nil {
		return nil, fmt.Errorf("vits 失败：%w", err)
	}
	defer destroyValues(vOut)
	audio := append([]float32(nil), vOut[0].(*ort.Tensor[float32]).GetData()...)
	// 句首/句尾的静音由模型自由发挥，逐句拼接时会叠成"停顿两秒"；这里削掉，
	// 句间停顿交给播放端（统一 50ms 起排）。
	return TrimSilence(audio, e.voice.SampleRate), nil
}

func (e *Engine) runHuBERT(audio []float32) (*ort.Tensor[float32], error) {
	in, err := ort.NewTensor(ort.NewShape(1, int64(len(audio))), audio)
	if err != nil {
		return nil, fmt.Errorf("构造 hubert 输入失败：%w", err)
	}
	defer in.Destroy()
	outs, err := runSession(e.shared.HuBERT, []ort.Value{in})
	if err != nil {
		return nil, fmt.Errorf("hubert 失败：%w", err)
	}
	t, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		destroyValues(outs)
		return nil, fmt.Errorf("hubert 输出不是 float32（实际 %T）", outs[0])
	}
	for _, v := range outs[1:] {
		if v != nil {
			v.Destroy()
		}
	}
	return t, nil
}

func (e *Engine) runSpeaker(audio []float32) (*ort.Tensor[float32], error) {
	in, err := ort.NewTensor(ort.NewShape(1, int64(len(audio))), audio)
	if err != nil {
		return nil, fmt.Errorf("构造 speaker 输入失败：%w", err)
	}
	defer in.Destroy()
	outs, err := runSession(e.shared.Speaker, []ort.Value{in})
	if err != nil {
		return nil, fmt.Errorf("speaker 失败：%w", err)
	}
	t, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		destroyValues(outs)
		return nil, fmt.Errorf("speaker 输出不是 float32（实际 %T）", outs[0])
	}
	for _, v := range outs[1:] {
		if v != nil {
			v.Destroy()
		}
	}
	return t, nil
}

func (e *Engine) runPromptEncoder(audio32k []float32, sv *ort.Tensor[float32]) (*ort.Tensor[float32], *ort.Tensor[float32], error) {
	a, err := ort.NewTensor(ort.NewShape(1, int64(len(audio32k))), audio32k)
	if err != nil {
		return nil, nil, fmt.Errorf("构造 ref_audio 失败：%w", err)
	}
	defer a.Destroy()
	b, err := ort.NewTensor(ort.NewShape(1, int64(len(sv.GetData()))), append([]float32(nil), sv.GetData()...))
	if err != nil {
		return nil, nil, fmt.Errorf("构造 sv_emb 失败：%w", err)
	}
	defer b.Destroy()

	in, err := feed(e.set.PromptEncoder, map[string]ort.Value{"ref_audio": a, "sv_emb": b})
	if err != nil {
		return nil, nil, err
	}
	outs, err := runSession(e.set.PromptEncoder, in)
	if err != nil {
		return nil, nil, fmt.Errorf("prompt_encoder 失败：%w", err)
	}
	ge, ok1 := outs[0].(*ort.Tensor[float32])
	geAdv, ok2 := outs[1].(*ort.Tensor[float32])
	if !ok1 || !ok2 {
		destroyValues(outs)
		return nil, nil, fmt.Errorf("prompt_encoder 的输出类型不对")
	}
	for _, v := range outs[2:] {
		if v != nil {
			v.Destroy()
		}
	}
	return ge, geAdv, nil
}

// feed 按模型的输入名取张量：**不靠位置猜**，名字对不上就报错（换版本时最先暴露问题的地方）。
func feed(s *sovits.Session, byName map[string]ort.Value) ([]ort.Value, error) {
	vals := make([]ort.Value, len(s.Inputs))
	for i, name := range s.Inputs {
		v, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("模型缺输入 %q（模型有：%v）", name, s.Inputs)
		}
		vals[i] = v
	}
	return vals, nil
}

func runSession(s *sovits.Session, inputs []ort.Value) ([]ort.Value, error) {
	outs := make([]ort.Value, len(s.Outputs))
	if err := s.Run(inputs, outs); err != nil {
		destroyValues(outs)
		return nil, err
	}
	return outs, nil
}

func destroyValues(vs []ort.Value) {
	for _, v := range vs {
		if v == nil {
			continue
		}
		// 输出切片里的空位是"类型化的 nil 指针"（*Tensor[T](nil)），直接 v != nil 判不出来，
		// 调 Destroy 就会炸在 nil 解引用上。
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Ptr && rv.IsNil() {
			continue
		}
		v.Destroy()
	}
}

// semanticSlice 从 sdec 的 y 里取出**新生成的那一段**。
//
// y 的前 initLen 个是参考音频的 prompt token（fsdec 给的初始序列），后面每一步追加一个。
// 这里必须把 prompt 挡掉：它一旦进 vocoder，合成出来的就是参考音频那句话本身——
// 实测症状是"每句回复里都夹着示例语句的声音"。
//
// 丢掉第一个生成 token 是为了与 GENIE 的 `y[:, -idx:]` 对齐（idx 是 0 基下标）；
// 若只生成了一个，就保留它——宁可短，也不能把 prompt 放进来。
func semanticSlice(y []int64, initLen int) []int64 {
	generated := len(y) - initLen
	if generated < 0 {
		generated = 0
	}
	take := generated - 1
	if take <= 0 && generated > 0 {
		take = generated
	}
	if take > len(y) {
		take = len(y)
	}
	if take <= 0 {
		return nil
	}
	return append([]int64(nil), y[len(y)-take:]...)
}

// trimEOS 从**最后一个**越界位置截断（方案 §7.2：>=1024 视为非法/EOS）。
func trimEOS(tokens []int64) []int64 {
	for i := len(tokens) - 1; i >= 0; i-- {
		if tokens[i] >= eosToken {
			return tokens[:i]
		}
	}
	return tokens
}
