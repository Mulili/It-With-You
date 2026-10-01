package persona

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 「请她让位」挑的必须是**她学来的**那条，不是用户写的。
//
// 这条界线是整件事的立身之本：用户手写的（manual）、明说的（explicit），
// 系统没有资格主动提议收掉它们——真不要了，用户会自己去删。
func TestDowngradeCandidateOnlyPicksPromoted(t *testing.T) {
	rules := []PersonaRule{
		{ID: "a", Slot: "tone", Value: "别太正经", Source: SourceManual, Tier: TierRecent, Enabled: true},
		{ID: "b", Slot: "catchphrase", Value: "好耶", Source: SourceExplicit, Tier: TierRecent, Enabled: true},
		{ID: "c", Slot: "catchphrase", Value: "得嘞", Source: SourcePromoted, Tier: TierRecent, Enabled: true},
	}

	got, ok := DowngradeCandidate(rules)
	if !ok {
		t.Fatal("有一条她学来的规则，应当挑得出来")
	}
	if got.ID != "c" {
		t.Errorf("只该挑她学来的那条（c），实际 %s：%s", got.ID, got.Value)
	}

	// 一条都没有时，不能"退而求其次"去动用户写的
	onlyMine := []PersonaRule{rules[0], rules[1]}
	if _, ok := DowngradeCandidate(onlyMine); ok {
		t.Error("没有她学来的规则时，不该去动用户手写/明说的那些")
	}
}

// 已经在等回话的、以及被拒绝过的，都不再被挑——否则同一件事会被反复提起。
func TestDowngradeCandidateSkipsAskedAndRefused(t *testing.T) {
	base := func(id string) PersonaRule {
		return PersonaRule{ID: id, Slot: "catchphrase", Value: "好耶",
			Source: SourcePromoted, Tier: TierRecent, Enabled: true}
	}

	asked := base("a")
	asked.DowngradeAskedAt = 111
	refused := base("b")
	refused.DowngradeRefusedAt = 222

	if _, ok := DowngradeCandidate([]PersonaRule{asked}); ok {
		t.Error("已经问过、还等着回话的不该再被挑出来")
	}
	if _, ok := DowngradeCandidate([]PersonaRule{refused}); ok {
		t.Error("被拒绝过的**永不再提**——这是那一列存在的全部意义")
	}

	// 但"问过"的记录不影响**之后新学来的**那条：她可以接着为别的做法来商量
	fresh := base("c")
	// 同槽位不同取值：多值槽位本来就该共存
	if got, ok := DowngradeCandidate([]PersonaRule{asked, fresh}); !ok || got.ID != "c" {
		t.Errorf("应当挑出没问过的那条，实际 ok=%v %+v", ok, got)
	}
}

// 排序：priority 小的先让位，其次最旧的先让位。
//
// 为什么是"最旧"而不是"最近"：同样是让位，先来后到最不伤感情；
// 而且旧的那条更可能已经被新习惯取代了（新的那条还在被用户念叨）。
func TestDowngradeCandidateOrder(t *testing.T) {
	newRule := func(id string, priority int, createdAt int64) PersonaRule {
		return PersonaRule{ID: id, Slot: "catchphrase", Value: "V" + id,
			Source: SourcePromoted, Tier: TierRecent, Enabled: true,
			Priority: priority, CreatedAt: createdAt}
	}

	// priority 优先于年龄：哪怕它是刚学的
	rules := []PersonaRule{
		newRule("a", 0, 300),
		newRule("b", -5, 900), // priority 最小 → 它先让位
		newRule("c", 0, 100),  // 同 priority 里最旧
	}
	if got, _ := DowngradeCandidate(rules); got.ID != "b" {
		t.Errorf("priority 最小的先让位，实际挑了 %s", got.ID)
	}
	// 把 priority 最小的那条排除掉，剩下的两条 priority 相同 → 比年龄
	if got, _ := DowngradeCandidate([]PersonaRule{rules[0], rules[2]}); got.ID != "c" {
		t.Errorf("priority 相同时最旧的先让位，实际挑了 %s", got.ID)
	}

	// 时间戳也完全相同时按 ID 兜底：结果必须与遍历顺序无关（存储顺序不该影响决策）
	same := []PersonaRule{newRule("z", 0, 100), newRule("a", 0, 100)}
	got, _ := DowngradeCandidate(same)
	reversed := []PersonaRule{same[1], same[0]}
	got2, _ := DowngradeCandidate(reversed)
	if got.ID != got2.ID {
		t.Errorf("同样的数据不该因遍历顺序挑出不同的条目：%s vs %s", got.ID, got2.ID)
	}
}

