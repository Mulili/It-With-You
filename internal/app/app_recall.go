package app

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
)

// ---------- 检索注入（第 4 步）----------
//
// 这一组参数**都是拍的**，必须用真实对话校。校准依据是每轮那行 `[recall]` 日志：
// 里面打了候选的最高相似度，攒几十轮就能看出"相关"与"无关"的真实分界在哪。
const (
	// RecallTimeout 是检索这一段的超时。
	//
	// 它跑在**关键路径**上（拼上下文之前），所以给得比嵌入器自己的默认超时短得多。
	// 这个 3 秒是掐着实测数字定的：Ollama 空闲 5 分钟会卸载模型，下一次嵌入要先把它读回来
	// （实测冷 2.23s / 热 0.05s，`ollama ps` 的 UNTIL 列就是那个 5 分钟）。
	// 也就是说冷启动本身就要 2.2 秒——这一档刚够单发、不够并发，所以失败必须有人兜底：
	// 见 recall 里失败后的 warmEmbedder（宁可这一轮不回忆，也不把 2 秒加在用户按回车之后）。
	RecallTimeout = 3 * time.Second
	// EmbedWarmTimeout 是后台预热的上限。它**不在关键路径上**，所以给得宽松：
	// 这一档要装下"模型冷启动 + 一次完整嵌入"，宁可慢也不能失败。
	EmbedWarmTimeout = 30 * time.Second
	// recallQueryMessages 是拼查询用的最近消息条数。
	// 只用最后一句的话，"嗯""在吗"这种短句会检出噪声；多带几句上下文，话题信号强得多。
	recallQueryMessages = 4
	// recallTopN 是每条线各取几条候选（取回来再按阈值筛）
	recallTopN = 3
	// recallMinScore 是"多像才算相关"的下限。
	//
	// **0.55 是用户真机调出来的（2026-09-28），实测"十分优秀"** —— 这是最终依据。
	//
	// ⚠️ 我自造样本测出的分布（**仅作量级参考，不要拿它调参**）：
	//   - 完全无关的短句对          0.37
	//   - 话题近邻（雪糕 vs 冰棍）   0.48
	//   - 纯语气词噪声（嗯嗯，好呀） 0.52
	// 我据此测出"用户直接问起只有 0.532"、还建议过降到 0.45；而用户的真机数据是**能打到 0.7**
	// （问得直接、字面重合多）。自造样本**系统性偏低**——我把无关内容全塞进了一句里，
	// 而真实问法往往是分开的短句。**教训：阈值只能靠真机日志校，自造样本会误导。**
	//
	// ⚠️ 另外，"减少重复"不靠这个阈值：重复来自她主动回扣，
	// 管它的是措辞降档（formatRecall 的分档）与 recallCountPenalty。
	recallMinScore = 0.55
	// recallCountPenalty 是"这段对话里提过之后，命中门槛最多抬多高"。
	//
	// **0.12 是用户真机手调后的值（2026-09-28）**，判据与 recallMinScore 同一份真机观察：
	// **门槛上限 = 0.55 + 0.12 = 0.67**，仍压在"直接提问"那一档（约 0.7）之下——
	// 再高就会把她该答得上的那一档也挡掉。
	//
	// ⚠️ 这里的 0.67 是**渐近上限**，曲线趋近而不达到（见 recallCountDecay）：
	// 也就是说"提过很多次"之后，只有相似度接近 0.67 的才还会被注入。
	//
	// 我早先按自造样本（0.532）建议过 P = 0.06、甚至 < 0.08，那些数**与真机分布不符**——
	// 真机上"直接提问"能打到 0.7，增幅太小就压不住重复。与 recallMinScore 是同一条教训。
	recallCountPenalty = 0.12
	// recallCountDecay 决定抬升的快慢：越大则"第一次提过"抬得越狠。
	//
	// 0.63 配上 0.12 的形状是"先陡后缓"：
	//   提过 1 次 → +0.056（门槛 0.606）
	//   提过 3 次 → +0.102（门槛 0.652）
	//   之后趋近 +0.12（门槛 0.67）
	recallCountDecay = 0.63
	// recallMemoryLimit 是最多注入几条事实。事实很短（一句话），几条不占地方。
	recallMemoryLimit = 3
	// recallChunkLimit 是最多注入几段往事。
	//
	// 只给 1：一条片摘要就有两千字上下，"同时想起两段往事"在真实对话里很少见，
	// 而代价是双倍的注入预算。要放更多就改这个数（候选已经按相似度排好）。
	recallChunkLimit = 1
)

