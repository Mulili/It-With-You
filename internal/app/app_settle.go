package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/settle"
)

// TopicJudgeTimeout 是话题边界判断的超时。与指令抽取同级：
// 两者都是短小的 JSON 调用，慢过这个时间就没有意义了。
const TopicJudgeTimeout = 30 * time.Second

// SettleTimeout 是收尾结算的超时。比上面两个大一个量级是有原因的：
// 它们的输入只有几百 token，而这里的输入是**一整片原文**（约 2 万字符），
// 输出也要几百到上千 token（摘要 + 事实），慢是正常的。
const SettleTimeout = 90 * time.Second

// settlePerRun 是一次扫描最多结算几片。
//
// 有上限是因为"积压"是真实存在的：升级后第一次启动、或离线一段时间再打开，
// 待结算的片可能有好几片。一次全结算会在后台打出一串调用，而结算本来就不急——
// 每轮顺带补几片，很快就追平了。
const settlePerRun = 3

// CandidateTTL 是「她学到的」保留多久。
//
// 30 天是**拍的**，与 recallMinScore 一样要靠实际使用校。校准依据是 `[archive]` 日志里
// 每次清掉几条：一直清不完说明留得太久（界面会被一堆早就用不上的旧观察占满），
// 几乎不清说明太短（她刚学会的说法转眼就没了）。
const CandidateTTL = 30 * 24 * time.Hour

// judgeTopic 是每轮跑一次的后台判断：这一句是否结束了当前话题。
//
// 为什么每轮都调、不能像人格指令那样先做本地粗筛："话题结束"没有可靠的词面特征
// （详见 history.TopicPrompt 的说明）。为什么异步：串在回复后面会让用户每轮多等一两秒，
// 而判定滞后一轮完全无妨——结束掉的会话下一轮不会被复用，新会话自然开始。
func (a *App) judgeTopic(sessionID string, recent []llm.Message) {
	if a.history == nil || a.ctx == nil || len(recent) == 0 {
		return
	}
	// 抢不到锁说明上一次判断还没跑完，跳过这次（理由见 judgeMu 的注释）
	if !a.judgeMu.TryLock() {
		return
	}
	defer a.judgeMu.Unlock()

	ctx, cancel := context.WithTimeout(a.ctx, TopicJudgeTimeout)
	defer cancel()

	system, user := history.TopicPrompt(recent)
	out, err := a.chatJSON(ctx, system, user)
	if err != nil {
		log.Printf("[history] 话题判断调用失败: %v", err)
		return
	}

	v, err := history.ParseTopicVerdict(out)
	if err != nil {
		log.Printf("[history] 话题判断结果不可用: %v（模型原始输出：%s）", err, out)
		return
	}
	if !v.Ended {
		return // 话题还在继续，安静收场
	}

	if err := a.history.EndSession(sessionID); err != nil {
		log.Printf("[history] 结束会话失败: %v", err)
		return
	}
	log.Printf("[history] 话题结束，已收尾这一段会话（%s）", v.Reason)
}

// settlePending 是「收尾懒结算」：把已经聊完、却还没整理过的片整理成
// 片摘要 + 会话标题 + 待存事实，分头写进 session_chunks / sessions / memories / chunk_index。
//
// 为什么不挂在"话题结束那一刻"：那一刻本来就没人知道——结束是**下一轮**才判出来的。
// 于是搭别人的车：Ask 开头、以及每轮回复写完之后（见 stream）。
// 这与"懒归档"是同一个姿态：不引入常驻定时器，桌面应用会休眠，绝对时间的定时器不可靠。
//
// 触发点是"片"而不是"会话"：片写满时就会被切走并收尾，那时它就已经可以结算了，
// 不必等到整段会话聊完（会话可能一整天不结束）。
//
// 三个触发点（都是后台跑，不占关键路径）：
//  1. 启动时——补上次退出得急、或那几轮没赶上的
//  2. 每轮 Ask 开头——兜底
//  3. **每轮回复写完之后**（见 stream）——话题一收尾，那段内容当场离开上下文，
//     尽早结算才赶得上用户下一句的检索；这一条是最及时的
func (a *App) settlePending() {
	// 三个条件都要满足：没有历史就没得结算；没有记忆存储就没处写；
	// 没有嵌入服务则**整体停用**——抽了却嵌不出向量，等于白花一次调用。
	// 注意这是"记忆功能停用"，对话与人格不受任何影响。
	if a.history == nil || a.memories == nil || a.embedder == nil || a.ctx == nil {
		return
	}
	// 抢不到锁说明上一次还没跑完，跳过这次（理由同 judgeMu）
	if !a.settleMu.TryLock() {
		return
	}
	defer a.settleMu.Unlock()

	pending, err := a.history.PendingChunks(settlePerRun)
	if err != nil {
		log.Printf("[settle] 扫描待结算的片失败: %v", err)
		return
	}
	for _, c := range pending {
		// 这一片之前试过、输出不可用：不再重试（理由见 App.settleBad 的注释）
		if a.settleBadChunk(c.ID) {
			continue
		}
		if err := a.settleChunk(c); err != nil {
			log.Printf("[settle] 片 %s 结算失败: %v", c.ID, err)
			if errors.Is(err, settle.ErrUnusable) {
				a.markSettleBad(c.ID)
			}
		}
	}

	// 候选只由结算产生，所以结算跑完就是清理它们的最好时机（懒归档，不另起定时器）。
	a.pruneStaleCandidates()
}

