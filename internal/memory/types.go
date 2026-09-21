// Package memory 是长期记忆（阶段4）。
//
// 它和 history 的区别：history 是"我们聊过什么"（原文，按会话组织），
// memory 是"从中提炼出、值得长期留着的东西"（一句句事实，按语义检索）。
//
// 两分法是这个包的核心约定：
//   - persona_id 为空 = 关于**用户**的公共事实（"用户养了猫"），任何人格都该知道；
//   - persona_id 非空 = 关于**这段关系**的私有经历，只属于那一个人格。
//
// 判据是"这条记忆是关于用户的，还是关于我和他的关系的"——抽取时由模型判断，
// 但写进哪个字段由本包的接口决定，不给调用方选。
package memory

import "time"

// 记忆类型。
//
// promise 是双向的："用户答应我…"与"我答应过用户…"都算，后者对拟人感更关键
// （答应过的事要兑现）。不必加字段区分方向，content 的自然语言能说清。
const (
	KindFact       = "fact"
	KindPreference = "preference"
	KindEvent      = "event"
	KindPromise    = "promise"
)

// Importance 的取值范围。1 分的条目不该被存下来（那本来就是噪声），
// 所以抽取端不会输出它——这里的 Min 只是兜底校验。
const (
	MinImportance     = 1
	MaxImportance     = 5
	DefaultImportance = 3
)

// DefaultDedupThreshold 是判"这两条记忆是不是同一件事"的相似度阈值。
//
// 0.92 是**故意偏高**的：错并不可逆（两条不同的记忆合成一条，信息就没了），
// 而重复只是多占一个 Top-3 名额（还能清理）。等有真实数据再校。
const DefaultDedupThreshold = 0.92

// Memory 是一条长期记忆。
type Memory struct {
	ID string `json:"id"`
	// PersonaID 为空 = 公共事实；非空 = 私有经历。见包注释的两分法。
	PersonaID string `json:"personaId"`
	// Content 是一句**可直接拼进提示词**的话，不是原始对话片段。
	// 判据：能不能不加加工就注入。
	Content string `json:"content"`
	// Kind 取值见 Kind*
	Kind string `json:"kind"`
	// Importance 1..5。没有它，「用户离婚了」与「今天吃了面」就是平等候选。
	Importance int `json:"importance"`
	// Evidence 是抽取这条记忆时的原话，便于回溯（与 persona_changes 同一套路）。
	Evidence string `json:"evidence"`
	// FollowUpAt 非零 = 未完结话题，到这个时间点可以主动问一句（4.6 主动发言用）。
	//
	// 注意它和 CreatedAt 是两件事：CreatedAt 是"事情什么时候发生的"（事实、永久），
	// FollowUpAt 是"我该什么时候提它"（调度、问过一次就清零）。
	FollowUpAt int64 `json:"followUpAt"`
	// LastRecalledAt 是最近一次被检索注入的时间，用于抑制"反复提同一件事"。
	LastRecalledAt int64 `json:"lastRecalledAt"`
	CreatedAt      int64 `json:"createdAt"`
	UpdatedAt      int64 `json:"updatedAt"`
}

// SaveResult 说明一次保存的结果，让调用方能如实记日志。
type SaveResult struct {
	ID string
	// Updated 为 true 表示命中了去重、更新的是已有那条而不是新插入
	Updated bool
}

// Store 是长期记忆的存储契约。
//
// 目前只放 3b（收尾结算）需要的方法。检索（Search）、回访（PendingFollowUps）等
// 留到第 4 步再加——接口随需要长，一次定义全会变成"猜着写"。
type Store interface {
	// Save 写入一条记忆，返回它的 ID。
	//
	// 去重在这里做：若在**该人格可见的范围**内（公共 + 本人格私有）存在
	// "相似度 ≥ threshold 且 Kind 相同"的记忆，就更新那一条而不是插入新的。
	// 跨人格的私有记忆不参与比较——那是别人的私事。
	//
	// vec 是 Content 的向量；单独传而不是塞进 Memory：向量是实现细节（只为检索服务），
	// 不属于"一条记忆"本身。
	Save(m Memory, vec []float32, threshold float64) (SaveResult, error)

	// List 列出该人格**可见**的记忆（公共 + 本人格私有），按创建时间倒序。
	//
	// 给"它记得什么"那个界面用。它与检索的区别是：这里不按相关性排序，只按时间倒序——
	// 用户想看的是"你都记了些什么"，而不是"什么最相关"。
	List(personaID string, limit int) ([]Memory, error)

	// DeletePersona 删除该人格的**私有**记忆。
	//
	// 公共记忆（PersonaID 为空）保留：人格没了，但"用户养了猫"这类事实不该跟着消失。
	DeletePersona(personaID string) error
}

// NowMillis 取当前时间（Unix 毫秒，与项目其他部分同一口径）。
func NowMillis() int64 { return time.Now().UnixMilli() }
