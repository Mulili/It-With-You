// Package sovits 是 GPT-SoVITS 推理模型的薄封装：它只知道"模型文件 → 会话"，
// 不知道音色配置、文本前端、播放。
//
// 把 cgo 与 onnxruntime_go 关在这一层，是为了将来换推理后端（或换声码器）时
// 上层一行不用改。
package sovits

import (
	"fmt"
	"runtime"

	ort "github.com/yalue/onnxruntime_go"
)

// config.json 里 models 的逻辑名。字符串只在这里定义一份。
const (
	Encoder       = "t2s_encoder"
	FirstStageDec = "t2s_first_stage_decoder"
	StageDec      = "t2s_stage_decoder"
	PromptEncoder = "prompt_encoder"
	VITS          = "vits"
)

// DefaultThreads 是算子内线程上限的默认值。
//
// 桌宠不能吃满 CPU：她说话时用户可能正在打游戏（阶段7 的低负载目标）。
// 代价是单句更慢，所以分句阈值要跟着调（方案 §7.3）。
const DefaultThreads = 4

// Session 是一条会话，连同它的输入输出名。
//
// 名字必须显式取（不能写死）：`sdec` 的 KV 层数随模型版本变，
// 写死会在某天换模型时静默喂错（方案 §7.1）。
type Session struct {
	Sess    *ort.DynamicAdvancedSession
	Inputs  []string
	Outputs []string
	// OutputTypes 与 Outputs 一一对应：输出张量的元素类型。
	//
	// 需要它是因为**不同精度会走不同的构造方式**：float32 可以让 ORT 自动分配，
	// 而 float16（BERT 的原始导出）必须自己按 2 字节/元素预分配，
	// 否则 binding 按 1 字节算，直接报 "not enough space"。
	OutputTypes []ort.TensorElementDataType
}

// Run 按位置喂输入、按位置收输出（输出传 nil 由 runtime 分配）。
func (s *Session) Run(inputs, outputs []ort.Value) error {
	return s.Sess.Run(inputs, outputs)
}

// Destroy 释放会话。
func (s *Session) Destroy() error {
	if s == nil || s.Sess == nil {
		return nil
	}
	err := s.Sess.Destroy()
	s.Sess = nil
	return err
}

// Set 是一个音色的五条会话。
type Set struct {
	Encoder       *Session
	FirstStageDec *Session
	StageDec      *Session
	PromptEncoder *Session
	VITS          *Session
}

// Shared 是与音色无关的两条骨干会话：hubert 出 ssl 特征，speaker 出说话人向量。
type Shared struct {
	HuBERT  *Session
	Speaker *Session
}

// Open 打开一个音色的五条会话。
func Open(files map[string]string, threads int) (*Set, error) {
	opts, err := newOptions(threads)
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()

	names := []string{Encoder, FirstStageDec, StageDec, PromptEncoder, VITS}
	sessions, err := openGroup(files, names, opts)
	if err != nil {
		return nil, err
	}
	return &Set{
		Encoder:       sessions[0],
		FirstStageDec: sessions[1],
		StageDec:      sessions[2],
		PromptEncoder: sessions[3],
		VITS:          sessions[4],
	}, nil
}

// OpenShared 打开骨干的两条会话。
func OpenShared(hubertPath, speakerPath string, threads int) (*Shared, error) {
	opts, err := newOptions(threads)
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()

	sessions, err := openGroup(map[string]string{
		"hubert":  hubertPath,
		"speaker": speakerPath,
	}, []string{"hubert", "speaker"}, opts)
	if err != nil {
		return nil, err
	}
	return &Shared{HuBERT: sessions[0], Speaker: sessions[1]}, nil
}

// OpenOne 打开单条会话（骨干里那些"不属于音色五件套"的模型用它，比如中文 BERT）。
func OpenOne(path string, threads int) (*Session, error) {
	opts, err := newOptions(threads)
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	return openOne(path, opts)
}

// Close 释放本音色的全部会话；返回第一个错误（其余继续释放，不能因为一个失败就泄漏）。
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	err := destroyGroup([]*Session{s.Encoder, s.FirstStageDec, s.StageDec, s.PromptEncoder, s.VITS})
	s.Encoder, s.FirstStageDec, s.StageDec, s.PromptEncoder, s.VITS = nil, nil, nil, nil, nil
	return err
}

// Close 释放骨干会话。
func (s *Shared) Close() error {
	if s == nil {
		return nil
	}
	err := destroyGroup([]*Session{s.HuBERT, s.Speaker})
	s.HuBERT, s.Speaker = nil, nil
	return err
}

func newOptions(threads int) (*ort.SessionOptions, error) {
	if threads <= 0 {
		threads = DefaultThreads
		if n := runtime.NumCPU(); n < threads {
			threads = n
		}
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("创建 SessionOptions 失败：%w", err)
	}
	if err := opts.SetIntraOpNumThreads(threads); err != nil {
		opts.Destroy()
		return nil, fmt.Errorf("设置线程数失败：%w", err)
	}
	return opts, nil
}

// openGroup 按给定顺序打开一组模型，任一失败就整体失败并收掉已开的。
func openGroup(files map[string]string, names []string, opts *ort.SessionOptions) ([]*Session, error) {
	out := make([]*Session, 0, len(names))
	for _, name := range names {
		path, ok := files[name]
		if !ok || path == "" {
			destroyGroup(out)
			return nil, fmt.Errorf("缺少模型 %q", name)
		}
		s, err := openOne(path, opts)
		if err != nil {
			destroyGroup(out)
			return nil, fmt.Errorf("加载 %s 失败（%s）：%w", name, path, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// openOne 打开一条会话。
//
// 名字来自 GetInputOutputInfo：它会临时建一次会话，等于**每个模型多加载一遍**。
// 这个代价换的是"永远不写死名字"。启动慢可以用后台预热盖掉；
// 如果将来实测这 2~3 秒不能接受，再把名字缓存进 config.json。
func openOne(path string, opts *ort.SessionOptions) (*Session, error) {
	ins, outs, err := ort.GetInputOutputInfoWithOptions(path, opts)
	if err != nil {
		return nil, fmt.Errorf("读取输入输出信息失败：%w", err)
	}
	inNames := make([]string, len(ins))
	for i, v := range ins {
		inNames[i] = v.Name
	}
	outNames := make([]string, 0, len(outs))
	outTypes := make([]ort.TensorElementDataType, 0, len(outs))
	for _, v := range outs {
		// 跳过零维（标量）输出：这个 binding 建不出零维张量，自动分配会直接报
		// "The shape has no dimensions"。`sdec` 的 stop_condition_tensor 就是标量，
		// 而它的语义（该停了）我们能用"最新 token >= 1024"等价判断。
		if len(v.Dimensions) == 0 {
			continue
		}
		outNames = append(outNames, v.Name)
		outTypes = append(outTypes, v.DataType)
	}
	sess, err := ort.NewDynamicAdvancedSession(path, inNames, outNames, opts)
	if err != nil {
		return nil, fmt.Errorf("创建会话失败：%w", err)
	}
	return &Session{Sess: sess, Inputs: inNames, Outputs: outNames, OutputTypes: outTypes}, nil
}

func destroyGroup(list []*Session) error {
	var first error
	for _, s := range list {
		if err := s.Destroy(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
