package ui

// "它记得什么"界面（阶段4 第 5 步）走的是与历史同一套姿态：**拉**模式——
// 打开菜单时调一次 App.Memories()，删一条之后再拉一次。不推送的理由也一样：
// 记忆只在收尾结算时批量产生，变化频率很低。

// MemoryDisplayLimit 是"它记得什么"列表一次最多返回的条数。
//
// 只限制**展示**，不代表库里只留这么多：记忆留多少是长期策略问题，
// 与"界面一屏能看几条"是两件事。
const MemoryDisplayLimit = 200

// MemoryItem 是"它记得什么"列表里的一条。
//
// 字段刻意比 memory.Memory 少：这里只放**用户需要看见**的东西。
// 尤其 PersonaID 不直接给出去，换成一个布尔——用户关心的是"这条是关于我的，
// 还是你们俩之间的"，而不是谁的主键（一串 uuid 摆在那儿既没有信息量，
// 也会让人以为能拿它做什么）。
type MemoryItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	// Kind 取值 fact / preference / event / promise（见 internal/memory 的常量）。
	//
	// 这里给英文原值、由界面自己映射成中文：四个值很稳定，
	// 为它单开一个元数据接口（像 SlotSpec 那样）不划算。
	// 界面遇到不认识的值应当原样显示，而不是留空——将来加了新类型也看得见。
	Kind string `json:"kind"`
	// Private 为 true 表示这是**本人格私有**的经历（"你们之间的事"）；
	// false 表示公共事实（关于用户本人，换个人格也仍然成立）。
	Private   bool  `json:"private"`
	CreatedAt int64 `json:"createdAt"`
}
