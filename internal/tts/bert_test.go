package tts

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// fp16 → fp32 展开是 BERT 输出的必经一步：模型给的是半精度字节，
// 而喂给 t2s 的必须是 float32。错一位就是整片数值错乱。
func TestFp16ToFloat32(t *testing.T) {
	cases := []struct {
		in   uint16
		want float32
	}{
		{0x0000, 0},
		{0x3C00, 1},
		{0xC000, -2},
		{0x3555, 0.333251953125},        // 0.3333 的最近半精度
		{0x7BFF, 65504},                 // 半精度最大正规数
		{0x0001, 5.960464477539063e-08}, // 最小次正规数
	}
	for _, c := range cases {
		got := fp16ToFloat32(c.in)
		if math.Abs(float64(got-c.want)) > 1e-6*math.Max(1, math.Abs(float64(c.want))) {
			t.Errorf("fp16 %#04x = %v，期望 %v", c.in, got, c.want)
		}
	}
	if v := fp16ToFloat32(0x8000); v != 0 || !math.Signbit(float64(v)) {
		t.Errorf("0x8000 应当是负零，实际 %v", v)
	}
	if !math.IsInf(float64(fp16ToFloat32(0x7C00)), 1) {
		t.Error("0x7C00 应当是 +Inf")
	}
	if !math.IsNaN(float64(fp16ToFloat32(0x7E00))) {
		t.Error("0x7E00 应当是 NaN")
	}
}

func TestBertIdOf(t *testing.T) {
	b := &Bert{vocab: map[string]int64{"好": 7, "a": 3, ",": 5}}
	if got := b.idOf('好'); got != 7 {
		t.Errorf("汉字应当直接命中，实际 %d", got)
	}
	if got := b.idOf('A'); got != 3 {
		t.Errorf("大写应当按 do_lower_case 落到小写，实际 %d", got)
	}
	if got := b.idOf('，'); got != bertUNK {
		t.Errorf("全角逗号不在词表里应当给 [UNK]，实际 %d", got)
	}
}

// 词表是"行号即 id"，且首行可能带 BOM：错位一个就是整句 token 全错。
func TestLoadBertVocab(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vocab.txt")
	content := "\ufeff[PAD]\n[UNK]\n[CLS]\n好\na\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写词表失败: %v", err)
	}
	v, err := loadBertVocab(p)
	if err != nil {
		t.Fatalf("读词表失败: %v", err)
	}
	if v["[PAD]"] != 0 || v["[UNK]"] != 1 || v["[CLS]"] != 2 || v["好"] != 3 || v["a"] != 4 {
		t.Errorf("id 必须是行号（且 BOM 只影响第一行的文本、不影响 id）：%v", v)
	}
}
