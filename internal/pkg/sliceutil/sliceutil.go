// Package sliceutil 是跨包共用的切片工具。
package sliceutil

// FilterTail 过滤出满足 keep 的元素；结果超过 limit 时**取尾部** limit 个。
//
// 取尾部而不是头部，是给"取最近的 N 条"这类需求用的：消息、日志、记录都是越新越靠后。
// limit <= 0 表示不限。
//
// 写成泛型函数而不是各包手抄，是因为"过滤 + 取尾部"这套在 history 的内存实现里
// 抄了三遍（只差筛选的字段不同）。那种抄法的问题不在重复本身，而在于
// **改条件时很难发现另一处也要改**——比如某天要给其中一条加上"排除已取消的"，
// 另外两条就会静默地与它分叉。
func FilterTail[T any](items []T, keep func(T) bool, limit int) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
