package text

import (
	"path/filepath"
	"strings"
	"testing"
)

const (
	testOpencpop  = "../../../models/shared/G2P/ChineseG2P/opencpop-strict.txt"
	testPolyphone = "../../../models/shared/G2P/ChineseG2P/polyphonic.json"
)

func newTestFrontend(t *testing.T) *Frontend {
	t.Helper()
	f, err := New(Options{
		OpencpopPath:  filepath.FromSlash(testOpencpop),
		PolyphonePath: filepath.FromSlash(testPolyphone),
	})
	if err != nil {
		t.Fatalf("载入前端失败（模型目录可能不在）：%v", err)
	}
	return f
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"1998年":   "一九九八年",
		"50%":     "百分之五十",
		"3.14":    "三点一四",
		"你好，世界。":  "你好,世界.",
		"**粗体**":  "粗体",
		"hello你好": "你好",
		"真的吗？？？":  "真的吗?",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestPhonesBasic(t *testing.T) {
	f := newTestFrontend(t)
	got := strings.Join(f.Phones("你好"), " ")
	if got != "n i3 h ao3" {
		t.Errorf("你好 → %q，期望 %q", got, "n i3 h ao3")
	}
}

func TestPolyphoneByWord(t *testing.T) {
	f := newTestFrontend(t)
	got := strings.Join(f.Phones("湖泊"), " ")
	if got != "h u2 p o1" {
		t.Errorf("湖泊 → %q（多音字表没生效？）", got)
	}
}

func TestSandhiBuAndYi(t *testing.T) {
	f := newTestFrontend(t)
	if got := strings.Join(f.Phones("不对"), " "); got != "b u2 d ui4" {
		t.Errorf("不对 → %q，期望 不 变二声", got)
	}
	if got := strings.Join(f.Phones("一天"), " "); got != "y i4 t ian1" {
		t.Errorf("一天 → %q，期望 一变四声", got)
	}
}

func TestSplitter(t *testing.T) {
	s := NewSplitter()
	got := s.Split("你好......我是谁？今天天气不错，我们出去走走吧，顺便买点东西回来。")
	// "你好......" 的有效长度只有 4（标点不计），不到 min_len=5，所以并进下一句；
	// 这是方案 §9 那条规则的原样结果，不是 bug。
	if len(got) != 2 {
		t.Fatalf("切出 %d 句：%#v", len(got), got)
	}
	if got[0] != "你好......我是谁？" {
		t.Errorf("第一句 = %q", got[0])
	}
}
