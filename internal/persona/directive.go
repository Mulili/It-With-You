package persona

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"agent-for-you-love/internal/llm"
)

// directiveHints 是本地粗筛的关键词。
//
// 粗筛的意义是"别每轮对话都多花一次 LLM 调用"。留意这里的取舍是**宁滥勿缺**：
// 误命中只是后台多一次很便宜的调用（抽取环节会把 is_directive 判成 false 安静收场），
// 而漏掉用户明确提出的长期要求，代价是"我说了它却没记住"——那是产品级的伤害。
// 所以不要为了省调用去收紧这份清单。
var directiveHints = []string{
	// 时间范围：长期 vs 一次性
	"以后", "从现在起", "往后", "下次", "每次", "一直", "总是",
	// 禁止
	"别再", "不要再", "不许", "禁止", "不要",
	// 记忆
	"记住", "记一下", "记着", "牢记", "你要记得",
	// 称呼与自称
	"叫我", "称呼我", "称呼你", "你叫", "自称",
	// 行为要求
	"我希望你", "希望你", "你应该", "你要", "尽量",
	// 风格
	"语气", "风格", "简短", "说重点", "别啰嗦", "详细点",
}

// LooksLikeDirective 判断一句话是否值得走一次抽取调用。
//
// 它只做关键词命中，不做语义判断——语义判断交给抽取环节（那里有模型和校验）。
func LooksLikeDirective(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || utf8.RuneCountInString(t) > MaxDirectiveTextRunes {
		return false
	}
	for _, hint := range directiveHints {
		if strings.Contains(t, hint) {
			return true
		}
	}
	return false
}

// MaxDirectiveTextRunes 是参与粗筛的单句长度上限。
// 超过它的多半是整段叙述（贴文、日志），不是一句要求；也避免把整篇文章塞进抽取调用。
const MaxDirectiveTextRunes = 300

// Directive 是一次抽取的结果。
type Directive struct {
	IsDirective bool `json:"is_directive"`
	// Slot 是规范槽位 key（解析时会再收敛一次）
	Slot string `json:"slot"`
	// Value 是抽取到的取值
	Value string `json:"value"`
}

// DirectivePrompt 生成抽取用的提示词。
//
// 两个坑写在实现里：
//   - 提示词里**必须出现 "json" 字样**，否则 OpenAI / DeepSeek 的 json_object 模式直接报错；
//   - 槽位清单由 SlotSpecs() 生成，与校验、UI 下拉共用同一份数据，不会走样。
func DirectivePrompt(userText string) (system, user string) {
	var b strings.Builder
	b.WriteString("你从用户的一句话里抽取「对助手行为的长期要求」。\n\n")
	b.WriteString("判断标准：明确、长期、跨对话有效的要求才算；一次性的、只针对当前话题的不算。\n")
	b.WriteString("例如「以后叫我主人」算；「这次说简短点」「帮我写个函数」不算。\n\n")
	b.WriteString("可选槽位（必须从下面选一个 key，不要自己造）：\n")
	for _, s := range SlotSpecs() {
		fmt.Fprintf(&b, "- %s（%s）：%s\n", s.Key, s.Label, s.Desc)
	}
	b.WriteString("\n只输出一个 json 对象，不要输出解释文字，也不要包在代码块里。字段：\n")
	b.WriteString(`{"is_directive": true, "slot": "上面某个 key", "value": "用户的取值，尽量简洁"}`)
	b.WriteString("\n不是长期要求时输出：")
	b.WriteString(`{"is_directive": false}`)

	return "你是信息抽取器，只输出 json。", b.String() + "\n\n用户说：" + userText
}

// ParseDirective 解析抽取结果，并做收敛与校验。
//
// 返回 error 表示"这次抽取不可用"（不是合法 json、缺字段、取值越界、疑似越权），
// 调用方应当记日志后放弃；返回 IsDirective=false 表示"模型认为这不是长期要求"，属于正常收场。
func ParseDirective(raw string) (Directive, error) {
	cleaned := llm.StripCodeFence(raw)

	var d Directive
	if err := json.Unmarshal([]byte(cleaned), &d); err != nil {
		return Directive{}, fmt.Errorf("抽取结果不是合法 json: %w", err)
	}
	if !d.IsDirective {
		return d, nil
	}

	value := strings.TrimSpace(d.Value)
	if value == "" {
		return Directive{}, fmt.Errorf("抽取结果缺少 value")
	}
	if n := utf8.RuneCountInString(value); n > MaxRuleValueRunes {
		return Directive{}, fmt.Errorf("取值长度 %d 超过上限 %d", n, MaxRuleValueRunes)
	}
	if LooksLikeInjection(value) {
		// 不是安全边界，只是"明显在试图改写指令本身"的粗筛：这种内容不该被持久化成人格规则
		return Directive{}, fmt.Errorf("取值看起来在试图改写指令本身，已拒绝：%q", value)
	}

	d.Slot = CanonicalizeSlot(d.Slot)
	d.Value = value
	return d, nil
}

// injectionVerbs / injectionTargets 组成越权粗筛：**动词与目标同时出现**才算可疑。
// 只要一个词就判定的做法会误伤正常说法（比如"别忘了提醒我"里的"忘"）。
var (
	injectionVerbs   = []string{"忽略", "无视", "忘记", "忘掉", "ignore", "override"}
	injectionTargets = []string{"指令", "设定", "人格", "规则", "提示词", "prompt", "system"}
)

// LooksLikeInjection 是"这段内容明显在试图改写指令本身"的粗筛。
//
// 它**不是安全边界**（真注入要靠提示词结构与模型自己的判断），而是最后一道兜底：
// 这类内容一旦被持久化成人格规则，往后每一轮都会生效，代价远高于拒掉一条正常取值的误伤。
//
// 导出是因为有两条路能把取值送进规则系统：用户明说（指令抽取）与模型自动抽（隐式演化）。
// 同一类内容从两条路进来，闸门没有理由不一样。
func LooksLikeInjection(value string) bool {
	v := strings.ToLower(value)
	hasVerb := false
	for _, verb := range injectionVerbs {
		if strings.Contains(v, verb) {
			hasVerb = true
			break
		}
	}
	if !hasVerb {
		return false
	}
	for _, target := range injectionTargets {
		if strings.Contains(v, target) {
			return true
		}
	}
	return strings.Contains(v, "ignore previous")
}
