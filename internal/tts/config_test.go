package tts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-for-you-love/internal/tts/sovits"
)

// newVoiceDir 造一个"文件齐全"的音色目录，返回目录与写给它的配置。
func newVoiceDir(t *testing.T, mutate func(cfg *Voice)) (dir string, cfg Voice) {
	t.Helper()
	dir = t.TempDir()

	mustWrite(t, filepath.Join(dir, "ref", "prompt_wav", "ref.wav"))
	cfg = Voice{
		ID:               "test",
		DisplayName:      "测试",
		ModelVersion:     "v2ProPlus",
		Language:         []string{"zh"},
		SampleRate:       32000,
		DefaultReference: "Normal",
		References: map[string]Reference{
			"Normal": {WAV: "ref/prompt_wav/ref.wav", Text: "参考文本"},
		},
		Models: map[string]string{},
	}
	for _, name := range requiredModels {
		rel := filepath.ToSlash(filepath.Join("tts", "int8", name+".onnx"))
		mustWrite(t, filepath.Join(dir, filepath.FromSlash(rel)))
		cfg.Models[name] = rel
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化配置失败：%v", err)
	}
	mustWriteBytes(t, filepath.Join(dir, "config.json"), b)
	return dir, cfg
}

func TestLoadVoiceOK(t *testing.T) {
	dir, _ := newVoiceDir(t, nil)

	v, err := LoadVoice(dir)
	if err != nil {
		t.Fatalf("应当加载成功，实际：%v", err)
	}
	if v.Dir != dir && v.Dir != mustAbs(t, dir) {
		t.Errorf("Dir 应为绝对路径，得到 %q", v.Dir)
	}
	p, err := v.ModelPath(sovits.VITS)
	if err != nil {
		t.Fatalf("ModelPath(vits) 失败：%v", err)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("ModelPath 应返回绝对路径，得到 %q", p)
	}
}

func TestReferenceFallsBackToDefault(t *testing.T) {
	dir, _ := newVoiceDir(t, nil)
	v, err := LoadVoice(dir)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}

	for _, emotion := range []string{"", "Sad"} {
		wav, text, err := v.ReferenceFor(emotion)
		if err != nil {
			t.Fatalf("情绪 %q 应当回退到默认，实际报错：%v", emotion, err)
		}
		if text != "参考文本" {
			t.Errorf("回退后文本不对：%q", text)
		}
		if !strings.HasSuffix(filepath.ToSlash(wav), "ref/prompt_wav/ref.wav") {
			t.Errorf("参考音频路径不对：%q", wav)
		}
	}
}

func TestLoadVoiceMissingModel(t *testing.T) {
	dir, cfg := newVoiceDir(t, nil)
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(cfg.Models[sovits.StageDec]))); err != nil {
		t.Fatalf("删除测试文件失败：%v", err)
	}

	_, err := LoadVoice(dir)
	if err == nil {
		t.Fatal("缺模型应当报错")
	}
	if !strings.Contains(err.Error(), sovits.StageDec) {
		t.Errorf("错误信息应当点出缺的是哪个模型，实际：%v", err)
	}
}

func TestLoadVoiceRejectsPathEscape(t *testing.T) {
	dir, _ := newVoiceDir(t, func(cfg *Voice) {
		cfg.Models[sovits.Encoder] = "../evil.onnx"
	})
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "evil.onnx"), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备越界文件失败：%v", err)
	}

	_, err := LoadVoice(dir)
	if err == nil {
		t.Fatal("越界路径应当报错")
	}
	if !strings.Contains(err.Error(), "越出") {
		t.Errorf("错误信息应当说明越出音色目录，实际：%v", err)
	}
}

func TestLoadVoiceRejectsUnknownVersion(t *testing.T) {
	dir, _ := newVoiceDir(t, func(cfg *Voice) { cfg.ModelVersion = "v3" })

	if _, err := LoadVoice(dir); err == nil {
		t.Fatal("未知版本应当报错（v3 的键名映射不同）")
	}
}

func TestLoadVoiceRejectsBadDefaultReference(t *testing.T) {
	dir, _ := newVoiceDir(t, func(cfg *Voice) { cfg.DefaultReference = "Angry" })

	if _, err := LoadVoice(dir); err == nil {
		t.Fatal("default_reference 指向不存在的键时应当报错")
	}
}

func TestLoadBackbone(t *testing.T) {
	dir := t.TempDir()
	mustWriteBytes(t, filepath.Join(dir, "config.json"), []byte(`{"model_version":"v2ProPlus","sample_rate":32000}`))

	bb, err := LoadBackbone(dir)
	if err != nil {
		t.Fatalf("骨干配置应当加载成功：%v", err)
	}
	if bb.ModelVersion != "v2ProPlus" || bb.SampleRate != 32000 {
		t.Errorf("解析结果不对：%+v", bb)
	}

	mustWriteBytes(t, filepath.Join(dir, "config.json"), []byte(`{"model_version":"v4"}`))
	if _, err := LoadBackbone(dir); err == nil {
		t.Fatal("未知骨干版本应当报错")
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	mustWriteBytes(t, path, []byte("x"))
}

func mustWriteBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("取绝对路径失败：%v", err)
	}
	return abs
}
