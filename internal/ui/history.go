package ui

// 历史记录走「拉」模式：前端打开菜单时调一次 App.History()，拿到快照渲染。
// 不做推送的理由：历史变化频率低（一轮对话才变一次），而菜单是偶尔打开的，
// 为它维护一套增量事件是白付复杂度。
//
// 这里定义的是「用户可见的对话」，与发给模型的消息（llm.Message）是两件事。
// 将来的人格 System Prompt、检索到的记忆片段只进 llm.Message，不进 HistoryItem，
// 所以用户永远不会在菜单里看到系统内容——这是把展示模型独立出来的主要收益。

// HistoryDisplayLimit 是单次返回给前端的最大条数，超出部分丢掉最旧的。
//
// 只限制「展示」，不限制内部记录本身：记录该留多久是上下文策略问题
// （涉及 token 预算），留到阶段4 与记忆检索一起定。
const HistoryDisplayLimit = 128

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
	// ID 唯一标识一条记录，取自与流式事件同一套进程内自增编号。
	//
	// 注意它只在当前进程内唯一：阶段2.5 不持久化，重启即空。
	// 阶段3 / 阶段4 落库时必须换成稳定 ID，不要拿它当主键。
	ID   string `json:"id"`
	Role string `json:"role"` // user / assistant
	Text string `json:"text"`
	// At 是这条消息发生的时刻，Unix 毫秒。
	At int64 `json:"at"`
	// Status 取值见上面的常量。
	Status string `json:"status"`
}
