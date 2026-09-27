package app

import (
	"fmt"
	"log"

	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/ui"
)

// 这个文件装「给界面的数据绑定」：历史、记忆、应用级设置。
//
// 它们的共同点是"读为主、失败也不值得打断对话"——菜单是"看一眼"的地方，
// 弹一个错误框比少显示几条更打扰。所以这里的读方法失败一律返回空 + 记日志，
// 只有真正的写操作（删记忆、存开关）才把错误交给前端。

// ---------- 应用级设置（⑨）----------

// AppSettings 是「设置」浮层需要的全局设置。
//
// 刻意与 persona.Snapshot 分开：那个快照装的是"某个人格"的数据，而思考开关不属于任何人格，
// 混进去会让"读某个人格的快照"顺带读到全局状态。
type AppSettings struct {
	// ThinkingDisabled 为 true 表示用户关掉了思考模式；默认 false = 跟随官方默认（思考开启）
	ThinkingDisabled bool `json:"thinkingDisabled"`
}

// GetSettings 返回全局设置。
func (a *App) GetSettings() AppSettings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AppSettings{ThinkingDisabled: a.thinkingDisabled}
}

// SetThinkingDisabled 保存思考模式开关。
//
// 关掉它的收益（见 operation.md 问题7）：首字更快（不必先算完思维链）、思考 token 不再计费，
// 且 temperature / top_p 这些"跳脱感"旋钮恢复生效——思考模式下它们会**静默失效**。
//
// 默认不关：这是用户的偏好，不替他做决定。
// 只作用于对话通路（Ask）；内部抽取（handleDirective）不受影响，那条链路靠准确性吃饭。
func (a *App) SetThinkingDisabled(disabled bool) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	if err := a.personas.SetThinkingDisabled(disabled); err != nil {
		return err
	}

	a.mu.Lock()
	a.thinkingDisabled = disabled
	a.mu.Unlock()

	state := "已开启思考模式"
	if disabled {
		state = "已关闭思考模式"
	}
	log.Printf("[app] %s（只影响对话，人格抽取不受影响）", state)
	return nil
}

// ---------- 历史与记忆（第 5 步的界面数据）----------

// History 返回当前人格的会话列表（新的在前）——菜单里的第一级。
//
// 按人格过滤是刻意的：人格在用户眼里是独立个体，A 的对话不该出现在 B 的历史里。
// 为什么按会话分组而不是平铺消息：一次完整对话才是"一件事"，
// 标题由结算生成（"那次聊冰棍"），拿它当入口比重翻一堆消息好用得多。
//
// 读失败返回空列表而不是错误：菜单是"看一眼"的东西，为它弹错误反而更吵；
// 真出错时日志里有完整原因。
func (a *App) History() []ui.HistorySession {
	if a.history == nil {
		return nil
	}
	rows, err := a.history.ListSessions(a.activePersonaID(), history.DefaultSessionLimit)
	if err != nil {
		log.Printf("[history] 读取会话列表失败: %v", err)
		return nil
	}
	out := make([]ui.HistorySession, 0, len(rows))
	for _, s := range rows {
		out = append(out, ui.HistorySession{
			ID:        s.ID,
			Title:     s.Title,
			StartedAt: s.StartedAt,
			EndedAt:   s.EndedAt,
		})
	}
	return out
}

// SessionMessages 返回某条会话里的消息（时间正序）——菜单里的第二级。
//
// 超长会话只给尾部，见 ui.SessionMessagesLimit。读不到时返回空而不是报错：
// 菜单是"看一眼"的地方，弹一个错误框比少显示几条更打扰。
func (a *App) SessionMessages(sessionID string) []ui.HistoryItem {
	if a.history == nil || sessionID == "" {
		return nil
	}
	rows, err := a.history.SessionMessages(sessionID, ui.SessionMessagesLimit)
	if err != nil {
		log.Printf("[history] 读取会话消息失败: %v", err)
		return nil
	}
	out := make([]ui.HistoryItem, 0, len(rows))
	for _, m := range rows {
		out = append(out, ui.HistoryItem{
			ID:     m.ID,
			Role:   m.Role,
			Text:   m.Content,
			At:     m.CreatedAt,
			Status: m.Status,
		})
	}
	return out
}

// Memories 返回当前人格**能看见**的记忆（公共 + 它私有的），给"它记得什么"列表用。
//
// 让用户看见这些是有意的：记忆是自动抽出来的，"她到底记住了什么"必须可查、可删——
// 否则抽错一条就只能忍着，或者把整个库清掉。
func (a *App) Memories() []ui.MemoryItem {
	if a.memories == nil {
		return nil
	}
	rows, err := a.memories.List(a.activePersonaID(), ui.MemoryDisplayLimit)
	if err != nil {
		log.Printf("[memory] 读取记忆失败: %v", err)
		return nil
	}
	out := make([]ui.MemoryItem, 0, len(rows))
	for _, m := range rows {
		out = append(out, ui.MemoryItem{
			ID:        m.ID,
			Content:   m.Content,
			Kind:      m.Kind,
			Private:   m.PersonaID != "",
			CreatedAt: m.CreatedAt,
		})
	}
	return out
}

// DeleteMemory 让伴侣忘掉一条记忆。
//
// 删掉就是真删（不留 tombstone）：去重是"相似度 + 类型"判定的，留一条软删除的记录
// 会让它继续参与去重，于是刚被删掉的内容会被下一次结算"重新学回来"——那才是最让人困惑的行为。
func (a *App) DeleteMemory(id string) error {
	if a.memories == nil {
		return fmt.Errorf("记忆存储未就绪")
	}
	if id == "" {
		return fmt.Errorf("没有指定要删的记忆")
	}
	if err := a.memories.Delete(id); err != nil {
		return fmt.Errorf("删除记忆失败: %w", err)
	}
	log.Printf("[memory] 用户删掉了一条记忆（%s）", id)
	return nil
}
