package ui

// ContextStat 是"这一轮的上下文由什么构成"，给界面顶部的条状图用。
//
// 为什么要分段、而不是只给一个总数：用户真正想分清的是
// **回忆是"被注入进来的"还是"本来就在历史里"** ——
//   - 注入的（Recall）每轮临时插入、**不进历史**，受检索阈值与限流管；
//   - 历史里的（History）是这一片的消息，只要在片内就一直读得到，不受任何阈值影响。
//
// 两者的表现完全不同，所以条上必须是两段不同的颜色。
//
// 单位一律是**字符数**（不是 token）：本项目的所有预算（片容量、人格注入预算）
// 也都按字符算，保持一致。字符数比 token 数大 1~2 倍（中文），看比例用足够了。
type ContextStat struct {
	// Persona 是人格提示词（含「她学到的」）。它每轮都带、不随对话增长
	Persona int `json:"persona"`
	// History 是片内历史消息。它才是"随聊天长大"的那一段
	History int `json:"history"`
	// Recall 是**本轮注入的回忆**（临时插入、不进历史）。这条是用户最关心的
	Recall int `json:"recall"`
	// Prompt 是本轮用户说的话（历史里的最后一条，单独拆出来便于看清）
	Prompt int `json:"prompt"`

	// Total 是四段之和，也就是这一轮真正发出去的体量
	Total int `json:"total"`

	// RecallFacts / RecallChunks / RecallRules 是回忆块里各有几条，用来看"注入量从哪来"
	RecallFacts  int `json:"recallFacts"`
	RecallChunks int `json:"recallChunks"`
	// RecallRules 是**归档规则**（"你以前的做法"）的条数——它也是注入来的，
	// 但来源与前两者不同：不是记忆库的事实，而是人格里被收起来的那部分
	RecallRules int `json:"recallRules"`

	// Capacity 是"满"的参照：片的字符上限。到它就在下一条用户消息前切开，
	// 所以它比模型窗口更贴近用户感受到的"上下文有多满"。
	Capacity int `json:"capacity"`
	// Messages / MessageLimit 是"条数"那个闸门——它与字符数**互补**
	// （一屏"嗯""哈哈"字符很少却能塞很多条，那时先撞上的是条数）。
	Messages     int `json:"messages"`
	MessageLimit int `json:"messageLimit"`
}
