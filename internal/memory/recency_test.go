package memory

import "testing"

func TestRecencyLevel(t *testing.T) {
	const day = int64(24 * 60 * 60 * 1000)
	now := int64(1_700_000_000_000)

	cases := []struct {
		name string
		age  int64
		want int
	}{
		{"刚写入", 0, 5},
		{"2.9 天", day*29/10 - 1, 5},
		{"正好 3 天", 3 * day, 3},
		{"6.9 天", 7*day - 1, 3},
		{"正好 7 天", 7 * day, 2},
		{"一个月", 30 * day, 2},
	}
	for _, c := range cases {
		if got := RecencyLevel(now-c.age, now); got != c.want {
			t.Errorf("%s：档位 = %d，期望 %d", c.name, got, c.want)
		}
	}

	// 缺失/未来时间：中性 3，不给奖励也不惩罚
	if got := RecencyLevel(0, now); got != 3 {
		t.Errorf("时间缺失应为中性 3，实际 %d", got)
	}
	if got := RecencyLevel(now+day, now); got != 3 {
		t.Errorf("未来时间应为中性 3，实际 %d", got)
	}
}

func TestRankScoreAt(t *testing.T) {
	// 同相似度、同 importance：越新越高
	if !(RankScoreAt(0.6, 3, 5) > RankScoreAt(0.6, 3, 3) &&
		RankScoreAt(0.6, 3, 3) > RankScoreAt(0.6, 3, 2)) {
		t.Error("时间档位应当单调影响排序分")
	}

	// 时间差（3 档 = 0.06）不能盖过 importance 的一整级（0.05 × 2 = 0.10）：
	// "很重要但旧"仍应排在"不重要但新"前面
	oldImportant := RankScoreAt(0.6, 5, 2)
	newTrivial := RankScoreAt(0.6, 1, 5)
	if !(oldImportant > newTrivial) {
		t.Errorf("importance 应当仍能压过时间差：%.3f vs %.3f", oldImportant, newTrivial)
	}
}