// pruneStaleCandidates 清掉放太久没动过的「她学到的」。
//
// 它是**懒**的：不设定时器，搭两个本来就在跑的点——
//   - 启动时（应用关着的那段时间没有结算，候选不会自己过期）；
//   - 每次结算跑完之后（候选只由结算产生，见上）。
//
// **为什么只清候选，不按同样口径去降规则的层**：那个待做项写在「隐式演化会把东西攒进
// recent 层」这个前提上，而「基准 + 情调」那次改动之后**前提已经不成立**了——
// 隐式演化现在只写候选表，一个字都不往 persona_rules 写。规则那侧剩下的来源也不会无界增长：
// 单值槽位（称呼 / 语气 / 长度）写入即覆盖；多值槽位（禁忌 / 口头禅）只由用户明说或亲手提升产生。
//
// 而按时间淘汰规则**是有害的**：「以后叫我主人」落的正是 recent 层，
// 30 天后被降层，她就不叫了——用户看到的是"她忘了"，而这和"她真的忘了"无从分辨。
// 真要再收紧，正确顺序是先把「归档层接入检索」做完（降层之后还能被想起来），而不是现在先降。
//
// 失败只记日志：清理是维护动作，不该让启动或结算失败。
func (a *App) pruneStaleCandidates() {
	if a.personas == nil {
		return
	}
	before := time.Now().Add(-CandidateTTL).UnixMilli()

	// 先看清要删什么再删：清理**不可逆**，而这些条目正是 CandidateTTL 的校准依据。
	a.logStaleCandidates(before)

	n, err := a.personas.PruneStaleCandidates(before)
	if err != nil {
		log.Printf("[archive] 清理过期候选失败: %v", err)
		return
	}
	if n > 0 {
		// 一直有这行说明窗口太长，从来没它说明可能太短
		log.Printf("[archive] 清掉了 %d 条放太久没动过的「她学到的」", n)
	}
}

// logStaleCandidates 打出即将被清理的候选。
//
// 为什么要"先看一眼再删"：这些条目正是 CandidateTTL 的校准依据——如果被删的是
// "她其实还在用的那个称呼"，说明窗口太长（该放宽）；如果都是早就没再用过的，说明合适。
// 而**只打一个条数时，这两种情况在日志里长得一模一样**。
func (a *App) logStaleCandidates(before int64) {
	if a.personas == nil {
		return
	}
	for _, p := range a.personas.Snapshot().Personas {
		rows, err := a.personas.ListCandidates(p.ID, moodInjectLimit)
		if err != nil {
			continue // 读不到就当没有：清理本身照常进行，不因日志失败而中断
		}
		for _, c := range rows {
			if c.CreatedAt >= before {
				continue
			}
			log.Printf("[archive]   即将清掉：「%s」%s（%s 记下的）",
				persona.SlotLabel(c.Slot), clip(c.Value, 30), relativeDay(c.CreatedAt))
		}
	}
}