// recall 是本轮的"想起"：拿当下的话题去记忆库里问两条线，拼成一段可注入的文本。
//
// 两条线（见 operation.md「检索」）：
//   - **片索引线**给"经历"——你们以前聊过什么，命中后注入那一段的抽取式摘要
//   - **事实线**给"背景"——关于他的事（公共）与你们之间的事（本私有）
//
// 它跑在关键路径上，所以**只做一次嵌入 + 两次向量查询，不加任何 LLM 调用**：
// 加一次 1~2 秒的"该不该提这件事"的判断，会让每一轮回复都慢半拍；
// 而"要不要说出口"本来就可以交给模型在生成时自己决定——注入块里明确写了允许忽略。
//
// 任何一步失败都只是"这轮少想起一件事"，绝不让对话失败：记忆是加分项，不是必需项。
//
// sessionID 是**当前会话**，用来判断"这条我已经提过了吗"——**判据只在措辞上用，不在检索层面过滤**：
//   - 检索照常返回全部候选（包括本段对话里提过的、以及本会话自己的片）；
//   - 提过的那些改用**弱化措辞**注入："只当背景资料收着，除非他问起否则别提"。
//
// 为什么不在检索层面丢掉：丢了她就**不知道**，然后开始编。用户举过一个反例——
// 连着问"草莓 / 芒果 / 菠萝三种口味喜不喜欢"，第三次起若不再注入，她会答"喜欢"。
// **编出来的答案比重复提一句有害得多。**
// 返回值里的 Refs 是"编号 → ID"对照表，供 stream 解析她自标的 [[used:…]] 用——
// 那是计数归口的唯一依据（见 markUsed）。
func (a *App) recall(personaID, sessionID string, msgs []llm.Message) recallResult {
	if a.memories == nil || a.embedder == nil {
		return recallResult{} // 记忆功能整体停用（见 App.embedder 的注释）
	}
	query := recallQuery(msgs)
	if query == "" {
		return recallResult{}
	}

	ctx, cancel := context.WithTimeout(a.ctx, RecallTimeout)
	defer cancel()
	vecs, err := a.embedder.Embed(ctx, []string{query})
	if err != nil {
		log.Printf("[recall] 查询嵌入失败，本轮不回忆: %v", err)
		// 失败最可能的原因是"Ollama 空闲把模型卸了"（默认 5 分钟），下一次嵌入要先加载它。
		// 这一轮不等（等下去就是把 2 秒加在用户按回车之后），改成后台补一次：
		// 那次不赶时间，把模型加载起来之后，后面几轮就都是几十毫秒。
		go a.warmEmbedder()
		return recallResult{}
	}

	// 两条线各自独立取候选：一条挂了不影响另一条。
	// 取 recallTopN 的**三倍**：提过多次的那些会被抬高的门槛挡掉（见 recallThreshold），
	// 让它们占掉名额就等于"越提越轮不到新的"。
	memHits, err := a.memories.Search(personaID, vecs[0], recallTopN*3)
	if err != nil {
		log.Printf("[recall] 检索事实失败: %v", err)
	}
	chunkHits, err := a.memories.SearchChunks(personaID, vecs[0], recallTopN*3)
	if err != nil {
		log.Printf("[recall] 检索往事失败: %v", err)
	}

	// 逐条判门槛。**不能"遇到第一条低于就 break"**：门槛逐条不同——
	// 没提过的用基础阈值，提过的按次数抬高，所以排在后面的反而可能过线。
	var mems []memory.MemoryHit
	for _, h := range memHits {
		if len(mems) >= recallMemoryLimit {
			break
		}
		if h.Score < recallThreshold(h.Memory.LastRecalledSession, h.Memory.RecallCount, sessionID) {
			continue
		}
		mems = append(mems, h)
	}
	var chunks []memory.ChunkHit
	for _, h := range chunkHits {
		if len(chunks) >= recallChunkLimit {
			break
		}
		if h.Score < recallThreshold(h.Chunk.LastRecalledSession, h.Chunk.RecallCount, sessionID) {
			continue
		}
		chunks = append(chunks, h)
	}

	// 这一行是**校准阈值的依据**：把候选的最高相似度一直打出来，
	// 才能回答"recallMinScore 该定多少"（现在那个 0.5 是拍的）
	memTop, chunkTop := 0.0, 0.0
	if len(memHits) > 0 {
		memTop = memHits[0].Score
	}
	if len(chunkHits) > 0 {
		chunkTop = chunkHits[0].Score
	}
	log.Printf("[recall] 候选最高相似度 事实 %.3f / 往事 %.3f；注入 %d 条事实、%d 段往事",
		memTop, chunkTop, len(mems), len(chunks))

	// 再逐条打出候选与去向。上面那行只有分数，**校准不了阈值**——
	// 知道"0.47 被筛掉了"没有用，得知道**那 0.47 是什么内容**：
	// 是"差一点就该想起来的往事"（说明阈值该降），还是"纯噪声"（说明该升）。
	//
	// 门槛那一列里，"提过被挡"是"越提越难注入"这条曲线的**唯一可观测证据**：
	// 它挡掉了多少、挡在什么分数上，就是校准 recallCountPenalty 的依据。
	for _, h := range memHits {
		th := recallThreshold(h.Memory.LastRecalledSession, h.Memory.RecallCount, sessionID)
		log.Printf("[recall]   %s 事实 %.3f（门槛 %.2f，本次已提 %d 次）：%s",
			recallVerdict(h.Score, th), h.Score, th, h.Memory.RecallCount, clip(h.Memory.Content, 40))
	}
	for _, h := range chunkHits {
		th := recallThreshold(h.Chunk.LastRecalledSession, h.Chunk.RecallCount, sessionID)
		log.Printf("[recall]   %s 往事 %.3f（门槛 %.2f，本次已提 %d 次）：%s（%s）",
			recallVerdict(h.Score, th), h.Score, th, h.Chunk.RecallCount,
			clip(h.Chunk.Summary, 40), relativeDay(h.Chunk.CreatedAt))
	}

	if len(mems) == 0 && len(chunks) == 0 {
		return recallResult{}
	}

	// ⚠️ **这里刻意不记"注入过"。** 计数归口在 markUsed —— 只有她真的在回复里用到了才 +1。
	//
	// 为什么：一条记忆被人看了十轮、却一次都没提，按"注入就记"的话计数照样涨到十、
	// 门槛升到顶——**你以后问起它就再也拿不到了**，尽管它从没造成过任何重复。
	// 用户的原话是："按照采取次数累加，llm 认为这个记忆应该采取时才累加。"
	text, refs := formatRecall(mems, chunks, sessionID)
	return recallResult{Text: text, Refs: refs, Facts: len(mems), Chunks: len(chunks)}
}

