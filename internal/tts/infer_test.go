package tts

import "testing"

// 这条是回归测试：y 的前 initLen 个是参考音频的 prompt token，
// 一旦混进 vocoder，合成出来的就是参考音频本身（"示例语句的声音"）。
func TestSemanticSliceNeverIncludesPrompt(t *testing.T) {
	// 极端情况：只生成了 1 个 token
	y := make([]int64, 110)
	for i := 0; i < 109; i++ {
		y[i] = 10
	}
	y[109] = 7

	got := semanticSlice(y, 109)
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("只生成 1 个时应保留它且不含 prompt，实际 %v", got)
	}

	// 正常情况：25 步 → 取最后 24 个，起点必须越过 prompt
	y2 := make([]int64, 134)
	for i := range y2 {
		y2[i] = int64(i)
	}
	got2 := semanticSlice(y2, 109)
	if len(got2) != 24 {
		t.Fatalf("长度 = %d，期望 24", len(got2))
	}
	for _, v := range got2 {
		if v <= 108 {
			t.Fatalf("prompt token 混进来了：%d（%v）", v, got2)
		}
	}
}

func TestTrimEOS(t *testing.T) {
	cases := []struct {
		in   []int64
		want int
	}{
		{[]int64{1, 2, 1024}, 2},
		{[]int64{1, 1024, 5}, 1}, // 取**最后一个**越界位置之前
		{[]int64{1, 2, 3}, 3},
	}
	for _, c := range cases {
		if got := trimEOS(c.in); len(got) != c.want {
			t.Errorf("trimEOS(%v) = %v，期望长度 %d", c.in, got, c.want)
		}
	}
}
