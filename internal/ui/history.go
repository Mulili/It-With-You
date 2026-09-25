package ui

// 历史记录走「拉」模式：前端打开菜单时调一次 App.History()，拿到快照渲染。
// 不做推送的理由：历史变化频率低（一轮对话才变一次），而菜单是偶尔打开的，
// 为它维护一套增量事件是白付复杂度。
//
// 菜单是**两级**的：先列会话（HistorySession），点进去再拉那一条会话的消息
// （App.SessionMessages）。为什么要分两级而不是一把全带上：会话最多 50 条、
// 每条最多 300 条消息，一次拉全就是上万行 JSON，而用户通常只想看其中一条。
//
// 这里定义的是「用户可见的对话」，与发给模型的消息（llm.Message）是两件事。
// 将来的人格 System Prompt、检索到的记忆片段只进 llm.Message，不进 HistoryItem，
// 所以用户永远不会在菜单里看到系统内容——这是把展示模型独立出来的主要收益。

// SessionMessagesLimit 是展开一条会话时最多返回的消息条数，超出取**尾部**。
//
// 取尾部而不是开头：与 ContextMessagesLimit 同一个取舍——"最后发生的事"比开场寒暄更值得看，
// 也符合聊天界面"默认停在最新"的习惯。一个话题聊一整天时消息可能上千条。
const SessionMessagesLimit = 300

// HistorySession 是历史菜单里的第一级：一次"从聊起来到聊完"的会话。
type HistorySession struct {
	ID string `json:"id"`
	// Title 由结算在会话收尾时生成（懒结算），所以**未收尾的会话标题是空的**。
	// 前端要给"进行中 / 未命名"一个兜底显示，别把空字符串直接摆出来。
	Title     string `json:"title"`
	StartedAt int64  `json:"startedAt"`
	// EndedAt 为 0 表示这段还在进行中（正在聊的就是它）。
	EndedAt int64 `json:"endedAt"`
}

// 条目状态。
//
// user 条目天然是完整的，固定 StatusOK；assistant 条目只有正常说完才是 StatusOK，
// 被用户打断（点「停止」或直接发下一句）的记 StatusCanceled。
// 前端据此把半截回复标出来，否则用户看到一句戛然而止的话会当成 bug。
const (
	StatusOK       = "ok"
	StatusCanceled = "canceled"
)

// HistoryItem 是菜单里显示的一条对话。
type HistoryItem struct {
	// ID 是这条消息在库里的 ID（uuid）。
	//
	// 注意：阶段2.5 时它还是进程内自增编号、重启即空，现在已经换成稳定主键了
	// （见 app 层 SessionMessages 的映射），前端拿它当 key 或用来定位都是安全的。
	ID   string `json:"id"`
	Role string `json:"role"` // user / assistant
	Text string `json:"text"`
	// At 是这条消息发生的时刻，Unix 毫秒。
	At int64 `json:"at"`
	// Status 取值见上面的常量。
	Status string `json:"status"`
}
