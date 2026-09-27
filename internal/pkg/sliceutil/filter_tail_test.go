package sliceutil

import "testing"

// FilterTail 的语义有两处是刻意的，各钉一条：**取尾部**而不是头部、limit <= 0 表示不限。
//
// 特别说明为什么值得为这么短的函数写测试：它与 memory/store 里的 trimHits 长得很像，
// 但那个取的是**头部**（hits[:limit]）。两者语义相反、用途不同，而"顺手把重复的统一一下"
// 是很自然的念头——一旦统一错，症状是"最近几条消息变成了最早几条"，
// 在聊天场景里表现为"她记错了刚说过的话"，很难追到这里。
func TestFilterTail(t *testing.T) {
	nums := []int{1, 2, 3, 4, 5}
	even := func(n int) bool { return n%2 == 0 }

	if got := FilterTail(nums, even, 0); len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("limit <= 0 应当表示不限，实际 %v", got)
	}
	if got := FilterTail(nums, even, 5); len(got) != 2 {
		t.Errorf("命中数不足 limit 时应当全给，实际 %v", got)
	}
	// 三个偶数（2、4、6）里只留最后两个：取的是**尾部**
	if got := FilterTail([]int{1, 2, 3, 4, 5, 6}, even, 2); len(got) != 2 || got[0] != 4 || got[1] != 6 {
		t.Errorf("超出 limit 时应当取尾部（4、6），实际 %v", got)
	}
	if got := FilterTail(nums, func(int) bool { return false }, 3); len(got) != 0 {
		t.Errorf("没有命中时应当返回空切片，实际 %v", got)
	}
}
