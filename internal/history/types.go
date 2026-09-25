// Package history 是对话历史（阶段4）。
//
// 为什么单独成包、而不是并进 persona：人格是"它是个什么样的人"，历史是"我们聊过什么"。
// 两者的生命周期与写入频率完全不同（人格一次改一条，历史每轮加两条），
// 取用方式也不同（人格全量注入 system，历史按会话取）——混在一起会让 Store 接口越来越重。
//
// 存储形态见 operation.md「会话」一节：sessions 是组织单位，messages 是内容。
// 而"只发当前会话的消息"天然给出了上下文边界，替代了原先"只发最近 N 轮"的截断方案。
package history

import (
	"errors"
	"time"
)

// 消息状态。
//
// 取值与 internal/ui 的 Status* 一致（前端也认这两个字符串）。之所以在这里再定义一份：
// "这条消息是正常说完、还是被用户打断"属于数据本身的属性，不是展示层的事，
// 所以取值域该由数据层拥有。两处必须同步——值已经写进前端，不会变。
const (
	StatusOK       = "ok"
	StatusCanceled = "canceled"
)

// DefaultSessionLimit 是列出会话时的默认条数。
const DefaultSessionLimit = 50

// ChunkMaxRunes 是单片的最大字符数：达到它就在**下一条用户消息之前**切开。
//
// 为什么按字符数而不是 token：项目里没有 tokenizer，而片的大小只需"别太离谱"，
// 不需要精确。中文大致 1 字 ≈ 0.6 token，所以 2 万字符 ≈ 1.2 万 token，留了余量。
// 将来真接了 tokenizer 再换成精确计数——阈值是参数，调用点不用动。
//
// 为什么需要它：会话边界由"话题聊完"给出，但连续闲聊可能一整天不结束。
// 只靠会话做边界，超长会话要么被静默截断（中间内容既进不了上下文、也进不了长期记忆），
// 要么顶爆上下文。这一层是**不依赖语义**的兜底。
const ChunkMaxRunes = 20000

// ContextMessagesLimit 是单个片最多带上的消息条数。
//
// 与 ChunkMaxRunes 互补：那个管体量，这个管条数（一屏"嗯""哈哈"字符很少却能塞很多条）。
//
// ⚠️ 它必须 ≥ ChunkMaxMessages + 2，否则会出现"落在缝里"的消息（见 ChunkMaxMessages 的注释）。
const ContextMessagesLimit = 500

// ChunkMaxMessages 是单片最多几条消息：**与 ContextMessagesLimit 同一个量纲**，两条一起看。
//
// 为什么要它：只有**片**能触发结算。若只按字符切，一屏短消息（"嗯""哈哈"）能塞上千条
// 而字符数远没到线，于是拼上下文时先撞上 ContextMessagesLimit 被裁掉开头——
// 那些消息既不在上下文里，又因为片没收尾而进不了结算，**等于静默丢了**
// （正是分片当初要消灭的那个 bug）。
//
// 为什么比 ContextMessagesLimit 少 2：判断发生在写入本轮消息**之前**，
// 也就是说检查通过之后，这片还可能再多出"用户一条 + 回复一条"。
// 留出这个余量，才能保证片内的条数**永远**不超过拼上下文的上限——
// 否则最后那 1~2 条还是会被裁掉，缝依然在。
const ChunkMaxMessages = ContextMessagesLimit - 2

// 超出 ContextMessagesLimit 时取**尾部**（最近的）而不是开头：丢掉寒暄比丢掉"刚才说的那件事"代价小得多。
// 有了 ChunkMaxMessages 之后这条兜底理论上不会再触发，留着是为了防"阈值被调歪"。

// ErrNotFound 表示目标记录不存在。
var ErrNotFound = errors.New("记录不存在")

