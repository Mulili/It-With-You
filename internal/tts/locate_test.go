package tts

import (
	"os"
	"path/filepath"
	"testing"
)

// 开发期的真实场景：exe 在 build/bin，模型在仓库根。
// 这里用"工作目录在深层子目录"来复现，验证向上回退能命中。
func TestLocateBackboneUpward(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "models", "shared")
	mustWriteBytes(t, filepath.Join(shared, "config.json"), []byte(`{"model_version":"v2ProPlus"}`))

	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Chdir(deep)

	got, err := LocateBackbone("")
	if err != nil {
		t.Fatalf("向上查找应当命中：%v", err)
	}
	if mustAbs(t, got) != mustAbs(t, shared) {
		t.Errorf("找到的是 %q，期望 %q", got, shared)
	}
}

func TestLocateVoiceUpward(t *testing.T) {
	root := t.TempDir()
	voice := filepath.Join(root, "models", "voices", "feibi")
	mustWriteBytes(t, filepath.Join(voice, "config.json"), []byte(`{"id":"feibi"}`))

	deep := filepath.Join(root, "x", "y")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Chdir(deep)

	got, err := LocateVoice("feibi", "")
	if err != nil {
		t.Fatalf("向上查找应当命中：%v", err)
	}
	if mustAbs(t, got) != mustAbs(t, voice) {
		t.Errorf("找到的是 %q，期望 %q", got, voice)
	}

	if _, err := LocateVoice("nope", ""); err == nil {
		t.Error("不存在的音色应当报错")
	}
}

// 没指定 id 时：目录里只有一个音色就用它；有多个必须报错而不是猜。
func TestLocateOnlyVoice(t *testing.T) {
	root := t.TempDir()
	one := filepath.Join(root, "models", "voices", "feibi")
	mustWriteBytes(t, filepath.Join(one, "config.json"), []byte(`{"id":"feibi"}`))

	deep := filepath.Join(root, "p", "q")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Chdir(deep)

	got, err := LocateVoice("", "")
	if err != nil {
		t.Fatalf("单音色应当自动选中：%v", err)
	}
	if mustAbs(t, got) != mustAbs(t, one) {
		t.Errorf("选中 %q，期望 %q", got, one)
	}

	// 再放一个：必须报错并列出两个候选
	mustWriteBytes(t, filepath.Join(root, "models", "voices", "other", "config.json"), []byte(`{"id":"other"}`))
	if _, err := LocateVoice("", ""); err == nil {
		t.Error("多音色且未指定 id 时应当报错")
	}
}
