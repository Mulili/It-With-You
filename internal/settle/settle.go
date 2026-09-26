// Package settle 是「收尾结算」：把一段已经聊完的对话整理成几样能长期使用的东西——
// 片摘要（抽取式、带原话）、会话标题、待存事实、**她自己琢磨出来的行为倾向**。
//
// 为什么单独成包：它只做"从原文里挑"，不碰数据库、也不调网络——输入是消息、输出是结构化结果。
// 于是它的规则（原话必须回得到原文、抽取而非生成）可以用纯单元测试钉住；
// 而"什么时候结算、分头写到哪张表"的编排留在 app 层。
//
// 为什么是抽取而不是生成：生成式摘要必然丢细节、而且有编造风险——它会把两件事揉成一件，
// 读起来还很通顺。而这份摘要会被当作"想起的那次谈话"注入上下文，
// **编造出来的回忆比没有回忆更糟**。详见 operation.md「片摘要走「抽取」而不是「生成」」。
//
// 四样一次输出（而不是分几次调）：三次调用就是三倍成本与延迟，而它们读的是同一份原文。
// 这也是「隐式演化」和「记忆抽取」共用一条管线的落点——两者的差别只在**分派到哪张表**。
package settle

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	"agent-for-you-love/internal/persona"
)

// 各字段的条数上限。
//
// 不是为了"摘要要短"——恰恰相反，压缩比 10:1（2 万字符 → 2 千字符）是刻意的目标，
// 这个容积足够装下几十条带原话的要点。设上限只是防模型偶尔发疯输出几十条：
// 那会让摘要失去"浓缩"的意义，也会把注入预算吃光。
const (
	MaxKeyPoints  = 8
	MaxFacts      = 6
	MaxUnresolved = 5
	// MaxRuleCandidates 比别的都小得多：值得长期留的行为倾向本来就少，
	// 而多出来的每一条都是候选区里的一条待办，会烦到用户（见 operation.md 的取舍）
	MaxRuleCandidates = 3
)

// title / topic 是"一句话"字段，没有原话可以校验，跑飞了就会把摘要撑坏，所以截断。
const (
	maxTitleRunes = 20
	maxTopicRunes = 60
)

// minQuoteRunes 是参与校验的原话最短长度（去掉空白后）。
//
// 一个字的"嗯"算不上依据，但也不能要求太长——"我不去了"就是四个字。
const minQuoteRunes = 2

// ErrUnusable 表示这次输出没法用：不是合法 json，或者校验之后什么都没剩下。
//
// 它与"调用失败"必须分开：调用失败多半是瞬时的（网络抖动、服务端偶发），下一轮值得再试一次；
// 而输出不可用是**确定的**（同样的输入还是同一份垃圾输出），调用方据此在本进程内放弃这一片，
// 免得每一轮都白烧一次调用。这个区分是调用方（app 层）标记失败集的唯一依据。
var ErrUnusable = errors.New("结算输出不可用")

// KeyPoint 是一条要点：概括 + 支持它的原话。
type KeyPoint struct {
	Point string `json:"point"`
	// Quote 必须逐字出现在原文里；对不上就说明模型改写了，会被丢掉
	Quote string `json:"quote"`
}

// Fact 是一条待长期记住的事实。
type Fact struct {
	// Content 写成一句**可以直接记住的话**（将来会原样注入提示词，不是原始片段）
	Content string
	// Quote 是支持这条事实的原话。与要点同理，但这里更要紧：
	// 摘要写错了只是这一次回忆不准，事实写错了会一直影响之后的每一轮对话
	Quote string
	// Kind 取值见 memory.Kind*
	Kind string
	// Importance 1..5
	Importance int
	// Private 为 true 表示这条只属于本人格（模型判定的 about = persona），
	// false 表示是关于用户的公共事实。见 memory 包的两分法
	Private bool
}

