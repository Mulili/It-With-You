package tts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-for-you-love/internal/tts/sovits"
)

// 一个音色的 config.json 里 models 必须齐全的五个逻辑名。
// 字符串本身由 sovits 定义，避免两处各写一份。
var requiredModels = []string{
	sovits.Encoder,
	sovits.FirstStageDec,
	sovits.StageDec,
	sovits.PromptEncoder,
	sovits.VITS,
}

// 支持的模型版本。两者的键名映射不同，混用会产出怪音而不是报错。
var supportedVersions = map[string]bool{
	"v2":        true,
	"v2ProPlus": true,
}

// Reference 是某个情绪下的一条参考音频（零样本克隆的"音色"就是它）。
type Reference struct {
	WAV  string `json:"wav"`
	Text string `json:"text"`
}

// Voice 是一个音色的配置。见方案 §5.1。
type Voice struct {
	ID               string               `json:"id"`
	DisplayName      string               `json:"display_name"`
	ModelVersion     string               `json:"model_version"`
	Language         []string             `json:"language"`
	SampleRate       int                  `json:"sample_rate"`
	Redistributable  bool                 `json:"redistributable"`
	LicenseNote      string               `json:"license_note"`
	DefaultReference string               `json:"default_reference"`
	References       map[string]Reference `json:"references"`
	Models           map[string]string    `json:"models"`

	// Dir 是音色根目录（绝对路径），也是配置里所有相对路径的基准。
	// 不写进 json：它由加载位置决定。
	Dir string `json:"-"`
}

// Backbone 是共享骨干的 config.json（models/shared/config.json）。
//
// 它承担两件事：给"音色声明的版本"一个**可比对的对象**；给出骨干文件的位置。
// 没有它，config.json 里的 model_version 就只是个没人验证的字符串。
type Backbone struct {
	ModelVersion string `json:"model_version"`
	SampleRate   int    `json:"sample_rate"`
	HuBERT       string `json:"hubert"`
	Speaker      string `json:"speaker"`
	Symbols      string `json:"symbols"`
	Opencpop     string `json:"opencpop"`
	Polyphone    string `json:"polyphone"`
	// Bert / BertVocab 是中文 BERT 与它的词表。**可为空**：缺了就退回"零 BERT"，
	// 合成照常进行，只是语调更平（见 infer.go 的 text_bert）。
	//
	// 单独做成可选，是因为它 600MB 上下：有些人只想听个声、不想再下一份大模型。
	Bert      string `json:"bert"`
	BertVocab string `json:"bert_vocab"`
}

// 骨干文件的默认位置，只在 config.json 没写时才用。
const (
	defaultHuBERT    = "hubert/chinese-hubert-base.onnx"
	defaultSpeaker   = "speaker/speaker_encoder.onnx"
	defaultSymbols   = "G2P/symbols_v2.json"
	defaultOpencpop  = "G2P/ChineseG2P/opencpop-strict.txt"
	defaultPolyphone = "G2P/ChineseG2P/polyphonic.json"
	defaultBert      = "roberta/RoBERTa.onnx"
	defaultBertVocab = "roberta/roberta_tokenizer/vocab.txt"
)

// LoadVoice 读入并校验一个音色目录。
//
// 校验在加载期一次做完：五个模型齐全、参考音频存在、版本合法、路径不越出音色目录。
// 把这些留到合成时才发现，用户看到的就是"点了没反应"或"一句话怪音"。
func LoadVoice(dir string) (*Voice, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("音色目录 %q 无法解析为绝对路径：%w", dir, err)
	}
	b, err := os.ReadFile(filepath.Join(abs, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", filepath.Join(abs, "config.json"), err)
	}
	var v Voice
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%w", filepath.Join(abs, "config.json"), err)
	}
	v.Dir = abs
	if err := v.validate(); err != nil {
		return nil, fmt.Errorf("音色 %s：%w", abs, err)
	}
	return &v, nil
}

// LoadBackbone 读入共享骨干的 config.json。
func LoadBackbone(dir string) (*Backbone, error) {
	p := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", p, err)
	}
	var bb Backbone
	if err := json.Unmarshal(b, &bb); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%w", p, err)
	}
	if !supportedVersions[bb.ModelVersion] {
		return nil, fmt.Errorf("%s 的 model_version=%q 不在支持范围（v2 / v2ProPlus）", p, bb.ModelVersion)
	}
	if bb.HuBERT == "" {
		bb.HuBERT = defaultHuBERT
	}
	if bb.Speaker == "" {
		bb.Speaker = defaultSpeaker
	}
	if bb.Symbols == "" {
		bb.Symbols = defaultSymbols
	}
	if bb.Opencpop == "" {
		bb.Opencpop = defaultOpencpop
	}
	if bb.Polyphone == "" {
		bb.Polyphone = defaultPolyphone
	}
	if bb.Bert == "" {
		bb.Bert = defaultBert
	}
	if bb.BertVocab == "" {
		bb.BertVocab = defaultBertVocab
	}
	return &bb, nil
}

