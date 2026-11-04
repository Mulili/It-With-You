package tts

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"agent-for-you-love/internal/onnxrt"
	"agent-for-you-love/internal/tts/sovits"
	"agent-for-you-love/internal/tts/text"
)

// 上层据此决定怎么提示用户；文本原样写进日志。
var (
	ErrMissing         = errors.New("TTS 资源缺失")
	ErrVersionMismatch = errors.New("TTS 模型版本不匹配")
	ErrRuntime         = errors.New("TTS 运行时初始化失败")
)

// Options 是加载一个音色所需的全部输入。
//
// VoiceDir 与 VoiceID 二选一：前者直接指目录，后者按约定位置去找。
// 其余三项留空则按"exe 同目录 → 用户目录"的约定定位。
type Options struct {
	VoiceID     string
	VoiceDir    string
	BackboneDir string
	RuntimePath string
	Threads     int
}

// Engine 持有一次加载的结果：一个音色的五条会话、骨干的两条、音素表与文本前端。
//
// mu 把整条合成串行化：ONNX Runtime 的 Session::Run 不是并发安全的，而分句队列、
// 打断、预热会同时打进来。它同时保护参考侧缓存。
type Engine struct {
	voice    *Voice
	backbone *Backbone
	set      *sovits.Set
	shared   *sovits.Shared
	symbols  *Symbols
	frontend *text.Frontend

	mu   sync.Mutex
	refs map[string]*RefFeatures
}

// NewEngine 按顺序做完六件事，任何一步失败都返回错误、不返回半个 Engine：
//
//  1. 定位骨干与音色目录
//  2. 读音色 config.json 并校验（五个模型、参考音频、路径边界）
//  3. 骨干版本比对——不匹配必须报错，两个版本的键名映射不同，混用是怪音
//  4. 初始化 ONNX Runtime（进程级，与嵌入共用）
//  5. 打开音色五条会话 + 骨干两条会话
//  6. 载入音素表
func NewEngine(opts Options) (*Engine, error) {
	backboneDir := opts.BackboneDir
	if backboneDir == "" {
		d, err := LocateBackbone("")
		if err != nil {
			return nil, fmt.Errorf("%w：%v", ErrMissing, err)
		}
		backboneDir = d
	}
	bb, err := LoadBackbone(backboneDir)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}

	voiceDir := opts.VoiceDir
	if voiceDir == "" {
		d, err := LocateVoice(opts.VoiceID, "")
		if err != nil {
			return nil, fmt.Errorf("%w：%v", ErrMissing, err)
		}
		voiceDir = d
	}
	v, err := LoadVoice(voiceDir)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}

	if v.ModelVersion != bb.ModelVersion {
		return nil, fmt.Errorf("%w：音色要求 %s，骨干是 %s（%s）",
			ErrVersionMismatch, v.ModelVersion, bb.ModelVersion, backboneDir)
	}

	runtimePath := opts.RuntimePath
	if runtimePath == "" {
		p, err := LocateRuntime("")
		if err != nil {
			return nil, fmt.Errorf("%w：%v", ErrRuntime, err)
		}
		runtimePath = p
	}
	if err := onnxrt.Ensure(runtimePath); err != nil {
		return nil, fmt.Errorf("%w：初始化 %s 失败：%v", ErrRuntime, runtimePath, err)
	}

	symbolsPath, err := bb.SymbolsPath(backboneDir)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}
	symbols, err := LoadSymbols(symbolsPath)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}
	opencpopPath, err := bb.OpencpopPath(backboneDir)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}
	polyPath, err := bb.PolyphonePath(backboneDir)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}
	frontend, err := text.New(text.Options{OpencpopPath: opencpopPath, PolyphonePath: polyPath})
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}

	files := make(map[string]string, len(requiredModels))
	for _, name := range requiredModels {
		p, err := v.ModelPath(name)
		if err != nil {
			return nil, fmt.Errorf("%w：%v", ErrMissing, err)
		}
		files[name] = p
	}
	set, err := sovits.Open(files, opts.Threads)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}

	hubertPath, err := bb.HuBERTPath(backboneDir)
	if err != nil {
		set.Close()
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}
	speakerPath, err := bb.SpeakerPath(backboneDir)
	if err != nil {
		set.Close()
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}
	shared, err := sovits.OpenShared(hubertPath, speakerPath, opts.Threads)
	if err != nil {
		set.Close()
		return nil, fmt.Errorf("%w：%v", ErrMissing, err)
	}

	return &Engine{
		voice: v, backbone: bb, set: set, shared: shared,
		symbols: symbols, frontend: frontend,
		refs: make(map[string]*RefFeatures, 2),
	}, nil
}

// Voice 返回加载成功的音色配置（只读）。
func (e *Engine) Voice() *Voice { return e.voice }

// Backbone 返回骨干配置（只读）。
func (e *Engine) Backbone() *Backbone { return e.backbone }

// Symbols 返回音素表。
func (e *Engine) Symbols() *Symbols { return e.symbols }

// Prewarm 预先算好某个情绪下的参考侧张量。
//
// 参考侧只跟"参考音频 + 它的文本"有关，算一次就能缓存；首延迟的大头就是它
// （hubert + speaker + prompt_encoder 三次推理）。启动后调一次，第一句话就不用等。
func (e *Engine) Prewarm(ctx context.Context, emotion string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.refLocked(ctx, emotion)
	return err
}

// Speak 把一句话合成成 PCM（float32、单声道、voice.SampleRate）。
func (e *Engine) Speak(ctx context.Context, txt, emotion string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ref, err := e.refLocked(ctx, emotion)
	if err != nil {
		return nil, err
	}
	phones, dropped := e.symbols.Filter(e.frontend.Phones(txt))
	if len(phones) == 0 {
		return nil, fmt.Errorf("这句话没有可用音素（文本前端没认出任何字）：%q", txt)
	}
	_ = dropped // 丢掉的符号只影响个别字，不值得为它让整句失败
	return e.Synthesize(ctx, ref, phones)
}

// refLocked 取（必要时算）某个情绪的参考侧张量。调用方必须持有 e.mu。
func (e *Engine) refLocked(ctx context.Context, emotion string) (*RefFeatures, error) {
	if r, ok := e.refs[emotion]; ok && r != nil {
		return r, nil
	}
	wavPath, refText, err := e.voice.ReferenceFor(emotion)
	if err != nil {
		return nil, err
	}
	r, err := e.PrepareReference(ctx, wavPath, refText)
	if err != nil {
		return nil, err
	}
	e.refs[emotion] = r
	return r, nil
}

// Close 释放全部会话（音色五条 + 骨干两条）与缓存的参考张量。可以重复调用。
func (e *Engine) Close() error {
	for _, r := range e.refs {
		r.Close()
	}
	e.refs = nil

	var first error
	if e.set != nil {
		first = e.set.Close()
		e.set = nil
	}
	if e.shared != nil {
		if err := e.shared.Close(); err != nil && first == nil {
			first = err
		}
		e.shared = nil
	}
	return first
}