// Session 是一次完整的对话：从某个话题开始，到聊完为止。
//
// Title / Summary 在会话收尾时一次生成（懒结算），所以未收尾时为空。
type Session struct {
	ID        string `json:"id"`
	PersonaID string `json:"personaId"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	StartedAt int64  `json:"startedAt"`
	// EndedAt 为 0 表示还没收尾。"当前会话"就是该人格最近一个未收尾的会话——
	// 这也是重启后能接着上一段继续聊的原因。
	EndedAt   int64 `json:"endedAt"`
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// Chunk 是会话内的一个片（约 ChunkMaxRunes 字符）。
//
// 它解决的是"会话本身可能永远不结束"：边玩边聊一整天，如果没有这一层，
// 超出上下文的部分既发不出去、也进不了长期记忆，等于凭空消失。
type Chunk struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	PersonaID string `json:"personaId"`
	// Seq 是会话内的片序号，从 1 开始
	Seq       int   `json:"seq"`
	StartedAt int64 `json:"startedAt"`
	// EndedAt 为 0 表示这还是当前片（每个会话最多一片）
	EndedAt int64 `json:"endedAt"`
	// Summary 由片收尾时生成一次（抽取式、带原话引用），**生成后不再重算**——
	// 它要作为稳定前缀注入，一变缓存就全废（理由见 operation.md）
	Summary   string `json:"summary"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Message 是会话里的一条消息。
type Message struct {
	// ID 由存储层生成（uuid）。
	//
	// 注意它与流式事件的"轮次 ID"不是一回事：后者只在进程内标识一轮回复
	//（前端据此过滤上一个请求的残留片段），不落库、也不需要持久。
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	// ChunkID 指向所属的片。v3 之前写入的老行可能为空——它们仍能按 session_id 读出，
	// 只是不参与"按片拼上下文"。
	ChunkID   string `json:"chunkId"`
	PersonaID string `json:"personaId"`
	// Role 取值见 llm.Role*；system 不进这张表（每轮现拼，改人格才即时生效）
	Role    string `json:"role"`
	Content string `json:"content"`
	// Status 取值见 Status*
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
}

// Store 是对话历史的存储契约。
//
// EnsureSession / EnsureChunk 之所以带副作用（没有就建）：调用方要的是"这一轮该写进哪儿"，
// 而不是"有没有"。把"没有就建"推给每个调用点，等于每处都要处理这个分支；
// 而"最近一个未收尾的"只有存储层能高效判断（PG 上都有对应的部分索引）。
type Store interface {
	// EnsureSession 返回该人格最近一个未收尾的会话；没有就新建一个。
	EnsureSession(personaID string) (Session, error)

	// EnsureChunk 返回会话当前的片；当前片**字符数达到 maxRunes 或条数达到 maxMessages** 时，
	// 先把它收尾（标记 ended_at）再开一片新的。两个上限传 <= 0 表示该项不设限。
	//
	// 为什么两个上限都要（2026-09-24 补）：它们的量纲不同，而只有**片**能触发结算。
	// 只按字符切的话，一屏"嗯""哈哈"能塞很多条却几乎不涨字符数，
	// 于是拼上下文时先撞上 ContextMessagesLimit 被裁掉开头——
	// 那些消息既不在上下文里，又因为片没收尾而进不了结算，就成了静默丢失
	// （正是分片当初要消灭的那种 bug）。两个上限对齐之后，"离开上下文"与"可被结算"是同一件事。
	//
	// 判断发生在**写入下一条用户消息之前**，而不是等会话结束——会话可能一整天不结束。
	// 也因此片边界永远落在用户发言之前，一个问答对不会被从中间切开。
	EnsureChunk(sessionID string, maxRunes, maxMessages int) (Chunk, error)

	// AppendMessage 追加一条消息，返回消息 ID（m.ID 为空时由实现生成）。
	AppendMessage(m Message) (string, error)

	// ChunkMessages 取该片的全部消息（按 CreatedAt 正序）——拼上下文用它。
	//
	// limit <= 0 表示**取全部**。收尾结算传 0：它要的是整片原文（摘要的保真度上限就是它）；
	// 拼上下文传 ContextMessagesLimit：那个上限是给上下文兜底的，不是给结算用的。
	//
	// 注意：CreatedAt 是毫秒精度，所以**同一毫秒内写入的多条消息，相对顺序不保证**。
	// 真实路径撞不上（用户敲字与模型生成之间隔着好几秒），但批量灌数据时要留意。
	// 超出 limit 时取**尾部**（最近的）：丢掉寒暄比丢掉"刚才说的那件事"代价小得多。
	ChunkMessages(chunkID string, limit int) ([]Message, error)

	// SessionMessages 取该会话的消息（按 CreatedAt 正序、最多 limit 条，超出取**尾部**）。
	//
	// 给"点开一条会话，看看当时聊了什么"用（历史菜单的第二级）。
	// 与 ChunkMessages 的区别只在范围：那个是一条片，这个是整段会话（可能横跨好几个片）。
	// 排序的精度问题与它一致（毫秒相同则相对顺序不保证）。
	SessionMessages(sessionID string, limit int) ([]Message, error)

	// RecentMessages 取该人格最近的消息（跨会话、时间正序、最多 limit 条）。
	//
	// 界面**不再用它**：历史已经改成"先列会话、点进去再看消息"（见 ui.HistorySession），
	// 所以它现在只剩两个用武之地——删除人格后的清理校验（跨会话地确认"这个人的消息真没了"），
	// 以及将来做摘录 / 导出时的跨会话取数。
	RecentMessages(personaID string, limit int) ([]Message, error)

	// ListSessions 列出该人格的会话（时间倒序）。
	ListSessions(personaID string, limit int) ([]Session, error)

	// ListChunks 列出该会话的片（按 seq 正序）。
	ListChunks(sessionID string) ([]Chunk, error)

	// EndSession 给会话打上结束时间（ended_at），并**顺手收尾它的当前片**。
	//
	// 收尾当前片是必须做的，不是顺手：懒结算扫的是"已收尾但还没摘要"的片，
	// 会话的最后一片若一直停在"未收尾"，就永远拿不到摘要、也永远不会被抽成事实——
	// 那正是分片要修掉的盲区。
	//
	// 幂等：已经结束的会话再调一次不会改动它（条件是 ended_at IS NULL），
	// 因为"哪一轮算结束"是异步判断出来的，重复判定很常见。
	EndSession(sessionID string) error

	// PendingChunks 取"等待结算"的片：已经收尾、还没有摘要、**里面真的有消息**。
	//
	// 收尾懒结算的入口。第三个条件是为了让空片不再被反复扫到：
	// 它们结算不出任何东西，留着只会每轮白扫一遍。
	PendingChunks(limit int) ([]Chunk, error)

	// SetChunkSummary 写入片摘要。它是**一次结算的提交点**：
	// 这一步成功之后，这片就不再出现在 PendingChunks 里（见 app 层 settleChunk 的写库顺序）。
	SetChunkSummary(chunkID, summary string) error

	// SetSessionTitleIfEmpty 写入会话标题，但**只在还没有标题时**。
	//
	// 于是"标题由第一片定下来"这件事由数据本身保证：后来的片即使也产出了标题，
	// 也改不掉已经写下的那个（一次会话只该有一个题目）。
	SetSessionTitleIfEmpty(sessionID, title string) error

	// DeletePersona 删除该人格的全部会话、片与消息。
	//
	// PG 侧本来有 ON DELETE CASCADE 兜着，但仍然显式删除：不让"数据被清掉"这件事
	// 依赖一个看不见的外键行为，也让内存实现有同一条路径可走。
	DeletePersona(personaID string) error
}

// NowMillis 是取当前时间的统一入口，便于测试替换（保持与项目其他部分同一口径：Unix 毫秒）。
func NowMillis() int64 { return time.Now().UnixMilli() }