// settleChunk 结算一片。
//
// **写库顺序是有意的**：`session_chunks.summary` 放在最后写，它是"这一片已结算"的提交点。
// 在它之前的每一步都只做可重放的事——事实有去重兜底（相似度命中就更新那一条）、
// 片索引是覆盖写（chunk_id 是主键）、标题只写"还没有标题的那一次"。
// 于是中途失败不会留下半截状态：下一轮整体重放一遍就行。
//
// 反过来（先写摘要、再嵌向量）的话，嵌入一旦失败，这一片就再也扫不到了——
// 那些事实**永久丢失**，而且完全无声。
func (a *App) settleChunk(c history.Chunk) error {
	// limit 传 0 = 整片原文。不能用拼上下文那个上限：摘要的保真度上限就是原文的完整度，
	// 而短句闲聊很容易在 2 万字符里塞下 500 条以上消息
	rows, err := a.history.ChunkMessages(c.ID, 0)
	if err != nil {
		return err
	}
	msgs := toLLMMessages(rows)
	if len(msgs) == 0 {
		return nil // 空片（扫描时已排除），这里只是兜底
	}

	// 结算比话题判断重得多（输入是整片原文），所以自己的超时也更长
	ctx, cancel := context.WithTimeout(a.ctx, SettleTimeout)
	defer cancel()

	system, user := settle.Prompt(msgs)
	out, err := a.chatJSON(ctx, system, user)
	if err != nil {
		return fmt.Errorf("结算调用失败: %w", err)
	}

	res, err := settle.Parse(out, msgs)
	if err != nil {
		return fmt.Errorf("%w（模型原始输出：%s）", err, out)
	}

	summary := res.Summary()
	// 摘要与全部事实**一次嵌入算完**：逐条发请求就是十几次网络往返
	texts := make([]string, 0, 1+len(res.Facts))
	texts = append(texts, summary)
	for _, f := range res.Facts {
		texts = append(texts, f.Content)
	}
	vecs, err := a.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("嵌入失败: %w", err)
	}

	// 事实：about=user 的写公共（任何人格都该知道），about=persona 的写私有
	//（那是"我和他之间的事"，只属于这一段关系）
	for i, f := range res.Facts {
		personaID := ""
		if f.Private {
			personaID = c.PersonaID
		}
		// 一条存不进去就整体作废（不写摘要、留待重放）：去重让重放是幂等的——
		// 已经存下的那几条会被"更新"而不是各存一份，代价只是一次多余的调用。
		// 反过来（跳过失败那条继续）会让这条事实**永久丢失且无声**，
		// 与"错并不可逆"的取舍方向相反。
		if _, err := a.memories.Save(memory.Memory{
			PersonaID:  personaID,
			Content:    f.Content,
			Kind:       f.Kind,
			Importance: f.Importance,
			// 依据存**原话**而不是模型的转述：将来要核对"这条记忆是从哪儿来的"时，
			// 只有原话能回到原文里去对
			Evidence: f.Quote,
		}, vecs[1+i], memory.DefaultDedupThreshold); err != nil {
			return fmt.Errorf("保存记忆失败（%s）: %w", f.Content, err)
		}
	}

	// 行为规则候选：**只给能改的人格抽**（理由写在 addRuleCandidates 里）
	if len(res.Rules) > 0 {
		if err := a.addRuleCandidates(c.PersonaID, res.Rules); err != nil {
			return err
		}
	}

	// 片索引：检索时靠它找到"聊过的那件事"，再顺着 chunk_id 回表取原文
	if err := a.memories.IndexChunk(memory.ChunkIndex{
		ChunkID:   c.ID,
		SessionID: c.SessionID,
		PersonaID: c.PersonaID,
		Summary:   summary,
	}, vecs[0]); err != nil {
		return fmt.Errorf("写片索引失败: %w", err)
	}

	// 会话标题：只有第一片能写进去（后来者改不掉），所以失败也不值得中断结算
	if res.Title != "" {
		if err := a.history.SetSessionTitleIfEmpty(c.SessionID, res.Title); err != nil {
			log.Printf("[settle] 写会话标题失败: %v", err)
		}
	}

	// **提交点**：这一步成功之后，这一片就再也不会被扫到了
	if err := a.history.SetChunkSummary(c.ID, summary); err != nil {
		return fmt.Errorf("写片摘要失败: %w", err)
	}

	log.Printf("[settle] 已结算片 %s（要点 %d / 事实 %d / 行为倾向 %d / 未了 %d / 丢弃 %d）",
		c.ID, len(res.KeyPoints), len(res.Facts), len(res.Rules), len(res.Unresolved), res.Dropped)
	return nil
}

// addRuleCandidates 把这次抽出来的行为倾向写进候选表。
//
// 写进去就**已经在生效了**（注入时以【偶尔可以这样】的措辞带进上下文），所以这里不是
// 在攒一张待办清单。用户随后在界面上看到的是"她已经会了"，可以提升成基准，也可以删掉。
//
// 为什么只给**可写人格**抽：内置人格的规则是作者定稿的，不该被她自己悄悄改歪；
// 而"想在它身上改点什么"走的是另一条路——对内置人格提要求时，handleDirective 会先把它复制成
// 一份「我的」（那是用户主动发起的显式意图）。隐式演化没有这个授权，所以这里只跳过、不复制：
// 一次结算就悄悄多出一个新人格，用户会以为出了 bug。
//
// 写不进去就整体作废（与事实同一条理由）：静默少几条只会让人觉得"它怎么没学到"；
// 而重放是幂等的（同 persona+slot+value 只留一条），代价只是一次多余的写入。
func (a *App) addRuleCandidates(personaID string, rules []settle.RuleCandidate) error {
	if a.personas == nil {
		return nil
	}
	p, ok := findPersona(a.personas.Snapshot().Personas, personaID)
	if !ok || p.IsBuiltin {
		log.Printf("[settle] 「%s」是内置人格，本次抽出的 %d 条行为倾向已跳过",
			a.personaName(personaID), len(rules))
		return nil
	}

	cs := make([]persona.Candidate, 0, len(rules))
	for _, r := range rules {
		cs = append(cs, persona.Candidate{
			PersonaID: personaID,
			Slot:      r.Slot,
			Value:     r.Value,
			// 依据存**原话**：用户判断"要不要让她一直这样"时，只有原话能说明她是从哪句听出来的
			Evidence: r.Quote,
		})
	}
	if err := a.personas.AddCandidates(cs); err != nil {
		return fmt.Errorf("写行为倾向候选失败: %w", err)
	}
	return nil
}

// settleBadChunk 判断这一片是否已经被判过"输出不可用"。
func (a *App) settleBadChunk(chunkID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settleBad[chunkID]
}

// markSettleBad 记住这一片的输出不可用，本进程内不再重试。
func (a *App) markSettleBad(chunkID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settleBad == nil {
		a.settleBad = make(map[string]bool)
	}
	a.settleBad[chunkID] = true
}