// warmEmbedder 在后台补一次嵌入，目的只是让 Ollama 把模型重新读进显存。
//
// 为什么需要它：Ollama 默认**空闲 5 分钟就卸载模型**（`ollama ps` 的 UNTIL 列能看到），
// 而下一次嵌入要先把它加载回来（实测冷 2.23s / 热 0.05s）。这 2 秒落在关键路径上
// 就是把等待加在用户按回车之后，所以 recall 宁可这一轮不回忆，由这里把模型烘热。
//
// 抢不到锁就跳过：说明已经有一次预热在跑，重复发只会让 Ollama 排队。
// 这也让它天然不会堆积——无论失败多少次，同一时刻最多一个预热。
func (a *App) warmEmbedder() {
	if a.embedder == nil || !a.warmMu.TryLock() {
		return
	}
	defer a.warmMu.Unlock()

	ctx, cancel := context.WithTimeout(a.ctx, EmbedWarmTimeout)
	defer cancel()
	if _, err := a.embedder.Embed(ctx, []string{"预热"}); err != nil {
		log.Printf("[recall] 后台预热嵌入失败: %v", err)
		return
	}
	log.Printf("[recall] 嵌入模型已预热，后面几轮可以正常回忆")
}

// recallQuery 把最近几句拼成检索用的查询文本。
//
// 用尾部若干条而不是只有最后一句：单句常常短到没有话题信号（"嗯""在吗"），
// 拼上前面几轮才问得出"我们现在在聊什么"。
func recallQuery(msgs []llm.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	tail := msgs
	if len(tail) > recallQueryMessages {
		tail = tail[len(tail)-recallQueryMessages:]
	}
	var b strings.Builder
	for _, m := range tail {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// recallRef 是注入块里某一条编号指向的东西。
//
// 模型会在回复末尾自标"用到了第几条"（见 formatRecall 末尾那段要求），我们凭这个对照表
// 把序号还原成 ID —— **只给真正被用到的那些累加计数**。
//
// 为什么让模型回序号、而不是直接回 ID：抄 uuid 它一定会抄错，而"2"不会。
// 代价是这张表必须与注入文本逐行对应（渲染时同步 append，是唯一可靠的做法）。
type recallRef struct {
	MemoryID string // 非空 = 这是一条事实
	ChunkID  string // 非空 = 这是一段往事
}

// recallResult 是一次检索的产物。
type recallResult struct {
	// Text 是要塞进 system 消息的注入文本；空串表示这一轮什么都没想起来
	Text string
	// Refs[i] 对应注入文本里编号 i+1 的条目
	Refs []recallRef
	// Facts / Chunks 是这一次"想起来"各注入了几条，给界面顶部的状态条用。
	// 与 Refs 的区别：Refs 是"编号 → ID"的对照表（含降档那一段），这两个只是计数。
	Facts  int
	Chunks int
}

// formatRecall 把命中的两条线渲染成一段注入文本，并给出"编号 → ID"的对照表。
//
// 四处写法是有意的：
//   - 开头明说"可能无关、没关系就别提"——否则她会把每一条都当任务汇报一遍（最容易翻车的地方）
//   - 带**时间**（"昨天""3 天前"）：摘要本身不带时间，而"上次""前几天"这类说法全靠它，
//     让模型自己从毫秒时间戳里推算是不现实的
//   - 空的那条线不出标题：只写"以前聊过："却什么也没有，是在引导她编
//   - **本段对话里已经提过一遭的单独成段**，并附上"别再主动提、他问起才答"的限定。
//     这一档不能简单地"不注入"：用户主动问起时她得答得上细节——而那正是最需要它的时刻。
func formatRecall(mems []memory.MemoryHit, chunks []memory.ChunkHit, sessionID string) (string, []recallRef) {
	// 拆成两组：本段对话里**还没提过**的、和**已经提过一遭**的（后者用弱化措辞）
	var freshMems, saidMems []memory.MemoryHit
	for _, h := range mems {
		if sessionID != "" && h.Memory.LastRecalledSession == sessionID && h.Memory.RecallCount > 0 {
			saidMems = append(saidMems, h)
		} else {
			freshMems = append(freshMems, h)
		}
	}
	var freshChunks, saidChunks []memory.ChunkHit
	for _, h := range chunks {
		if sessionID != "" && h.Chunk.LastRecalledSession == sessionID && h.Chunk.RecallCount > 0 {
			saidChunks = append(saidChunks, h)
		} else {
			freshChunks = append(freshChunks, h)
		}
	}

	var b strings.Builder
	// refs[i] 对应注入文本里编号 i+1 的条目。编号**跨分组连续**地编，
	// 因为模型只需要回一个小整数。务必与下面的渲染逐行同步 append——这是它唯一可靠的来源。
	var refs []recallRef

	b.WriteString("（下面是你脑子里冒出来的东西，可能和现在有关、也可能无关。\n")
	b.WriteString("有关系就自然地带一句；没关系就当没想起来，别硬提，也别当成任务汇报。）\n")
	if len(freshMems) > 0 {
		b.WriteString("\n你记得关于他的事：\n")
		for _, h := range freshMems {
			refs = append(refs, recallRef{MemoryID: h.Memory.ID})
			fmt.Fprintf(&b, "%d. %s\n", len(refs), h.Memory.Content)
		}
	}
	if len(freshChunks) > 0 {
		b.WriteString("\n以前聊过：\n")
		for _, h := range freshChunks {
			refs = append(refs, recallRef{ChunkID: h.Chunk.ChunkID})
			fmt.Fprintf(&b, "%d. %s，%s\n", len(refs), relativeDay(h.Chunk.CreatedAt), h.Chunk.Summary)
		}
	}

	if len(saidMems)+len(saidChunks) > 0 {
		// 措辞要**极其克制**，而且随"提过几次"加强——这就是用户要的那条曲线：
		// 第一次提过就明显降档（别马上重复），之后趋于平缓。
		//
		// 为什么是分档而不是连续的对数函数：**提示词只能落成文字**。
		// 给模型一个"权重 0.37"它无从下手，而"你已经反复提过很多次了"是它能直接照做的。
		// 所以把对数曲线的形状翻译成两档：整段一句"别提了"（覆盖所有"提过"的情形），
		// 再给"反复提过"的那些单独加一个标记。
		b.WriteString("\n（下面这些你这次已经提过了，**只当背景资料收着**：不要再主动提起，\n")
		b.WriteString("也不要用它们回扣话题；只有他明确问到时才照这些内容回答，\n")
		b.WriteString("而且别提「我们之前聊过」。）\n")
		for _, h := range saidMems {
			refs = append(refs, recallRef{MemoryID: h.Memory.ID})
			fmt.Fprintf(&b, "%d. %s%s\n", len(refs), repeatedNote(h.Memory.RecallCount), h.Memory.Content)
		}
		for _, h := range saidChunks {
			refs = append(refs, recallRef{ChunkID: h.Chunk.ChunkID})
			fmt.Fprintf(&b, "%d. %s%s，%s\n", len(refs),
				repeatedNote(h.Chunk.RecallCount), relativeDay(h.Chunk.CreatedAt), h.Chunk.Summary)
		}
	}

	// 让模型自标"用到了哪几条"。**这是计数归口的全部依据**——
	// 只给真正被用到的加一次，而不是"注入过就记"（见 markUsed）。
	//
	// 用序号而非 ID：抄 uuid 必错，而"2"不会。要求写在最末，是为了让解析端能简单地
	// "见到标记就把后面的全丢掉"（见 stripRecallUsage）。
	if len(refs) > 0 {
		b.WriteString("\n（如果上面有哪几条确实帮到了你、你在回复里用到了，\n")
		b.WriteString("就在回复的最末尾另起一行写：[[used:编号]]，多条用逗号隔开，例如 [[used:1,3]]。\n")
		b.WriteString("一条都没用到，就完全不要写这一行。）\n")
	}
	return strings.TrimRight(b.String(), "\n"), refs
}

// repeatedRecallThreshold 是"同一个话题被反复提起"的门槛。
//
// 取 3 是个自然的分界：提过一两次还在正常聊天范围内，
// 而从第三次起用户会明确感觉到"她怎么又提这个"。
const repeatedRecallThreshold = 3

// repeatedNote 给"反复提过"的条目加一句标记，其余返回空串。
//
// 只标"反复"这一档、不按次数逐档标注，是刻意的：
// 整段措辞已经表达了"提过"这件事，而"提过 5 次"与"提过 6 次"对模型没有区别——
// 再细分只是往提示词里灌噪声。这正是那条曲线的**后半段：趋于平缓**。
func repeatedNote(count int) string {
	if count >= repeatedRecallThreshold {
		return "（这条你这次已经反复提过很多次了）"
	}
	return ""
}

// relativeDay 把时间戳说成"人话"。
//
// 按**自然日**算差，而不是按 24 小时：昨晚 23 点聊完，今天早上再提"上次"，
// 该说"昨天"而不是"今天"。
func relativeDay(ms int64) string {
	if ms <= 0 {
		return "以前"
	}
	at := time.UnixMilli(ms).Local()
	now := time.Now()
	midnight := func(x time.Time) time.Time {
		return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, x.Location())
	}
	switch days := int(midnight(now).Sub(midnight(at)).Hours() / 24); {
	case days <= 0:
		return "今天"
	case days == 1:
		return "昨天"
	case days < 7:
		return fmt.Sprintf("%d 天前", days)
	default:
		return at.Format("2006-01-02")
	}
}

// chunkMessages 读出一个片的全部消息，转成发给模型的形式（时间正序）。
//
// limit 传 ContextMessagesLimit：那是给**上下文**兜底的上限。结算走的是另一条路
// （见 settleChunk），它要整片原文，不受这个上限约束。
func (a *App) chunkMessages(chunkID string) ([]llm.Message, error) {
	rows, err := a.history.ChunkMessages(chunkID, history.ContextMessagesLimit)
	if err != nil {
		return nil, fmt.Errorf("读取片消息失败: %w", err)
	}
	return toLLMMessages(rows), nil
}

// toLLMMessages 把存储层的消息投影成发给模型的消息。
//
// 只取 role 与 content：时间、状态这些是**给人看**的字段，而 llm.Message 会被整段
// 序列化进请求体，多带一个字段就多发一份无用数据（还可能让模型误解）。
func toLLMMessages(rows []history.Message) []llm.Message {
	out := make([]llm.Message, 0, len(rows))
	for _, m := range rows {
		out = append(out, llm.Message{Role: m.Role, Content: m.Content})
	}
	return out
}

// recallThreshold 返回这条候选的命中门槛。
//
// 设计意图是"权重越低越少**主动注入**"——把两件事分开：
//   - **是否注入**：门槛随"这段对话里提过几次"抬高，越提越难命中；
//   - **注入什么**：一旦命中，内容**一字不少**地给全。
//
// ⚠️ 抬升的幅度被实测卡得很死（不能超过 0.08），所以它目前只挡得住"话题近邻"那一档，
// 完整推理见 recallCountPenalty 的注释。
//
// 形状是"先陡后缓"：第一次提过就抬得明显，之后趋近
// recallMinScore + recallCountPenalty 这个上限。
func recallThreshold(recalledIn string, count int, sessionID string) float64 {
	if sessionID == "" || recalledIn != sessionID || count <= 0 {
		return recallMinScore
	}
	return recallMinScore + recallCountPenalty*(1-math.Exp(-recallCountDecay*float64(count)))
}

// recallVerdict 给日志用：这条候选是过线了、还是被门槛挡掉。
func recallVerdict(score, threshold float64) string {
	if score < threshold {
		return "筛掉"
	}
	return "注入"
}

// recallUsageOpen 是"用到了哪几条"那个标记的开头。模型被要求在回复**最末尾**写它。
const recallUsageOpen = "[[used:"

// safePrefixLen 返回 s 里可以**立刻推给前端**的长度。
//
// 两条判据：
//   - 出现了完整标记 → 它之前的部分可以推（标记及其之后一律不推）
//   - 没出现，但**尾部**正好是标记开头的一段前缀（"["、"[[use"…）→ 那几位先扣住，
//     等下一块拼齐再定。否则一个被切开的 "[[used:" 会先在界面上闪一下。
func safePrefixLen(s string) int {
	if i := strings.Index(s, recallUsageOpen); i >= 0 {
		return i
	}
	limit := len(recallUsageOpen) - 1
	if limit > len(s) {
		limit = len(s)
	}
	for n := limit; n > 0; n-- {
		if strings.HasPrefix(recallUsageOpen, s[len(s)-n:]) {
			return len(s) - n
		}
	}
	return len(s)
}

// stripRecallUsage 从完整输出里剥出她自标的"用到了哪几条"。
//
// 返回正文与用到的编号（编号是注入块里的序号，1 起）。
// ⚠️ 标记**及其之后**的内容一律丢掉：约定要求它写在最末尾，这样最省事，
// 也不怕模型在标记后还多写一句。模型没写标记时，正文原样返回、编号为空。
func stripRecallUsage(s string) (string, []int) {
	i := strings.Index(s, recallUsageOpen)
	if i < 0 {
		return cleanBody(s), nil
	}
	rest := s[i+len(recallUsageOpen):]
	end := strings.Index(rest, "]]")
	if end < 0 {
		// 标记没闭合（模型写漏了半个）——按"没标"处理，但正文里那半截仍要去掉
		return cleanBody(s[:i]), nil
	}
	var used []int
	for _, part := range strings.Split(rest[:end], ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			continue // 模型偶尔会写出非数字，忽略即可
		}
		used = append(used, n)
	}
	return cleanBody(s[:i]), used
}