// RuleCandidate 是一条"关于你该怎么说话"的长期倾向：抽出来存进候选表，
// 由注入链路以【偶尔可以这样】的措辞带进上下文——**它不需要用户批准就已经在起作用了**。
//
// 为什么不像事实那样直接写进 persona_rules：规则改的是**行为基准**，一条错的会持续污染每一轮；
// 而这条只是"偶尔可以这样"，错了只说错一句话。所以两者走不同的表：
// 事实进 memories，倾向进候选表（不参与单值槽位覆盖，也就顶不掉用户明确的基准）。
// 用户若认可，可以在界面上把它提升成真规则——那时才走 SaveRule 的权限矩阵。
// 完整取舍见 operation.md（显式为主、隐式落候选区、变更可见可回滚）。
type RuleCandidate struct {
	// Slot 是收敛后的规范槽位 key，且**必然是 volatile**（stable 层自动抽取无权写入）
	Slot  string
	Value string
	// Quote 与其它条目同理：对不上原文就不要
	Quote string
}

// Result 是一次结算的产物（已经过校验，字段可信）。
type Result struct {
	Title      string
	Topic      string
	KeyPoints  []KeyPoint
	Facts      []Fact
	Rules      []RuleCandidate
	Unresolved []string
	// Dropped 是被校验刷掉的条目数（原话对不上、类型不认识、超出上限）。
	// 只用于日志——但它是"模型有没有在编"的唯一可观测信号，别省
	Dropped int
}

// IsEmpty 表示这次整理什么都没得到。
//
// Title 不算数：只有标题没有内容的摘要，进了向量库也匹配不出东西。
// **Rules 也不算数**：它是随摘要搭车出来的附加产物，而 Summary() 里不含它——
// 若只抽出规则、摘要却是空的，那这份结算进不了向量库，留着等于凭空多一条"什么都没记住"的片。
func (r Result) IsEmpty() bool {
	return strings.TrimSpace(r.Topic) == "" && len(r.KeyPoints) == 0 &&
		len(r.Facts) == 0 && len(r.Unresolved) == 0
}

