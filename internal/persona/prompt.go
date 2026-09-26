package persona

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 三段的标题与限定语。
//
// 抽成常量不是为了复用文案，而是为了让**预算能算准**：这些字符同样占 InjectBudgetRunes，
// 若写字面量、成本另算一个数，改了文案就会出现"估算没超、写出来超了"。
// 现在成本直接取自常量长度，改多少字都自动跟上。
const (
	coreHead   = "\n【必须遵守】\n"
	recentHead = "\n【最近用户希望你】\n"
	moodHead   = "\n【偶尔可以这样】\n"
	// moodNote 是情调区的限定语，它不能省也不能简写：
	// "这些只是偶尔用一下"完全靠这句话传达给模型（见下面渲染处的说明）。
	moodNote = "（下面这些不是必须的，偶尔换个说法就好，别每轮都用）\n"
)

// BuildSystemPrompt 把「人格主体 + 规则 + 她学到的情调」拼成注入用的 system 文本。
//
// 返回的第二个值是被预算丢掉的条数（>0 时调用方应当记一条日志：说明这人太长、需要瘦身）。
//
// 拼装顺序与预算规则（见 operation.md「注入规则」）：
//  1. 主体与 core 层永不参与截断——它们承载身份与底线，丢了人格就崩了；
//  2. recent 层按注入顺序逐个补，总长超过 InjectBudgetRunes 就**从那里开始停**
//     （截断取前缀，不做"跳过大的留小的"，否则规则取舍会变得不可预测）；
//  3. 情调区（candidates）排在最后，预算不够时它是**第一个被牺牲的**——
//     它本来就是"偶尔用用"，少注入几条不影响她是谁；
//  4. archived 层与停用的规则一律不注入。
//
// 为什么要分「recent 层」与「情调区」两段，而不是把情调也写成规则：
// 规则改的是行为基准，一条错的会持续污染每一轮，所以要走 SaveRule（有权限矩阵把关）；
// 而情调是"她观察到的、偶尔可以这样"，错了只说错一句话。两者的生效强度不同，
// 所以措辞不同（【最近用户希望你】vs【偶尔可以这样】），存储也分离——
// 关键是不能落进 persona_rules 的单值槽位（那里再写入即覆盖，会把她观察到的称呼
// 静默顶掉用户的基准称呼，连 core 层位置一起占走）。
func BuildSystemPrompt(p Persona, rules []PersonaRule, candidates []Candidate) (string, int) {
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
		b.WriteString(coreHead)
		b.WriteString(text)
	}

	// recent 层：先算出已占用的字数，再按顺序补到预算为止
	used := utf8.RuneCountInString(b.String())
	kept := make([]PersonaRule, 0, len(recent))
	dropped := 0
	if len(recent) > 0 {
		// 标题也要占预算，先预扣。不预扣的话累计误差会让最终文本越过上限，
		// 而"上限"这件事没有"差一点"的说法——它就是拿来兜住上下文长度的。
		// 代价是全被截时白扣了这十来个字，换来的是结果**必然**不超。
		used += utf8.RuneCountInString(recentHead)
	}
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
		b.WriteString(recentHead)
		b.WriteString(text)
	}

	// 情调区：她自己在对话里琢磨出来的倾向，措辞必须是"偶尔"而不是"必须"。
	//
	// 这一段的措辞承载着整条设计：它没有配套的生效开关，"只是偶尔用一下"这件事
	// 完全靠 moodNote 传达给模型。所以那行限定不能省，也不能简写——
	// 少了它，模型会把"偶尔可以叫老板"当成"应该叫老板"，那就退化成了一条没经过允许的规则。
	mood := moodCandidates(candidates)
	keptMood := make([]Candidate, 0, len(mood))
	if len(mood) > 0 {
		// 同 recent：标题与限定语先预扣
		used += utf8.RuneCountInString(moodHead) + utf8.RuneCountInString(moodNote)
	}
	for i, c := range mood {
		cost := candidateCost(c)
		if used+cost > InjectBudgetRunes {
			dropped += len(mood) - i
			break
		}
		used += cost
		keptMood = append(keptMood, c)
	}
	if text := renderMood(keptMood); text != "" {
		b.WriteString(moodHead)
		b.WriteString(moodNote)
		b.WriteString(text)
	}

	return strings.TrimRight(b.String(), "\n"), dropped
}

// moodCandidates 从候选里挑出真正能注入的那些，**每个槽位最多留一条**（最新的那条）。
//
// 只留最新的理由：同时给她"老板；老大；掌柜"三个备选称呼，她会轮着叫，反而不像人。
// 人换称呼是一阵一阵的，记住最近那一阵就够——更早的那些留在候选区里，界面看得见。
//
// candidates 按时间倒序（ListCandidates 的契约），所以"第一次见到某槽位"就是最新的那条。
func moodCandidates(candidates []Candidate) []Candidate {
	seen := make(map[string]struct{}, len(candidates))
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if _, dup := seen[c.Slot]; dup {
			continue
		}
		// 再验一次槽位与 kind。候选区的写入路径**不过 CheckWrite**（抽取时挡过一道，
		// 那是唯一一道），而这里已经是要拼进提示词了，是最后一道闸门。
		// 代价只有一次查表，换来的是"stable 内容绝不会以情调名义溜进 system"。
		spec, ok := LookupSlot(c.Slot)
		if !ok || spec.Kind != KindVolatile {
			continue
		}
		if strings.TrimSpace(c.Value) == "" {
			continue
		}
		seen[c.Slot] = struct{}{}
		out = append(out, c)
	}
	return out
}

// renderMood 渲染情调区，每条一行。
//
// 不像 renderRules 那样按槽位归组：那里归组是因为多值槽位（禁忌、口头禅）会有好几条，
// 逐条列出会让模型以为它们是互不相关的指令；而这里每槽位最多一条（见 moodCandidates），
// 归组没有意义。
func renderMood(cs []Candidate) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s：%s\n", SlotLabel(c.Slot), strings.TrimSpace(c.Value))
	}
	return b.String()
}

// candidateCost 估算一条情调占用的预算，与 ruleCost 同一口径（略高估，见那里说明）。
func candidateCost(c Candidate) int {
	return utf8.RuneCountInString(SlotLabel(c.Slot)) + utf8.RuneCountInString(strings.TrimSpace(c.Value)) + 4
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