// 不在范围内的都不该被挑：core 层、归档层、停用的、以及 stable 维度的槽位。
func TestDowngradeCandidateScope(t *testing.T) {
	base := func(id string) PersonaRule {
		return PersonaRule{ID: id, Slot: "catchphrase", Value: "好耶",
			Source: SourcePromoted, Tier: TierRecent, Enabled: true}
	}

	core := base("core")
	core.Tier = TierCore
	archived := base("archived")
	archived.Tier = TierArchived
	off := base("off")
	off.Enabled = false
	// stable 维度（身份、底线）：哪怕来源是提升来的，也不该由系统提议收掉
	stable := base("stable")
	stable.Slot = "personality"

	for name, r := range map[string]PersonaRule{
		"core 层": core, "归档层": archived, "停用的": off, "stable 槽位": stable,
	} {
		if _, ok := DowngradeCandidate([]PersonaRule{r}); ok {
			t.Errorf("%s 不该被挑出来让位", name)
		}
	}
}

// PromptBudgetUsed 算的是"**想要**多少"，含会被截掉的部分——那条触发线完全靠它。
//
// 反过来说：如果它算的是"实际注入多少"，那个数永远超不了预算（超的都被截掉了），
// 于是"请她腾位置"这件事永远不会发生。这条断言就是钉住这个区别的。
func TestPromptBudgetUsedCountsWhatItWantsNotWhatFits(t *testing.T) {
	p := Persona{Name: "预算", SeedText: strings.Repeat("主", 1000)}
	var rules []PersonaRule
	for _, slot := range []string{"catchphrase", "taboo", "tone", "verbosity", "other"} {
		rules = append(rules, PersonaRule{ID: slot, Slot: slot, Value: strings.Repeat("规", 200),
			Source: SourcePromoted, Tier: TierRecent, Enabled: true})
	}

	wants := PromptBudgetUsed(p, rules)
	if wants <= InjectBudgetRunes {
		t.Fatalf("这份数据想要的明显超过预算，PromptBudgetUsed 却算出 %d ≤ %d", wants, InjectBudgetRunes)
	}

	// 而真正注入的那份**必然**不超预算——两个数的差，就是被截掉的部分
	text, dropped, _ := BuildSystemPrompt(p, rules, nil)
	if dropped == 0 {
		t.Fatal("这份数据应当有规则被截掉")
	}
	if got := utf8.RuneCountInString(text); got > InjectBudgetRunes {
		t.Errorf("实际注入 %d 字，超过预算 %d", got, InjectBudgetRunes)
	}

	// 没超预算时它也该正常工作：算出的数应当落在合理范围内
	small := Persona{Name: "小", SeedText: "主体。"}
	few := []PersonaRule{{ID: "x", Slot: "tone", Value: "别太正经", Tier: TierRecent, Enabled: true}}
	lite := PromptBudgetUsed(small, few)
	if lite <= 0 || lite > InjectBudgetRunes {
		t.Errorf("小人格的占用应当是个合理的正数，实际 %d", lite)
	}
	if smaller := PromptBudgetUsed(small, nil); smaller >= lite {
		t.Errorf("没有规则时占用应当更小：%d vs %d", smaller, lite)
	}
}
