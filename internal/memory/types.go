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

// ChunkIndex 是「片索引」：向量库里的一条**指针**，指向 session_chunks 里的一片。
//
// 它不是记忆本身，而是回忆的入口：检索先在这里按语义找到"聊过的那件事"，
// 再顺着 ChunkID 回 messages 取**真实原文**——于是"她提起过去的事"用的是当时真实说过的话，
// 不是模型自己编的回忆。
//
// 它和 Memory 放在同一个包里，是因为两者共同构成记忆检索的两条线（经历线与事实线），
// 且同处 schema_vector.sql：缺 pgvector 时它们是一起停用的。两条线的分工见 operation.md「检索」。
type ChunkIndex struct {
	// ChunkID 与 session_chunks 一一对应，所以它直接当主键——天然防重复
	ChunkID string
	// SessionID / PersonaID 是冗余字段（反范式），见 schema_vector.sql 里的说明：
	// 前者用于"命中片后回溯它属于哪次对话"，后者用于按人格过滤而不必 JOIN
	SessionID string
	PersonaID string
	// Summary 是抽取式片摘要的渲染文本。**嵌入的就是它**：
	// 检索命中的东西必须与注入的东西是同一份，否则"匹配上了"和"看到了"会对不上
	Summary string
	// CreatedAt 是这条索引建立的时间
	CreatedAt int64
}

// SaveResult 说明一次保存的结果，让调用方能如实记日志。
type SaveResult struct {
	ID string
	// Updated 为 true 表示命中了去重、更新的是已有那条而不是新插入
	Updated bool
}

// MemoryHit 是一条检索命中的记忆：条目本身 + 它与查询的相似度。
//
// 为什么要把相似度带出来、而不是在存储层就按阈值筛掉：
// "多像才算相关"是**策略**（要能随手调、要看真实数据校），
// 而"什么最像"才是存储层的事。把策略写死进 SQL 的话，调一次阈值就得改两个实现。
type MemoryHit struct {
	Memory Memory
	// Score 是余弦相似度，-1..1；越大越像
	Score float64
}

// ChunkHit 是一条检索命中的片索引，语义同 MemoryHit。
type ChunkHit struct {
	Chunk ChunkIndex
	Score float64
}

// Store 是长期记忆的存储契约。
//
// 检索相关的方法在第 4 步补上（原先只有 3b 收尾结算需要的），回访（PendingFollowUps）等
// 仍然留到需要时再加——接口随需要长，一次定义全会变成"猜着写"。
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

	// Search 语义检索该人格**可见**的记忆（公共 + 本人格私有），按相似度倒序，最多 limit 条。
	//
	// 注意：相似度**相同**的几条之间，相对顺序不保证（PG 对排序键相同的行不保证顺序，
	// 内存实现则按插入顺序）。真实数据几乎不会撞上完全相同的相似度，
	// 但测试里构造数据时要留意——与 history 的 created_at 是同一个坑。
	Search(personaID string, vec []float32, limit int) ([]MemoryHit, error)

	// SearchChunks 语义检索该人格的**片索引**，按相似度倒序，最多 limit 条。
	//
	// 命中片索引只是拿到"聊过这件事"的指针与摘要；要更多细节得顺着 ChunkID 回 messages 取原文
	// （那是宿主自己的事，见 operation.md「命中片之后注入什么」）。
	SearchChunks(personaID string, vec []float32, limit int) ([]ChunkHit, error)

	// List 列出该人格**可见**的记忆（公共 + 本人格私有），按创建时间倒序。
	//
	// 给"它记得什么"那个界面用。它与检索的区别是：这里不按相关性排序，只按时间倒序——
	// 用户想看的是"你都记了些什么"，而不是"什么最相关"。
	List(personaID string, limit int) ([]Memory, error)

	// Delete 删掉一条记忆。
	//
	// 与 DeletePersona 的区别只在粒度：那个是"人格没了，把它私有的都带走"，
	// 这个是用户指着某一条说"忘掉它"。
	//
	// 找不到不报错（删两次是同一次的结果）：这个入口只有界面在用，
	// 而"删一个已经不存在的东西"不该变成一个错误弹窗打扰用户。
	Delete(id string) error

	// IndexChunk 写入一条片索引；同一片重复写是**覆盖**（chunk_id 是主键）。
	//
	// 幂等是有意的：结算的最后一步才写"已结算"的标记（见 app 层 settleChunk 的写库顺序），
	// 在那之前的任何失败都会让下一轮整体重放，而重放必须能覆盖而不是撞主键。
	IndexChunk(ci ChunkIndex, vec []float32) error

	// MarkRecalled 把这些记忆标成"刚被注入过"（写 last_recalled_at）。
	//
	// 这是"别反复提同一件事"的**唯一依据**——它记录的是**注入**时间，不是"被用到"的时间：
	// 后者要靠模型自述，得再加一次调用，而注入本身就足以说明"她刚被提醒过这件事"。
	//
	// 找不到的 ID 不该报错：记忆可能刚被用户删掉，而这件事不值得打断一轮对话。
	MarkRecalled(ids []string) error

	// DeletePersona 删除该人格的**私有**记忆。
	//
	// 公共记忆（PersonaID 为空）保留：人格没了，但"用户养了猫"这类事实不该跟着消失。
	DeletePersona(personaID string) error
}

// NowMillis 取当前时间（Unix 毫秒，与项目其他部分同一口径）。
func NowMillis() int64 { return time.Now().UnixMilli() }