// cleanBody 去掉正文末尾的空白与换行——标记前通常留着一个空行。
func cleanBody(s string) string {
	return strings.TrimRight(s, " \t\r\n")
}

// markUsed 只给她**真的用到了**的那些累加一次计数。
//
// 为什么不是"注入了就记"——见 recall 里那段注释：一条被看了十轮、一次都没提的记忆，
// 按"注入就记"会把门槛顶到最高，结果**你以后问起它反而拿不到了**。
//
// 编号越界、或模型写了不存在的编号，直接忽略：那是它的小失误，不该影响这一轮。
func (a *App) markUsed(refs []recallRef, used []int, sessionID string) {
	if len(used) == 0 || a.memories == nil {
		return
	}
	var memIDs, chunkIDs []string
	for _, n := range used {
		if n < 1 || n > len(refs) {
			continue
		}
		if r := refs[n-1]; r.MemoryID != "" {
			memIDs = append(memIDs, r.MemoryID)
		} else if r.ChunkID != "" {
			chunkIDs = append(chunkIDs, r.ChunkID)
		}
	}
	if len(memIDs) > 0 {
		if err := a.memories.MarkRecalled(memIDs, sessionID); err != nil {
			log.Printf("[recall] 记录记忆取用失败: %v", err)
		}
	}
	if len(chunkIDs) > 0 {
		if err := a.memories.MarkChunksRecalled(chunkIDs, sessionID); err != nil {
			log.Printf("[recall] 记录往事取用失败: %v", err)
		}
	}
}

// clip 把文本压成一行、并截到 n 个字符。只用于日志。
//
// 压掉换行是必须的：摘要里带换行，一条日志会散成好几行，扫起来反而更乱。
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
