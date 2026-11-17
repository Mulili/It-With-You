package text

import "testing"

// counts 是 BERT 的 repeats：一个字 2 个音素、标点 1 个、被跳过的字 0 个。
// 它必须与 Normalize 后的字符**逐位对齐**，否则整句语调都会错位（而且很难查）。
func TestPhonesWithCounts(t *testing.T) {
	f := newTestFrontend(t)

	phones, counts := f.PhonesWithCounts("你好。")
	if len(counts) != 3 {
		t.Fatalf("三个字符应当有三个 count（你/好/。），实际 %v", counts)
	}
	if counts[0] != 2 || counts[1] != 2 || counts[2] != 1 {
		t.Errorf("汉字应各 2 个音素、标点 1 个，实际 %v", counts)
	}
	if sum := sumInts(counts); sum != len(phones) {
		t.Errorf("counts 之和（%d）必须等于音素数（%d）——这是 repeats 对齐的前提", sum, len(phones))
	}

	// 语气词补丁（嗯）走的是同一条路：仍然是一个字两个音素
	if _, counts = f.PhonesWithCounts("嗯。"); len(counts) != 2 || counts[0] != 2 || counts[1] != 1 {
		t.Errorf("「嗯。」应当是 [2 1]，实际 %v", counts)
	}

	// 数字会先被归一化成汉字，字符数随之变化——counts 也要跟着变
	const num = "1998年"
	_, counts = f.PhonesWithCounts(num)
	if want := len([]rune(Normalize(num))); len(counts) != want {
		t.Errorf("数字归一化后 counts 应当与归一化文本等长，实际 %d vs %d", len(counts), want)
	}
}

func sumInts(v []int) int {
	n := 0
	for _, x := range v {
		n += x
	}
	return n
}
