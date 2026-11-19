package emotion

import "testing"

func TestParseSeedsUsesHeaderColumns(t *testing.T) {
	md := `# 情绪参考集

| 标签 | 判定标准 |
|---|---|
| 调侃 | 玩笑 |

## 参考集

| # | 说话人 | 上文 | 当前句 | 人工标签 | 备注 |
|---|---|---|---|---|---|
| 1 | 她 | 上文一 | 哟，有在学就好 | 调侃 | 口是心非 |
| 2 | 她 | 上文二 | 我这不是在跟你斗嘴玩吗 | 调侃 | 斗嘴 |
| 3 | 她 | 上文三 | 你先去睡，别熬了 | 关心 | 提醒 |
`
	seeds, err := ParseSeeds(md)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(seeds) != 3 {
		t.Fatalf("应当只取参考集表格的 3 行，实际 %d：%+v", len(seeds), seeds)
	}
	if seeds[0].Text != "哟，有在学就好" || seeds[0].Label != "调侃" {
		t.Fatalf("第一行解析错位：%+v", seeds[0])
	}
	if seeds[2].Label != "关心" {
		t.Fatalf("第三行标签错位：%+v", seeds[2])
	}
}

// 列顺序变了也不能错位：参考集是人手维护的，插一列很常见。
func TestParseSeedsTolerantToColumnOrder(t *testing.T) {
	md := `| 人工标签 | 当前句 | 上文 |
|---|---|---|
| 生气 | 你别再说了 | 上文 |
`
	seeds, err := ParseSeeds(md)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(seeds) != 1 || seeds[0].Label != "生气" || seeds[0].Text != "你别再说了" {
		t.Fatalf("按表头定位列失败：%+v", seeds)
	}
}

func TestParseSeedsIgnoresNonTable(t *testing.T) {
	seeds, err := ParseSeeds("没有任何表格\n")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(seeds) != 0 {
		t.Fatalf("没有表格时应当为空，实际 %+v", seeds)
	}
}
