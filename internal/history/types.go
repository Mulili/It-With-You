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
// 两者都是**兜底**，真正的边界由分片给出。
//
// 超出时取**尾部**（最近的）而不是开头：丢掉寒暄比丢掉"刚才说的那件事"代价小得多。
const ContextMessagesLimit = 500

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

	// EnsureChunk 返回会话当前的片；当前片字符数达到 maxRunes 时，
	// 先把它收尾（标记 ended_at）再开一片新的。
	//
	// 判断发生在**写入下一条用户消息之前**，而不是等会话结束——会话可能一整天不结束。
	// 也因此片边界永远落在用户发言之前，一个问答对不会被从中间切开。
	EnsureChunk(sessionID string, maxRunes int) (Chunk, error)

	// AppendMessage 追加一条消息，返回消息 ID（m.ID 为空时由实现生成）。
	AppendMessage(m Message) (string, error)

	// ChunkMessages 取该片的全部消息（按 CreatedAt 正序）——拼上下文用它。
	//
	// 注意：CreatedAt 是毫秒精度，所以**同一毫秒内写入的多条消息，相对顺序不保证**。
	// 真实路径撞不上（用户敲字与模型生成之间隔着好几秒），但批量灌数据时要留意。
	ChunkMessages(chunkID string) ([]Message, error)

	// RecentMessages 取该人格最近的消息（跨会话、时间正序、最多 limit 条）——历史界面用它。
	RecentMessages(personaID string, limit int) ([]Message, error)

	// ListSessions 列出该人格的会话（时间倒序）。
	ListSessions(personaID string, limit int) ([]Session, error)

	// ListChunks 列出该会话的片（按 seq 正序）。
	ListChunks(sessionID string) ([]Chunk, error)

	// EndSession 给会话打上结束时间（ended_at）。
	//
	// 幂等：已经结束的会话再调一次不会改动它（条件是 ended_at IS NULL），
	// 因为"哪一轮算结束"是异步判断出来的，重复判定很常见。
	EndSession(sessionID string) error

	// DeletePersona 删除该人格的全部会话、片与消息。
	//
	// PG 侧本来有 ON DELETE CASCADE 兜着，但仍然显式删除：不让"数据被清掉"这件事
	// 依赖一个看不见的外键行为，也让内存实现有同一条路径可走。
	DeletePersona(personaID string) error
}

// NowMillis 是取当前时间的统一入口，便于测试替换（保持与项目其他部分同一口径：Unix 毫秒）。
func NowMillis() int64 { return time.Now().UnixMilli() }
