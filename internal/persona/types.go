// Package persona 是人格系统（阶段3）。
//
// 人格不是静态配置，而是「用户写的种子 + 随对话演化的规则」：
//   - Persona      主体：用户最初写的种子文本
//   - PersonaRule  演化项：按结构化槽位存放，同一槽位覆盖而不并存
//   - PersonaChange 变更日志：用于回溯，也用于"你上次让我…"这类体验
//
// 三条落地约束（完整契约见项目根的 operation.md「契约（② 定稿）」）：
//
//  1. 槽位写入前收敛：只接受 slots.go 里的规范 key，近义说法由别名表收敛，
//     认不出来落 other。不靠事后自动归并——那是在矛盾产生之后去猜。
//  2. 覆盖还是追加由槽位表决定，不由模型输出决定（模型的 mode 判断不可靠）。
//  3. 注入预算有硬上限：超预算先截 recent 层，主体与 core 永不截。
package persona

// 层级：控制是否注入，以及谁有权写。
const (
	// TierCore 每轮必带，永不被预算截掉，只有人工能写。
	TierCore = "core"
	// TierRecent 参与预算，超预算时优先截它；自动演化只能写这一层。
	TierRecent = "recent"
	// TierArchived 不注入，阶段4 起改为按相关性检索（降级而非删除）。
	TierArchived = "archived"
)

// 槽位稳定性。
const (
	// KindStable 身份/底线/禁忌这类维度：只允许人工改，自动抽取无权写入。
	KindStable = "stable"
	// KindVolatile 称呼/语气/口头禅这类维度：允许自动演化。
	KindVolatile = "volatile"
)

// 写入来源。注意：它与 Persona.Origin 不是一个取值域，别混用。
const (
	// SourceExplicit 用户明说的要求（"以后叫我主人"），经抽取解析后写入。
	SourceExplicit = "explicit"
	// SourceInferred 模型自动抽取，阶段4 才启用，且只能写 volatile 槽位的 recent 层。
	SourceInferred = "inferred"
	// SourceManual 人工在 UI 里直接改。
	SourceManual = "manual"
)

// 人格来源。
const (
	OriginBuiltin  = "builtin"
	OriginUser     = "user"
	OriginImported = "imported"
)

// 变更动作。
const (
	ActionCreate  = "create"
	ActionUpdate  = "update"
	ActionDelete  = "delete"
	ActionEnable  = "enable"
	ActionDisable = "disable"
	// ActionArchive / ActionRevive 是"收进归档层 / 放回近期层"。
	//
	// 单独两个动作而不是复用 update：变更记录里这两件事没有"旧值 → 新值"，
	// 它们的含义就是动作本身（"她学到的被我收起来了"），界面上直接显示成"收起 / 放回"。
	ActionArchive = "archive"
	ActionRevive  = "revive"
)

// 字段长度硬上限（按字符数，不是字节数——中文一个字算一个）。
//
// 这些是"单个字段别离谱"的兜底；真正的提示词预算（1500 字符量级）在注入时统一算，
// 见 operation.md「注入规则」。
const (
	MaxNameRunes      = 60
	MaxSeedTextRunes  = 1200
	MaxRuleValueRunes = 200
	MaxEvidenceRunes  = 500
)

// InjectBudgetRunes 是每轮注入 system 里**人格与规则**那一部分的预算上限（字符数）。
//
// 它覆盖：名字行 + 主体文本 + core/recent 规则。**不含情调区**——情调区有自己的
// MoodBudgetRunes（见下），两笔钱分开。人格是"每轮必带"的固定开销，所以必须有硬上限：
// 超预算时先截 recent 层规则，主体与 core 层永不截。④ 注入链路按这个值截断；
// 内置人格作者也可以用它自查——internal/persona/builtin 里的 TestRealBuiltinPersonasUsable
// 会按这个值报出超预算的人格。
const InjectBudgetRunes = 1500

// MoodBudgetRunes 是情调区（「她学到的」）**单独**的预算上限（字符数，含标题与限定语）。
//
// 为什么不让它和规则抢同一笔钱（2026-09-28 用户定的）：挤在一起时，人格主体一写长
// （主体单独能到 1200），情调区就会整段消失——而它的代价不只是"少了几句情调"：
// **"这一轮到底注入了哪些候选"从此不可知**，而那个时间戳正是候选淘汰的计时依据，
// 于是计时会跟着失准。
//
// 划开之后，两边各自独立：人格再长也不会挤掉情调，情调再多也不会挤掉用户明确说过的要求。
// 500 是拍的，但它比规则那 1500 宽松得多——情调每槽位最多一条，实际用量通常只有一两百字。
const MoodBudgetRunes = 500

// Meta 是前端渲染编辑器所需的"静态元信息"：槽位清单 + 各字段的长度上限。
//
// 由后端提供而不是前端写死：槽位表、上限值都是"唯一真相"，前端复制一份迟早走样——
// 加了槽位 UI 里选不到、改了上限提示就变成错的。一次调用同时拿到两者，也省一次往返。
type Meta struct {
	Slots             []SlotSpec `json:"slots"`
	SeedTextRunes     int        `json:"seedTextRunes"`
	RuleValueRunes    int        `json:"ruleValueRunes"`
	InjectBudgetRunes int        `json:"injectBudgetRunes"`
	MoodBudgetRunes   int        `json:"moodBudgetRunes"`
}

