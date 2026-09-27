// Package pgutil 是写 PG 时的参数转换工具。
//
// 这里放的都是同一类转换：**Go 侧用零值表示"没有"，而 SQL 侧要用 NULL**。
// 不能直接传零值——uuid 列传空串会让 pgx 报类型错，而 LIMIT 0 的语义是"一条都不取"，
// 与我们要表达的"不限"正好相反。
package pgutil

// NullIfEmpty 把空字符串转成 SQL NULL（往 uuid 列传 "" 会让 pgx 直接报错）。
func NullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NullIfLimit 把"不限"（limit <= 0）转成 SQL NULL——PG 的 LIMIT NULL 就是不限。
func NullIfLimit(limit int) any {
	if limit <= 0 {
		return nil
	}
	return limit
}

// NullIfZero 把 0 转成 SQL NULL（用于可空的 bigint 列）。
func NullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
