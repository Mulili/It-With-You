package emotion

import (
	"fmt"
	"strings"
)

// Snapshot 是"上一轮她处于什么情绪"，作为判官的惯性先验。
//
// 判官看到的是一句话，而情绪是连续的；不给上一轮状态，它会每轮独立猜，
// 表现就是"上一句还在生气，下一句突然平静"。
type Snapshot struct {
	Label     string
	Name      string
	Intensity int
	Object    string
}

// Turn 是判官看到的一轮对话。Role 只用"用户"/"她"两种写法。
type Turn struct {
	Role    string
	Content string
}

// JudgeSystemPrompt 拼出判官的 system 提示词。
//
// 只取判标签需要的四栏（对象 / 触发 / 排除 / 分界 / 滑动）：
// 速度、族、限制、冷却留给状态机，不进提示词——每轮都发的东西，
// 多一行就是每轮多一行成本，而它们对"是哪个标签"没有帮助。
func (r Rubric) JudgeSystemPrompt() string {
	var b strings.Builder
	b.WriteString("你是「她」的情绪判官。你只做一件事：读完最近几轮对话，判断她此刻最可能处于哪种情绪。\n")
	b.WriteString("\n# 判定规则\n")

	for _, rule := range r.PerTurn() {
		fmt.Fprintf(&b, "\n%s %s\n", rule.Key, rule.Name)
		writeTag(&b, "对象", rule.Object)
		writeTag(&b, "触发", rule.Trigger)
		writeTag(&b, "排除", rule.Exclude)
		writeTag(&b, "分界", rule.Boundary)
		writeTag(&b, "滑动", rule.Slide)
	}

	b.WriteString(judgeOrder)
	return b.String()
}

func writeTag(b *strings.Builder, tag, val string) {
	if strings.TrimSpace(val) == "" {
		return
	}
	// 情感.md 里的 ** 是给人看的排版，进提示词只会变成噪声。
	val = strings.ReplaceAll(val, "**", "")
	fmt.Fprintf(b, "  【%s】%s\n", tag, val)
}

// judgeOrder 是固定的判定顺序与输出约定。
//
// 它不随 情感.md 变——变的只是上面的规则表。分开写是为了让"规则"和"怎么用规则"
// 各自可维护：改一条情绪不用碰这里，改输出格式也不用碰 15 条情绪。
const judgeOrder = `
# 判定顺序

1. **先判归属**——这是谁的情绪？
   - 他自己难受 / 受挫 / 硬撑 → 2b
   - 她被冒犯 / 被否定 / 边界被踩 → 3
   - 她怕自己失去、被删、不安全 → 4
   - 她怕他出事 → 4b
   - 她觉得自己没做好 → 9
2. **再看惯性**——情绪有延续性。除非这一轮出现明显反转，否则优先延续「上一轮」。
3. **一句话支持多个标签时**：选证据最强的；第二强的放进 alt，不要硬凑。
4. **没有线索就输出 0**（平静）。不要为了"像人"硬编一个情绪。
5. **用户明说自己情绪时按字面读**——"我很生气""我没事""别管我"不要反着猜。
6. **不要输出 11 和 12**：11 孤独由"多久没回"的状态触发，12 爱/依恋是慢变量，
   都不由这一句话判定。

# 输出

只输出一个 json 对象，不要解释、不要 markdown 代码块：
{"label":"2b","intensity":3,"object":"用户","confidence":0.7,"alt":"2","reason":"他在说自己撑不住"}

- label / alt：必须是上面出现过的编号；alt 没有就填 ""
- intensity：1~5，1 = 只差一点，5 = 明显变了个人
- object：自己 / 用户 / 第三方 / 事件 / 关系
- confidence：0~1
- reason：不超过 30 字
`

// JudgeUserPrompt 拼出判官的 user 提示词。
//
// history 是**不含** latest 的最近几轮；latest 单独放在最后一行，
// 因为判官要判的是"她看到这句话时的反应"，不是"整段对话的平均情绪"。
func JudgeUserPrompt(prev *Snapshot, history []Turn, latest string) string {
	var b strings.Builder

	b.WriteString("# 上一轮她的情绪\n")
	if prev == nil || prev.Label == "" {
		b.WriteString("（还没有）\n")
	} else {
		name := prev.Name
		if name == "" {
			name = prev.Label
		}
		fmt.Fprintf(&b, "%s %s（强度 %d，对象 %s）\n", prev.Label, name, prev.Intensity, prev.Object)
	}

	b.WriteString("\n# 最近的对话\n")
	for _, t := range history {
		fmt.Fprintf(&b, "%s：%s\n", t.Role, t.Content)
	}
	fmt.Fprintf(&b, "用户：%s\n", latest)

	b.WriteString("\n# 请判定\n她看到最后这句话时，最可能处于哪种情绪？按约定只输出 json。\n")
	return b.String()
}
