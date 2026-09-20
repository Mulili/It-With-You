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

// ContextMessagesLimit 是拼上下文时单个会话最多带上的消息条数。
//
// 这是个**兜底上限，不是策略**：真正的上下文边界由"会话切分"给出（一段话题聊完就结束），
// 正常会话远短于这个数。它防的是极端情况——比如窗口挂着连聊几小时、几千条消息全发出去，
// 那会直接顶爆模型上下文。
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

// Message 是会话里的一条消息。
type Message struct {
	// ID 由存储层生成（uuid）。
	//
	// 注意它与流式事件的"轮次 ID"不是一回事：后者只在进程内标识一轮回复
	//（前端据此过滤上一个请求的残留片段），不落库、也不需要持久。
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
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
// EnsureSession 之所以带副作用（没有就建）：调用方要的是"这一轮该写进哪个会话"，
// 而不是"有没有会话"。把"没有就建"推给调用方，等于每个调用点都要处理这个分支；
// 而"最近一个未收尾的会话"只有存储层能高效判断（PG 上有对应的部分索引）。
type Store interface {
	// EnsureSession 返回该人格最近一个未收尾的会话；没有就新建一个。
	EnsureSession(personaID string) (Session, error)
	// AppendMessage 追加一条消息，返回消息 ID（m.ID 为空时由实现生成）。
	AppendMessage(m Message) (string, error)
	// MessagesOf 按会话取消息（时间正序）——拼上下文用它。
	MessagesOf(sessionID string) ([]Message, error)
	// RecentMessages 取该人格最近的消息（跨会话、时间正序、最多 limit 条）——历史界面用它。
	RecentMessages(personaID string, limit int) ([]Message, error)
	// ListSessions 列出该人格的会话（时间倒序）。
	ListSessions(personaID string, limit int) ([]Session, error)
	// DeletePersona 删除该人格的全部会话与消息。
	//
	// PG 侧本来有 ON DELETE CASCADE 兜着，但仍然显式删除：不让"数据被清掉"这件事
	// 依赖一个看不见的外键行为，也让内存实现有同一条路径可走。
	DeletePersona(personaID string) error
}

// NowMillis 是取当前时间的统一入口，便于测试替换（保持与项目其他部分同一口径：Unix 毫秒）。
func NowMillis() int64 { return time.Now().UnixMilli() }
