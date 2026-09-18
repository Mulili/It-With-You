package persona

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// BuildSystemPrompt 把「人格主体 + 规则」拼成注入用的 system 文本。
//
// 返回的第二个值是被预算丢掉的规则条数（>0 时调用方应当记一条日志：说明这人太长、需要瘦身）。
//
// 拼装顺序与预算规则（见 operation.md「注入规则」）：
//  1. 主体与 core 层永不参与截断——它们承载身份与底线，丢了人格就崩了；
//  2. recent 层排在最后，按注入顺序逐个补，总长超过 InjectBudgetRunes 就**从那里开始停**
//     （截断取前缀，不做"跳过大的留小的"，否则规则取舍会变得不可预测）；
//  3. archived 层与停用的规则一律不注入。
func BuildSystemPrompt(p Persona, rules []PersonaRule) (string, int) {
	sorted := append([]PersonaRule(nil), rules...)
	SortRules(sorted)

	var core, recent []PersonaRule
	for _, r := range sorted {
		if !r.Enabled {
			continue
		}
		switch r.Tier {
		case TierCore:
			core = append(core, r)
		case TierRecent:
			recent = append(recent, r)
		}
	}

	var b strings.Builder
	// 先给模型一个明确的"你是谁"，比让它从种子里自己猜更稳
	if name := strings.TrimSpace(p.Name); name != "" {
		fmt.Fprintf(&b, "你正在扮演「%s」。以下是你的人格设定，请始终保持，不要跳出角色。\n", name)
	}
	if seed := strings.TrimSpace(p.SeedText); seed != "" {
		b.WriteString("\n")
		b.WriteString(seed)
		b.WriteString("\n")
	}
	if text := renderRules(core); text != "" {
		b.WriteString("\n【必须遵守】\n")
		b.WriteString(text)
	}

	// recent 层：先算出已占用的字数，再按顺序补到预算为止
	used := utf8.RuneCountInString(b.String())
	kept := make([]PersonaRule, 0, len(recent))
	dropped := 0
	for i, r := range recent {
		cost := ruleCost(r)
		if used+cost > InjectBudgetRunes {
			dropped = len(recent) - i
			break
		}
		used += cost
		kept = append(kept, r)
	}
	if text := renderRules(kept); text != "" {
		// 措辞刻意用"最近用户希望你"：位置与措辞一起强化"这是较新的偏好"
		b.WriteString("\n【最近用户希望你】\n")
		b.WriteString(text)
	}

	return strings.TrimRight(b.String(), "\n"), dropped
}

// renderRules 把规则按槽位归组渲染成人类可读的列表。
//
// 归组而不是一条一行：多值槽位（禁忌、口头禅）会有好几条，逐条列出会让模型以为
// 它们是互不相关的指令；合并成"禁忌：A；B"更接近人写提示词的方式。
func renderRules(rules []PersonaRule) string {
	if len(rules) == 0 {
		return ""
	}
	order := make([]string, 0, len(rules))
	values := make(map[string][]string, len(rules))
	for _, r := range rules {
		if _, seen := values[r.Slot]; !seen {
			order = append(order, r.Slot)
		}
		values[r.Slot] = append(values[r.Slot], strings.TrimSpace(r.Value))
	}

	var b strings.Builder
	for _, slot := range order {
		fmt.Fprintf(&b, "- %s：%s\n", SlotLabel(slot), strings.Join(values[slot], "；"))
	}
	return b.String()
}

// ruleCost 估算一条规则占用多少预算。
//
// 只是估算（真实文本还有标签、分隔符等固定开销），用途是"别让 recent 层把上下文撑爆"，
// 所以宁可略高估；精度不影响正确性。
func ruleCost(r PersonaRule) int {
	return utf8.RuneCountInString(SlotLabel(r.Slot)) + utf8.RuneCountInString(r.Value) + 4
}
