package persona

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 拼装结果的形状与"谁被注入、谁被排除"。
func TestBuildSystemPromptBasics(t *testing.T) {
	p := Persona{Name: "Lapwing", SeedText: "你是阳光开朗的桌面伴侣。"}
	rules := []PersonaRule{
		{Slot: "address_user", Value: "Mulili", Tier: TierCore, Kind: KindVolatile, Enabled: true},
		{Slot: "taboo", Value: "不要长篇大论", Tier: TierCore, Kind: KindStable, Enabled: true},
		{Slot: "taboo", Value: "不要说你是AI", Tier: TierCore, Kind: KindStable, Enabled: true},
		{Slot: "catchphrase", Value: "好耶", Tier: TierRecent, Kind: KindVolatile, Enabled: true},
		{Slot: "tone", Value: "被停用的不该出现", Tier: TierCore, Kind: KindVolatile, Enabled: false},
		{Slot: "verbosity", Value: "归档层不该出现", Tier: TierArchived, Kind: KindVolatile, Enabled: true},
	}

	got, dropped := BuildSystemPrompt(p, rules)
	if dropped != 0 {
		t.Errorf("远未超预算时不该丢规则，实际丢了 %d 条", dropped)
	}
	for _, want := range []string{
		"Lapwing",
		"你是阳光开朗的桌面伴侣。",
		"【必须遵守】",
		"称呼用户：Mulili",
		"禁忌：不要长篇大论；不要说你是AI", // 多值槽位归组渲染
		"【最近用户希望你】",
		"口头禅：好耶",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system 文本里缺少 %q\n---\n%s", want, got)
		}
	}
	for _, bad := range []string{"被停用的不该出现", "归档层不该出现"} {
		if strings.Contains(got, bad) {
			t.Errorf("停用规则或归档层规则不该被注入：%q", bad)
		}
	}
	if strings.Index(got, "【必须遵守】") > strings.Index(got, "【最近用户希望你】") {
		t.Error("core 段应排在 recent 段之前——位置本身就是「优先」的一部分")
	}
	t.Logf("拼装结果：\n%s", got)
}

// 超预算必须截 recent、保主体与 core。
func TestBuildSystemPromptRespectsBudget(t *testing.T) {
	p := Persona{Name: "预算测试", SeedText: "这是主体种子，它永远不该被截掉。"}
	rules := []PersonaRule{
		{Slot: "personality", Value: "沉稳", Tier: TierCore, Kind: KindStable, Enabled: true},
	}
	for i := 0; i < 100; i++ {
		rules = append(rules, PersonaRule{
			Slot: "catchphrase", Value: strings.Repeat("字", 50),
			Tier: TierRecent, Kind: KindVolatile, Enabled: true,
		})
	}

	got, dropped := BuildSystemPrompt(p, rules)
	if dropped == 0 {
		t.Fatal("明显超预算时应当丢弃 recent 层规则")
	}
	if n := utf8.RuneCountInString(got); n > InjectBudgetRunes {
		t.Errorf("拼装结果 %d 字，超过预算 %d 字", n, InjectBudgetRunes)
	}
	if !strings.Contains(got, "这是主体种子") {
		t.Error("主体种子永不该被截")
	}
	if !strings.Contains(got, "性格倾向：沉稳") {
		t.Error("core 层规则永不该被截")
	}
	t.Logf("超预算时丢弃 %d 条 recent 规则，最终 %d/%d 字", dropped, utf8.RuneCountInString(got), InjectBudgetRunes)
}

func TestBuildSystemPromptEmpty(t *testing.T) {
	got, dropped := BuildSystemPrompt(Persona{}, nil)
	if strings.TrimSpace(got) != "" {
		t.Errorf("没有任何人格内容时应返回空串（调用方据此不带 system），实际 %q", got)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d，期望 0", dropped)
	}
}
