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

	got, dropped := BuildSystemPrompt(p, rules, nil)
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
	for _, bad := range []string{"被停用的不该出现", "归档层不该出现", "【偶尔可以这样】"} {
		if strings.Contains(got, bad) {
			t.Errorf("停用规则、归档层规则、以及没有候选时的情调区都不该出现：%q", bad)
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

	got, dropped := BuildSystemPrompt(p, rules, nil)
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

// 情调区：她学到的倾向以「偶尔」措辞注入，每槽位只留最新一条，stable 一律滤掉。
//
// 最关键的一条断言是**基准没被顶掉**：用户明确的称呼与观察到的称呼同槽位，
// 前者仍原样待在【必须遵守】里，后者另起一段。这正是"情调不落 persona_rules"要保住的东西。
func TestBuildSystemPromptMood(t *testing.T) {
	p := Persona{Name: "Mood", SeedText: "主体。"}
	rules := []PersonaRule{
		{Slot: "address_user", Value: "主人", Tier: TierCore, Kind: KindVolatile, Enabled: true},
	}
	// 已按时间倒序（ListCandidates 的契约），所以同一槽位"第一次见到的"就是最新的那条
	candidates := []Candidate{
		{Slot: "address_user", Value: "老板", Evidence: "老板早"},
		{Slot: "address_user", Value: "老大", Evidence: "老大早"}, // 同槽位更旧，不该出现
		{Slot: "tone", Value: "别太正经", Evidence: "你别太正经"},
		{Slot: "personality", Value: "毒舌", Evidence: "她挺毒舌"}, // stable，必须被滤掉
		{Slot: "查无此槽", Value: "x", Evidence: "x"},            // 不认识的槽位，也滤掉
	}

	got, _ := BuildSystemPrompt(p, rules, candidates)

	if !strings.Contains(got, "称呼用户：主人") {
		t.Errorf("用户明确的基准不该被观察到的顶掉：\n%s", got)
	}
	if !strings.Contains(got, "【偶尔可以这样】") {
		t.Errorf("有候选时应当出现情调区：\n%s", got)
	}
	for _, want := range []string{"称呼用户：老板", "语气：别太正经"} {
		if !strings.Contains(got, want) {
			t.Errorf("情调区缺少 %q：\n%s", want, got)
		}
	}
	for _, bad := range []string{"老大", "毒舌", "查无此槽"} {
		if strings.Contains(got, bad) {
			t.Errorf("不该出现的内容 %q 进了 system：\n%s", bad, got)
		}
	}
	if strings.Index(got, "【偶尔可以这样】") < strings.Index(got, "【必须遵守】") {
		t.Error("情调区必须排在最后——它是预算最先牺牲的一段")
	}
	t.Logf("拼装结果：\n%s", got)
}

// 预算不够时情调先死：规则一条不丢，先砍情调。
//
// 这条顺序是设计的一部分（core 永不截 → recent → 情调），反过来的话，
// 一段"偶尔用用"的倾向会把用户明确说过的要求挤出上下文。
func TestBuildSystemPromptMoodDroppedFirst(t *testing.T) {
	// 主体撑到接近预算，情调才有多余开销可砍
	p := Persona{Name: "预算", SeedText: strings.Repeat("主", 1000)}
	rules := []PersonaRule{
		{Slot: "catchphrase", Value: "好耶", Tier: TierRecent, Kind: KindVolatile, Enabled: true},
	}
	var candidates []Candidate
	for _, slot := range []string{"address_user", "tone", "verbosity", "catchphrase", "other"} {
		candidates = append(candidates, Candidate{Slot: slot, Value: strings.Repeat("情", 200)})
	}

	got, dropped := BuildSystemPrompt(p, rules, candidates)

	if dropped == 0 {
		t.Fatal("情调明显装不下时应当被丢弃")
	}
	if n := utf8.RuneCountInString(got); n > InjectBudgetRunes {
		t.Errorf("拼装结果 %d 字，超过预算 %d 字", n, InjectBudgetRunes)
	}
	if !strings.Contains(got, "口头禅：好耶") {
		t.Errorf("recent 层规则不该在情调之前被砍：\n%s", got)
	}
	t.Logf("情调被砍 %d 条，最终 %d/%d 字", dropped, utf8.RuneCountInString(got), InjectBudgetRunes)
}

func TestBuildSystemPromptEmpty(t *testing.T) {
	got, dropped := BuildSystemPrompt(Persona{}, nil, nil)
	if strings.TrimSpace(got) != "" {
		t.Errorf("没有任何人格内容时应返回空串（调用方据此不带 system），实际 %q", got)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d，期望 0", dropped)
	}
}

// 只有情调、没有主体与规则时，也得能用：她的"人设"可以全靠种子以外的东西撑着。
func TestBuildSystemPromptMoodOnly(t *testing.T) {
	got, dropped := BuildSystemPrompt(Persona{}, nil, []Candidate{{Slot: "tone", Value: "懒懒的"}})
	if dropped != 0 {
		t.Errorf("dropped = %d，期望 0", dropped)
	}
	if !strings.Contains(got, "语气：懒懒的") {
		t.Errorf("情调应当被注入：%q", got)
	}
}