// HuBERTPath / SpeakerPath / SymbolsPath 把骨干里的相对路径解析成绝对路径并确认存在。
func (b *Backbone) HuBERTPath(dir string) (string, error) {
	return resolveIn(dir, b.HuBERT, "hubert 模型")
}

func (b *Backbone) SpeakerPath(dir string) (string, error) {
	return resolveIn(dir, b.Speaker, "speaker 模型")
}

func (b *Backbone) SymbolsPath(dir string) (string, error) {
	return resolveIn(dir, b.Symbols, "音素表")
}

func (b *Backbone) OpencpopPath(dir string) (string, error) {
	return resolveIn(dir, b.Opencpop, "拼音音素映射表")
}

func (b *Backbone) PolyphonePath(dir string) (string, error) {
	return resolveIn(dir, b.Polyphone, "多音字表")
}

// BertPaths 返回中文 BERT 与它的词表；**缺任何一个都返回 ok=false**，不报错。
//
// 这是有意的取舍：BERT 只影响语调自然度，不影响"能不能出声"。
// 少一份 600MB 的模型不该让整个语音功能起不来——那时她会退回"零 BERT"，
// 声音更平，但仍然能说话。
func (b *Backbone) BertPaths(dir string) (model, vocab string, ok bool) {
	m, err := resolveIn(dir, b.Bert, "中文 BERT 模型")
	if err != nil {
		return "", "", false
	}
	v, err := resolveIn(dir, b.BertVocab, "BERT 词表")
	if err != nil {
		return "", "", false
	}
	return m, v, true
}

// ModelPath 返回某个逻辑名对应的模型绝对路径，并确认文件存在。
func (v *Voice) ModelPath(logical string) (string, error) {
	rel, ok := v.Models[logical]
	if !ok || rel == "" {
		return "", fmt.Errorf("config.json 的 models 缺少 %q", logical)
	}
	return v.resolve(rel, "模型 "+logical)
}

// ReferenceFor 返回某个情绪下的参考音频（绝对路径）与它的文本。
//
// 未知情绪**回退到 default_reference**，不报错：调用方传的是"这一句的情绪"，
// 不该因为还没给「难过」录参考音频就让整句合成失败。
func (v *Voice) ReferenceFor(emotion string) (wavPath, text string, err error) {
	r, ok := v.References[emotion]
	if !ok {
		r, ok = v.References[v.DefaultReference]
		if !ok {
			return "", "", fmt.Errorf("没有情绪 %q，且 default_reference %q 也不存在",
				emotion, v.DefaultReference)
		}
	}
	p, err := v.resolve(r.WAV, "参考音频")
	if err != nil {
		return "", "", err
	}
	return p, r.Text, nil
}

func (v *Voice) validate() error {
	for name, val := range map[string]string{
		"id":                v.ID,
		"display_name":      v.DisplayName,
		"model_version":     v.ModelVersion,
		"default_reference": v.DefaultReference,
	} {
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("缺少字段 %s", name)
		}
	}
	if !supportedVersions[v.ModelVersion] {
		return fmt.Errorf("model_version=%q 不在支持范围（v2 / v2ProPlus）", v.ModelVersion)
	}
	if len(v.Language) == 0 {
		return fmt.Errorf("缺少字段 language")
	}
	if v.SampleRate <= 0 {
		return fmt.Errorf("sample_rate 必须是正数，现在是 %d", v.SampleRate)
	}
	if len(v.References) == 0 {
		return fmt.Errorf("references 为空")
	}
	if _, ok := v.References[v.DefaultReference]; !ok {
		return fmt.Errorf("default_reference %q 不在 references 里", v.DefaultReference)
	}
	for name, rel := range v.References {
		if strings.TrimSpace(rel.WAV) == "" {
			return fmt.Errorf("references.%s 缺少 wav", name)
		}
		if _, err := v.resolve(rel.WAV, "参考音频 "+name); err != nil {
			return err
		}
	}
	for _, name := range requiredModels {
		if _, err := v.ModelPath(name); err != nil {
			return err
		}
	}
	return nil
}

// resolve 把配置里的相对路径变成本机绝对路径，并确认文件存在。
//
// 必须相对音色目录：绝对路径会让"整个音色目录拷走"失效，而 `../` 能逃出目录去读
// 任意文件——音色包可能是别人给的，这条得挡住。
func (v *Voice) resolve(rel, what string) (string, error) {
	return resolveIn(v.Dir, rel, what)
}

// resolveIn 是上面那条规则的共用实现（骨干路径也用它）。
func resolveIn(dir, rel, what string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s 必须用相对路径（相对音色根目录），现在是 %q", what, rel)
	}
	p := filepath.Join(dir, rel)
	back, err := filepath.Rel(dir, p)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s 的路径越出了目录：%q", what, rel)
	}
	if !fileExists(p) {
		return "", fmt.Errorf("%s 不存在：%s", what, p)
	}
	return p, nil
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