// MetaInfo 返回静态元信息（副本，调用方改不到内部表）。
func MetaInfo() Meta {
	return Meta{
		Slots:             SlotSpecs(),
		SeedTextRunes:     MaxSeedTextRunes,
		RuleValueRunes:    MaxRuleValueRunes,
		InjectBudgetRunes: InjectBudgetRunes,
		MoodBudgetRunes:   MoodBudgetRunes,
	}
}

// Persona 是一个人格的主体。
type Persona struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SeedText  string `json:"seedText"` // 用户最初写的提示词，注入时放在最前
	Origin    string `json:"origin"`   // builtin / user / imported
	IsBuiltin bool   `json:"isBuiltin"`
	// AvatarPath 是虚拟形象路径，阶段6 才用，现在只存不读
	AvatarPath string `json:"avatarPath"`
	CreatedAt  int64  `json:"createdAt"` // Unix 毫秒
	UpdatedAt  int64  `json:"updatedAt"`
}

// PersonaRule 是一条演化规则，按结构化槽位存放。
type PersonaRule struct {
	ID        string `json:"id"`
	PersonaID string `json:"personaId"`
	// Slot 必须是 slots.go 里的规范 key，不接受自由字符串
	Slot  string `json:"slot"`
	Value string `json:"value"`
	// Source 取值见 Source* 常量
	Source string `json:"source"`
	// Evidence 是触发这条规则的原始话，便于回溯；属敏感内容，导出时不带
	Evidence string `json:"evidence"`
	Tier     string `json:"tier"` // core / recent / archived
	Kind     string `json:"kind"` // stable / volatile
	// Priority 越大越先注入；同优先级按 UpdatedAt 倒序
	Priority  int   `json:"priority"`
	Enabled   bool  `json:"enabled"` // 停用而不删
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// PersonaChange 是变更日志的一条。
type PersonaChange struct {
	ID        string `json:"id"`
	PersonaID string `json:"personaId"`
	// RuleID 在"主体字段变更"时为空
	RuleID string `json:"ruleId"`
	// Action 取值见 Action* 常量
	Action   string `json:"action"`
	Field    string `json:"field"`
	OldValue string `json:"oldValue"`
	NewValue string `json:"newValue"`
	Source   string `json:"source"`
	Evidence string `json:"evidence"`
	// CreatedAt 是变更发生的时刻，Unix 毫秒
	CreatedAt int64 `json:"createdAt"`
}

// Candidate 是"值得留存的行为规则"的候选：从对话里自动抽出来，等用户采纳才成为真规则。
//
// 为什么不直接写进 persona_rules：规则改的是**行为方式**，一条错的会持续污染每一轮；
// 而记忆抽错了只影响"她记错一件事"。风险等级不同，所以一个要过审、一个直接生效
// （见 operation.md 的取舍：显式为主、隐式落候选区、变更可见可回滚）。
//
// 字段是 PersonaRule 的子集：真正落库时，tier / kind / priority 这些**由保存路径决定**
// （人写规则时也一样），不该由"抽出来的一条候选"指定——否则模型可以绕过权限矩阵。
type Candidate struct {
	ID        string `json:"id"`
	PersonaID string `json:"personaId"`
	// Slot 是规范槽位 key；抽取时就已收敛，且**只可能是允许自动演化的（volatile）槽位**
	Slot     string `json:"slot"`
	Value    string `json:"value"`
	Evidence string `json:"evidence"`
	// CreatedAt 是这条候选被抽出来的时刻，Unix 毫秒
	CreatedAt int64 `json:"createdAt"`
	// LastUsedAt 是这条候选**最后一次被注入进她的提示词**的时刻，Unix 毫秒。
	//
	// 淘汰看的是它，**不是 CreatedAt**：一条她天天在用的说法，不该因为"抽出来很久了"被删掉
	// （旧规则正是这么删的，能把还在生效的观察清掉）。
	//
	// 为什么把"被提起"定义成"被注入"、而不是"她真的说出口了"（2026-09-28 用户定的）：
	//   - 候选的注入块在**人格提示词**里，而回忆的自标（`[[used:…]]`）在另一个 system 块里，
	//     两套编号混在一起会互相干扰；
	//   - "注入"这个信号零成本、也不受漏标影响（漏一次自标就会让一条候选永远显得没人用）。
	// 语义上也说得通：她只会在**每个槽位最新的那一条**上说话（见 MoodCandidates），
	// 所以"还在她提示词里"≈"她还在用这个说法"。
	LastUsedAt int64 `json:"lastUsedAt"`
}

// NormalizeCandidate 补全候选的两个时间戳。
//
// LastUsedAt 缺省取 CreatedAt 而**不能留 0**，这一条是必须的：淘汰判据是
// `LastUsedAt < now − 窗口`，留 0 会让刚抽出来的候选当场算成"1970 年就没再用过"，
// 一升级就被整批清掉。放在领域包而不是各 store 里，理由与 history.NormalizeMessage 相同：
// 这是业务规则，两种存储实现必须一致（否则"开发用内存、用户用 PG"就成了两套行为）。
//
// ID 不在这里生成：那是存储层的活（两边都已经有各自的做法）。
func NormalizeCandidate(c Candidate, now int64) Candidate {
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	if c.LastUsedAt == 0 {
		c.LastUsedAt = c.CreatedAt
	}
	return c
}
