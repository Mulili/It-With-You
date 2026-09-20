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

// InjectBudgetRunes 是每轮注入 system 的预算上限（字符数）。
//
// 人格是"每轮必带"的固定开销，所以必须有硬上限：超预算时先截 recent 层规则，
// 主体与 core 层永不截。④ 注入链路按这个值截断；内置人格作者也可以用它自查——
// internal/persona/builtin 里的 TestRealBuiltinPersonasUsable 会按这个值报出超预算的人格。
const InjectBudgetRunes = 1500

// Meta 是前端渲染编辑器所需的"静态元信息"：槽位清单 + 各字段的长度上限。
//
// 由后端提供而不是前端写死：槽位表、上限值都是"唯一真相"，前端复制一份迟早走样——
// 加了槽位 UI 里选不到、改了上限提示就变成错的。一次调用同时拿到两者，也省一次往返。
type Meta struct {
	Slots             []SlotSpec `json:"slots"`
	SeedTextRunes     int        `json:"seedTextRunes"`
	RuleValueRunes    int        `json:"ruleValueRunes"`
	InjectBudgetRunes int        `json:"injectBudgetRunes"`
}

// MetaInfo 返回静态元信息（副本，调用方改不到内部表）。
func MetaInfo() Meta {
	return Meta{
		Slots:             SlotSpecs(),
		SeedTextRunes:     MaxSeedTextRunes,
		RuleValueRunes:    MaxRuleValueRunes,
		InjectBudgetRunes: InjectBudgetRunes,
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