// Summary 把结果渲染成一段文本：它既是**注入上下文**的内容，也是**进向量库**的内容。
//
// 两者用同一份文本是有意的：向量库里那份是"回忆的入口"，命中的正是要注入的那份。
// 若嵌入的是 A、注入的是 B，会出现"检索匹配上了，但模型看到的对不上"这种查不出来的错位。
//
// 事实**不写进摘要**：它们会单独进 memories 表（结构化的、可去重的），
// 同时写两处会让同一件事在注入时出现两遍。
func (r Result) Summary() string {
	var b strings.Builder
	if r.Topic != "" {
		fmt.Fprintf(&b, "主题：%s\n", r.Topic)
	}
	for _, kp := range r.KeyPoints {
		fmt.Fprintf(&b, "- %s：「%s」\n", kp.Point, kp.Quote)
	}
	if len(r.Unresolved) > 0 {
		fmt.Fprintf(&b, "还没说完：%s\n", strings.Join(r.Unresolved, "；"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Prompt 生成结算用的提示词。
//
// 提示词里**必须出现 "json" 字样**：OpenAI / DeepSeek 的 json_object 模式是硬性要求
// （不满足时服务端直接报错），有测试钉住这一点。
func Prompt(msgs []llm.Message) (system, user string) {
	var b strings.Builder
	b.WriteString("你在整理一段刚聊完的对话。请从**原文里挑**出值得留下的内容，不要改写、不要编造。\n\n")
	b.WriteString("输出字段：\n")
	b.WriteString("- title：给这段对话起个标题，十二个字以内\n")
	b.WriteString("- topic：这一段主要在聊什么（一句话）\n")
	b.WriteString("- key_points：最重要的 3~8 条，每条 = 概括 + 支持它的原话\n")
	b.WriteString("- facts：值得长期记住的事。每条包含：\n")
	b.WriteString("    content：写成一句可以直接记住的话（例如「用户爱你傲娇的样子」「用户爱喝美式」）\n")
	b.WriteString("    about：user = 关于用户本人的（喜好、经历、家人、工作），任何人格都该知道；\n")
	b.WriteString("           persona = 关于你和他之间的（约定、一起做过的事、你对他的承诺），只属于你\n")
	b.WriteString("    kind：fact 客观事实 / preference 喜好 / event 发生过的事 / promise 约定或承诺\n")
	b.WriteString("    importance：1~5。5 = 健康、亲人、重大变故；4 = 明确的喜恶或重要约定；\n")
	b.WriteString("                3 = 一般经历；2 = 日常小事； 1 = 无关紧要的内容 \n")
	b.WriteString("    quote：支持这条事实的原话\n")
	b.WriteString("- unresolved：这段里还没说完、或答应过还没做的事，没有就给空数组\n")
	b.WriteString("- rules：只收**你自己说话时的方式**——怎么称呼他、用什么语气、说多长、爱说什么口头禅。\n")
	b.WriteString("    先分清方向再动手：「你该怎么说话」写进 rules；「用户自己喜欢什么」只写进 facts。\n")
	b.WriteString("    「用户爱你傲娇的样子」是后一种，别写进 rules——那是他喜欢什么，不是给你的指示。\n")
	b.WriteString("    每条包含：\n")
	b.WriteString("    slot：只能从下面选\n")
	for _, s := range volatileSlots() {
		fmt.Fprintf(&b, "        %s = %s\n", s.Key, s.Label)
	}
	b.WriteString("    value：写成一句可以直接执行的指示（例如「说话简短一点」）\n")
	b.WriteString("    quote：支持它的原话\n")
	b.WriteString("    这类要求很少见，没有就给空数组；**别把一次性的要求写进来**\n")
	b.WriteString("\n铁律：\n")
	b.WriteString("1. quote 必须是原文里**一字不差**出现过的话，不许改写，不许把两句话拼起来\n")
	b.WriteString("2. 找不到原文依据的内容宁可不写——对不上原文的条目会被直接丢掉\n")
	b.WriteString("3. 只输出一个 json 对象，不要解释，也不要包在代码块里\n\n")
	b.WriteString("json 结构：\n")
	b.WriteString(`{"title":"…","topic":"…",`)
	b.WriteString(`"key_points":[{"point":"…","quote":"…"}],`)
	b.WriteString(`"facts":[{"content":"…","about":"user","kind":"fact","importance":3,"quote":"…"}],`)
	b.WriteString(`"rules":[{"slot":"tone","value":"…","quote":"…"}],`)
	b.WriteString(`"unresolved":["…"]}`)

	return "你是对话整理器：只从原文里抽取，只输出 json。",
		b.String() + "\n\n对话原文：\n" + renderConversation(msgs)
}

// renderConversation 把消息渲染成"用户：… / 你：…"。
//
// 与 history 包的话题判断各写一份（那边也是这么渲染的）：角色称谓属于**提示词措辞**，
// 两份提示词会各自演化，共用一个函数反而会互相牵制。
func renderConversation(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		who := "用户"
		if m.Role == llm.RoleAssistant {
			who = "你"
		}
		fmt.Fprintf(&b, "%s：%s\n", who, m.Content)
	}
	return b.String()
}

// rawResult / rawFact 是模型的原始输出形状，与 Result 分开：
// Result 是**校验过**的，字段都可信；raw 里什么都可能是假的。
type rawResult struct {
	Title      string     `json:"title"`
	Topic      string     `json:"topic"`
	KeyPoints  []KeyPoint `json:"key_points"`
	Facts      []rawFact  `json:"facts"`
	Rules      []rawRule  `json:"rules"`
	Unresolved []string   `json:"unresolved"`
}

type rawFact struct {
	Content    string `json:"content"`
	About      string `json:"about"`
	Kind       string `json:"kind"`
	Importance int    `json:"importance"`
	Quote      string `json:"quote"`
}

type rawRule struct {
	Slot  string `json:"slot"`
	Value string `json:"value"`
	Quote string `json:"quote"`
}

// about 的两个合法取值。
const (
	aboutUser    = "user"
	aboutPersona = "persona"
)

// Parse 解析并校验结算结果。
//
// source 是这一片的原文（用来验原话）。返回 ErrUnusable 表示"这一片这次没整理出来"。
//
// 校验是**逐条**的，不是整批的：一条原话对不上只丢那一条。理由——模型的输出里
// 混着几条编造是常态，为此把整片作废，等于把没编的那部分也一起丢了（不可逆）；
// 而丢掉的条目数会记在 Dropped 里，是"这次模型编得厉害不厉害"的信号。
func Parse(raw string, source []llm.Message) (Result, error) {
	var rr rawResult
	if err := json.Unmarshal([]byte(llm.StripCodeFence(raw)), &rr); err != nil {
		return Result{}, fmt.Errorf("%w: 不是合法 json: %v", ErrUnusable, err)
	}

	// 归一化一次，所有原话都拿它比对
	text := normalize(sourceText(source))

	var res Result
	res.Title = trimShort(rr.Title, maxTitleRunes)
	res.Topic = trimShort(rr.Topic, maxTopicRunes)

	for _, kp := range rr.KeyPoints {
		if len(res.KeyPoints) >= MaxKeyPoints {
			res.Dropped++
			continue
		}
		point := strings.TrimSpace(kp.Point)
		quote, ok := matchQuote(kp.Quote, text)
		if point == "" || !ok {
			res.Dropped++
			continue
		}
		res.KeyPoints = append(res.KeyPoints, KeyPoint{Point: point, Quote: quote})
	}

	for _, f := range rr.Facts {
		if len(res.Facts) >= MaxFacts {
			res.Dropped++
			continue
		}
		content := strings.TrimSpace(f.Content)
		kind := strings.TrimSpace(f.Kind)
		quote, ok := matchQuote(f.Quote, text)
		if content == "" || !ok || !validKind(kind) {
			res.Dropped++
			continue
		}
		res.Facts = append(res.Facts, Fact{
			Content:    content,
			Quote:      quote,
			Kind:       kind,
			Importance: clampImportance(f.Importance),
			// 缺失或不认识 about 时**按私有处理**：公共记忆对所有人生效，
			// 猜错的代价是"另一个她知道了不该知道的事"（不可逆）；
			// 猜成私有的代价只是用户得跟另一个她再说一遍（麻烦而已）。
			// 比较忽略大小写：模型写 "User" 与写 "user" 是同一个意思，
			// 而这里判错的后果正好是最不可逆的那一种
			Private: strings.ToLower(strings.TrimSpace(f.About)) != aboutUser,
		})
	}

	for _, r := range rr.Rules {
		if len(res.Rules) >= MaxRuleCandidates {
			res.Dropped++
			continue
		}
		slot, ok := canonicalVolatileSlot(r.Slot)
		value := strings.TrimSpace(r.Value)
		quote, quoted := matchQuote(r.Quote, text)
		if !ok || value == "" || !quoted || !validRuleValue(value) {
			res.Dropped++
			continue
		}
		res.Rules = append(res.Rules, RuleCandidate{Slot: slot, Value: value, Quote: quote})
	}

	for _, u := range rr.Unresolved {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if len(res.Unresolved) >= MaxUnresolved {
			res.Dropped++
			continue
		}
		res.Unresolved = append(res.Unresolved, u)
	}

	if res.IsEmpty() {
		return Result{}, fmt.Errorf("%w: 校验后什么也没剩下（原始输出 %d 字）",
			ErrUnusable, utf8.RuneCountInString(raw))
	}
	return res, nil
}

// volatileSlots 返回**允许自动演化**的槽位。
//
// 权限矩阵（operation.md）：stable（身份 / 底线 / 禁忌）只有人工能写，自动抽取无权写入——
// 那几样决定"她是谁"，被模型悄悄改掉是最难发现、也最不可逆的一种错。
// 清单直接从 SlotSpecs() 过滤出来，于是它与 UI 下拉、导入校验共用一份数据，不会走样。
func volatileSlots() []persona.SlotSpec {
	all := persona.SlotSpecs()
	out := make([]persona.SlotSpec, 0, len(all))
	for _, s := range all {
		if s.Kind == persona.KindVolatile {
			out = append(out, s)
		}
	}
	return out
}

// canonicalVolatileSlot 收敛槽位，并挡住"自动抽取不许写的那些槽位"。
//
// 为什么不能只靠 prompt 里那份白名单：模型可能自己造一个（address_self、taboo…），
// 而 CanonicalizeSlot 会把**认不出来**的说法一律落到 other —— 那是 volatile，
// 于是"自称"这种 stable 内容会绕个圈子被静默放行。所以这里看的是收敛结果的 kind。
func canonicalVolatileSlot(raw string) (string, bool) {
	key := persona.CanonicalizeSlot(raw)
	spec, ok := persona.LookupSlot(key)
	if !ok || spec.Kind != persona.KindVolatile {
		return "", false
	}
	return key, true
}

// validRuleValue 检查取值长度，以及"是不是在试图改写指令本身"。
//
// 越权粗筛与人格的指令抽取共用同一个判断：同一类内容从两条路进来
// （用户明说 / 模型自动抽），闸门没有理由不一样。
func validRuleValue(v string) bool {
	if utf8.RuneCountInString(v) > persona.MaxRuleValueRunes {
		return false
	}
	return !persona.LooksLikeInjection(v)
}

// matchQuote 检查原话是否真的出现在原文里，返回可以直接存下来的版本。
//
// 归一化只做两件事：去掉所有空白（模型常把换行、空格改动）、剥掉首尾的引号
// （模型常把原话再包一层「」）。除此之外**一律严格匹配**——
// 放宽到"大致相同"就等于取消了这道闸门，而它是抽取式摘要唯一的防编造手段。
func matchQuote(quote, normalizedSource string) (string, bool) {
	q := stripQuotes(quote)
	if utf8.RuneCountInString(q) < minQuoteRunes {
		return "", false
	}
	if !strings.Contains(normalizedSource, normalize(q)) {
		return "", false
	}
	// 比对用去掉空白的版本，但**存下来的是折叠过空白的原样**：
	// 直接存 comparison 用的那种"全无空白"会把英文句子粘成一坨（"Iloveyou"），
	// 而原样存又会让摘要里的一条要点被换行拆成两行。折成单个空格是两者之间唯一舒服的点。
	return collapseSpace(q), true
}

// collapseSpace 把连续空白折成一个空格，并去掉首尾。
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// quoteWrappers 是模型爱加在原话外面的成对符号。
var quoteWrappers = "「」『』“”‘’\"'《》"

// stripQuotes 剥掉首尾的引号类符号，并去掉因此露出来的空白。
//
// 两次 TrimSpace 是必要的：「 我上周末买了个 HHKB  」剥掉引号后首尾各剩一个空格，
// 而它会一路带进摘要文本（注入时看着很脏），也会让"存下来的原话"和原文对不上。
func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, quoteWrappers)
	return strings.TrimSpace(s)
}

// normalize 去掉所有空白字符。
//
// 用 unicode.IsSpace 而不是只认空格与换行：全角空格（U+3000）在中文里很常见，
// 而它恰好长得像普通空格，漏掉它会让"看起来一字不差"的原话校验失败。
func normalize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// sourceText 把原文拼成一段文本，用于校验原话。
func sourceText(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// validKind 判断类型是不是已知的记忆类型。
//
// 不认识的类型直接丢掉、而不是兜底成 fact：类型会影响去重（只有同类型才合并），
// 而"这条到底是什么"只有模型知道，猜不如不存。
func validKind(kind string) bool {
	switch kind {
	case memory.KindFact, memory.KindPreference, memory.KindEvent, memory.KindPromise:
		return true
	}
	return false
}

// clampImportance 把重要性夹到合法区间。
//
// 用夹而不是丢：importance 只是个排序权重，模型写成 7 和写成 5 是同一个意思，
// 为它丢掉一整条事实不值得。
func clampImportance(n int) int {
	if n <= 0 {
		// 模型没给：按"一般经历"处理，而不是按最小值——最小值在语义上是"本该丢掉"
		return memory.DefaultImportance
	}
	if n < memory.MinImportance {
		return memory.MinImportance
	}
	if n > memory.MaxImportance {
		return memory.MaxImportance
	}
	return n
}

// trimShort 截断过长的文本并去掉首尾空白。
func trimShort(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}
