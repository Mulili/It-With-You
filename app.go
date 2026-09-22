package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	"agent-for-you-love/internal/persona"
	errorcode "agent-for-you-love/internal/pkg/errorCode"
	"agent-for-you-love/internal/settle"
	"agent-for-you-love/internal/ui"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// DirectiveTimeout 是抽取显式指令的超时时间。
// 它跑在独立 ctx 上（不挂 prevCancel）：用户发下一句不该把"记住我的要求"这件事掐掉。
const DirectiveTimeout = 30 * time.Second

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

type App struct {
	ctx      context.Context
	win      *ui.Window
	provider llm.Provider
	// personas 是人格存储。阶段3 用内存实现，⑥ 换成 PG（同一接口）。
	personas persona.Store
	// history 是对话历史（阶段4）：PG 是**唯一真源**，不再留内存副本。
	//
	// 敢不留副本，是因为写入频率很低（每轮两条）、读取也低（每轮一次拼上下文 +
	// 打开菜单时一次）；而"两份数据要成对维护"的代价很高——漏一处就永久不同步，
	// 症状又是"菜单里少一条"这种极难定位的问题。
	history history.Store

	// memories 是长期记忆（阶段4）：公共事实 + 本人格私有经历 + 片索引。
	// 它与 history 共用同一个连接池，但**不是同一份数据**：history 存原文、按会话取；
	// memories 存提炼出来的东西、按语义取。
	memories memory.Store
	// embedder 为 nil 表示嵌入服务不可用。此时**记忆功能整体停用**：
	// 不结算、不抽取、不写索引，对话照常——抽了却嵌不出向量，等于白花一次调用。
	// 这与"数据库连不上"的处理姿态不同：嵌入只服务记忆，而对话根本不经过它。
	embedder llm.Embedder

	mu sync.Mutex
	// judgeMu 串行化「话题边界」判断。
	//
	// 每轮都会起一个后台判断，用户连发几句时它们会并发跑、却在改同一段会话的状态。
	// 用 TryLock 而不是 Lock：抢不到就跳过这次——判断依据是"最新的几句"，
	// 跳过旧的那次没有损失，而排队只会让判断越来越滞后。
	judgeMu sync.Mutex
	// prevCancel 是上一轮流的取消函数。context.CancelFunc 可重复调用，所以不必清理，
	// 每次新请求直接覆盖即可——省掉了"谁负责清空"的并发问题。
	prevCancel context.CancelFunc
	// msgSeq 生成**轮次 ID**：前端用它过滤掉上一轮请求的残留片段。
	//
	// 它与消息 ID 不是一回事：消息 ID 是 uuid、由 history 存储层生成、要落库；
	// 轮次 ID 只在一轮请求内有效，重启后从头数也无所谓。
	msgSeq int
	// deletedPersonas 记住已被删除的人格 ID：删人格时那一轮可能还在收尾，
	// 它的"半截回复"不能再写进历史（personaID 已失效，写进去就是孤儿数据）。
	// 集合很小且只在删人格时增长，不做清理。
	deletedPersonas map[string]bool

	// settleMu 串行化收尾结算。与 judgeMu 同一套路：抢不到就跳过这次——
	// 结算不急着这一刻完成，下一轮还有机会。
	settleMu sync.Mutex
	// settleBad 记住"这一片结算过了、但输出不可用"的片 ID。
	//
	// 为什么需要它：扫待结算的条件是"已收尾且没有摘要"，输出不可用的片会一直留在里面，
	// 于是每一轮都白烧一次调用。而这类失败是**确定的**（同样的输入还是同一份垃圾输出），
	// 所以进程内不再重试；重启后会再给一次机会。
	//
	// 注意只记"输出不可用"，网络与嵌入的失败**不记**——那些是瞬时的，值得下一轮再试。
	settleBad map[string]bool

	// thinkingDisabled 是用户的思考模式开关（应用级设置，缓存在内存里）。
	//
	// 缓存的理由：Ask 每轮都要用它决定带不带 thinking 字段，不该每次都查一遍库；
	// 而写入口只有 SetThinkingDisabled 一处，所以缓存不会走样。
	thinkingDisabled bool
}

// nextID 返回进程内自增的轮次编号。
func (a *App) nextID() string {
	a.msgSeq++
	return fmt.Sprintf("m%d", a.msgSeq)
}

// NewApp 组装应用。
//
// embedder 传 nil 表示嵌入服务不可用，此时记忆功能整体停用（不结算、不抽取），
// 对话与人格不受影响——见 App.embedder 的注释。
func NewApp(provider llm.Provider, personas persona.Store, hist history.Store,
	mems memory.Store, embedder llm.Embedder) *App {
	a := &App{provider: provider, personas: personas, history: hist,
		memories: mems, embedder: embedder}
	// 思考开关读一次就缓存在内存：Ask 每轮都要用它，不该每次都查库。
	// 读失败按默认（false = 跟随官方默认的"思考开启"）继续——一个设置读不到，
	// 不该让整个应用起不来。
	if personas != nil {
		disabled, err := personas.ThinkingDisabled()
		if err != nil {
			log.Printf("[app] 读取思考开关失败，按默认（开启思考）处理: %v", err)
		} else {
			a.thinkingDisabled = disabled
		}
	}
	return a
}

// startup 由 Wails 在应用启动时调用，此后 runtime 才可用。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.win = ui.NewWindow(ctx)

	ui.StartTray(ui.TrayOptions{
		Icon:           trayIcon,
		Tooltip:        "常驻助手",
		OnToggleWindow: a.ToggleWindow,
		OnSay:          func() { a.Say("我是助手哦") },
		OnQuit:         a.Quit,
	})

	log.Println("[app] 启动完成：窗口 + 系统托盘就绪")

	// 启动时把上次没结算完的补上：可能是上次退出得急，也可能是发消息那几轮没赶上。
	// 放后台跑，不拖慢启动——结算一次要几秒，而它跟"能不能开始聊"没有关系。
	go a.settlePending()
}

// shutdown 由 Wails 在应用退出时调用。
func (a *App) shutdown(ctx context.Context) {
	ui.StopTray()
	// 存储若持有资源（PG 连接池），在这里释放。两个 store 都实现了 io.Closer，
	// 但真正关池的只有 persona：池是全应用共用的那一个，history 的 Close 是空操作——
	// 若它也去关，先被调到的那个就会把池关掉、另一个立刻失效。
	if c, ok := a.personas.(io.Closer); ok {
		if err := c.Close(); err != nil {
			log.Printf("[persona] 关闭存储失败: %v", err)
		}
	}
	if c, ok := a.history.(io.Closer); ok {
		if err := c.Close(); err != nil {
			log.Printf("[history] 关闭存储失败: %v", err)
		}
	}
	log.Println("[app] 已退出")
}

// Say 让桌宠说一句话。
//
// 文本通过 Wails Events 推给前端，由 Bubble.vue 渲染成气泡。
// 之所以不用「返回值」而用「事件」：阶段2 接入 LLM 后，说话会由后端流式主动发起，
// 方向天然是 Go → 前端。这里先用同一条通路，后面接 LLM 时不必改架构。
func (a *App) Say(text string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, ui.EventSay, ui.SayPayload{
		Text: text,
		At:   time.Now().UnixMilli(),
	})
}

// Ask 把用户这句话交给模型，然后立刻返回本轮的消息 ID；
// 回复内容通过 chat:chunk / chat:done / chat:error 三个事件流式推给前端。
//
// 为什么必须立刻返回：绑定给前端的方法，前端 await 它时是在等返回值。
// 在这里等模型说完，界面就一直是卡住的——这正是流式输出要走事件的原因。
func (a *App) Ask(text string) (string, error) {
	if a.ctx == nil {
		return "", errorcode.New(errorcode.BadRequest, "应用尚未准备就绪")
	}
	if a.history == nil {
		return "", errorcode.New(errorcode.BadRequest, "历史存储未就绪")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}

	ctx, cancel := context.WithCancel(a.ctx)

	a.mu.Lock()
	if a.prevCancel != nil {
		a.prevCancel() // 掐掉上一轮，保证同时只有一个流
	}
	a.prevCancel = cancel
	// 这一轮归属"当前人格"：历史按人格隔离，回复也要写回它所属的那一份
	personaID := a.activePersonaID()
	// 轮次 ID：前端据此过滤片段，与落库的消息 ID 无关
	id := a.nextID()
	// 思考开关是 a 的字段，在锁内取出来交给 goroutine，别让后台再去碰它
	noThinking := a.thinkingDisabled
	a.mu.Unlock()

	// 回头把上一段已聊完、却还没整理的补结算（见 settlePending）。
	//
	// 触发点选在**这里**（而不是写完这一轮之后）是有意的：待结算的片属于"上一段"，
	// 它的最后一条回复是好几秒前写下的，此刻结算不会读到半截内容。
	// 放在后面则可能和正在流式的这一轮撞上——读到刚写一半的片。
	go a.settlePending()

	// 先确定"这一轮写进哪段会话的哪一片"：
	// 话题没聊完就接着上一段；而片写到阈值时会在**这次发言之前**切开、另起一片——
	// 所以片边界永远落在用户发言处，一个问答对不会被从中间切开。
	sess, err := a.history.EnsureSession(personaID)
	if err != nil {
		return "", fmt.Errorf("准备会话失败: %w", err)
	}
	chunk, err := a.history.EnsureChunk(sess.ID, history.ChunkMaxRunes)
	if err != nil {
		return "", fmt.Errorf("准备分片失败: %w", err)
	}

	// 用户这句话**先写库、再读**：这样读出来的消息列表天然包含它，
	// 不必在拼上下文时手工追加——少一处容易漏的地方。
	if _, err := a.history.AppendMessage(history.Message{
		SessionID: sess.ID,
		ChunkID:   chunk.ID,
		PersonaID: personaID,
		Role:      llm.RoleUser,
		Content:   text,
		Status:    history.StatusOK, // 用户这句话在发出时就是完整的
	}); err != nil {
		// 写不进去就不往下走：历史是硬性要求，缺一条会让后续上下文错位
		return "", fmt.Errorf("保存消息失败: %w", err)
	}

	msgs, err := a.chunkMessages(chunk.ID)
	if err != nil {
		return "", err
	}
	full := a.buildMessages(personaID, msgs)

	go a.stream(ctx, id, sess.ID, chunk.ID, personaID, full, noThinking)
	// 顺带看一眼这句话里有没有"长期要求"（粗筛命中才会真的调一次模型）。
	// 它有自己的 ctx 与超时，不会因为用户紧接着发下一句而被取消。
	go a.handleDirective(text)
	// 再顺带判断这一句是否结束了当前话题。同样是后台任务：判定之后，
	// 下一轮 EnsureSession 就不会再复用这段会话，新会话自然开始（边界滞后一轮，无妨）。
	go a.judgeTopic(sess.ID, tailMessages(msgs, history.TopicJudgeContextLimit))
	return id, nil
}

// tailMessages 取最后 n 条消息（不足则全给）。
//
// 返回的是子切片：底层数组来自 Ask 里刚读出来的新切片，之后不会再被改，
// 所以交给 goroutine 是安全的。
func tailMessages(msgs []llm.Message, n int) []llm.Message {
	if n <= 0 || len(msgs) <= n {
		return msgs
	}
	return msgs[len(msgs)-n:]
}

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
	out, err := a.provider.Chat(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user},
	}, llm.ChatOptions{JSON: true})
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

// ---------- 收尾结算（3b-2）----------

// settlePending 是「收尾懒结算」：把已经聊完、却还没整理过的片整理成
// 片摘要 + 会话标题 + 待存事实，分头写进 session_chunks / sessions / memories / chunk_index。
//
// 为什么不挂在"话题结束那一刻"：那一刻本来就没人知道——结束是**下一轮**才判出来的。
// 于是搭下一轮的车：Ask 开头顺手扫一遍，启动时也扫一遍（见 startup）。
// 这与"懒归档"是同一个姿态：不引入常驻定时器，桌面应用会休眠，绝对时间的定时器不可靠。
//
// 触发点是"片"而不是"会话"：片写满时就会被切走并收尾，那时它就已经可以结算了，
// 不必等到整段会话聊完（会话可能一整天不结束）。
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
	out, err := a.provider.Chat(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user},
	}, llm.ChatOptions{JSON: true})
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

	log.Printf("[settle] 已结算片 %s（要点 %d / 事实 %d / 未了 %d / 丢弃 %d）",
		c.ID, len(res.KeyPoints), len(res.Facts), len(res.Unresolved), res.Dropped)
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

// Cancel 中止正在进行的回复（前端「停止」按钮）。
func (a *App) Cancel() {
	a.mu.Lock()
	cancel := a.prevCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// History 返回**当前人格**最近的对话，供前端菜单展示（只读，拉模式）。
//
// 按人格过滤是刻意的：人格在用户眼里是独立个体，A 的对话不该出现在 B 的历史里。
// 这里取的是**跨会话**的最近若干条（按会话分组展示留到界面那一步）。
//
// 读失败返回空列表而不是错误：菜单是"看一眼"的东西，为它弹错误反而更吵；
// 真出错时日志里有完整原因。
func (a *App) History() []ui.HistoryItem {
	if a.history == nil {
		return nil
	}
	rows, err := a.history.RecentMessages(a.activePersonaID(), ui.HistoryDisplayLimit)
	if err != nil {
		log.Printf("[history] 读取历史失败: %v", err)
		return nil
	}
	items := make([]ui.HistoryItem, 0, len(rows))
	for _, m := range rows {
		items = append(items, ui.HistoryItem{
			ID:     m.ID,
			Role:   m.Role,
			Text:   m.Content,
			At:     m.CreatedAt,
			Status: m.Status,
		})
	}
	return items
}

// activePersonaID 返回当前生效人格的 ID；没有人格时返回空串。
func (a *App) activePersonaID() string {
	if a.personas == nil {
		return ""
	}
	return a.personas.Snapshot().ActiveID
}

// buildMessages 组装发给模型的消息：人格 system（若有）+ **当前片的**消息。
//
// 四点刻意如此：
//   - system **不写进历史**：用户不该在历史列表里看到系统提示词，切换人格也才会立即生效；
//   - 人格每轮现拼：所以改人格、改规则、用户提了新要求，下一轮就生效，不需要重启；
//   - 只喂**当前片**：分片带来的收益就在这里——上下文边界天然给出，
//     不再需要"只发最近 N 轮"那种截断。更早的内容要靠记忆召回，而不是一路全带上；
//   - 预算截断发生在 persona.BuildSystemPrompt 内（超预算先截 recent 层，主体与 core 永不截）。
func (a *App) buildMessages(personaID string, msgs []llm.Message) []llm.Message {
	if a.personas == nil {
		return msgs
	}

	snap := a.personas.Snapshot()
	active, ok := findPersona(snap.Personas, personaID)
	if !ok {
		return msgs
	}
	system, dropped := persona.BuildSystemPrompt(active, snap.Rules)
	if dropped > 0 {
		log.Printf("[persona] 「%s」的 recent 层有 %d 条规则超出注入预算，本轮未注入", active.Name, dropped)
	}
	if strings.TrimSpace(system) == "" {
		return msgs
	}

	out := make([]llm.Message, 0, len(msgs)+1)
	out = append(out, llm.Message{Role: llm.RoleSystem, Content: system})
	return append(out, msgs...)
}

// findPersona 按 ID 找人。
func findPersona(personas []persona.Persona, id string) (persona.Persona, bool) {
	for _, p := range personas {
		if p.ID == id {
			return p, true
		}
	}
	return persona.Persona{}, false
}

// handleDirective 处理「用户提出了长期要求」这条链路：
// 本地粗筛 → LLM 抽取（JSON）→ 校验 → 写入人格 → 发回执事件。
//
// 它是后台任务：主回复不等待它，所以这一轮用的是旧人格，用户的新要求从**下一轮**开始生效。
// 每一步失败都只记日志：一次抽取不成不该影响对话本身。
func (a *App) handleDirective(userText string) {
	if a.personas == nil || a.ctx == nil || !persona.LooksLikeDirective(userText) {
		return
	}

	ctx, cancel := context.WithTimeout(a.ctx, DirectiveTimeout)
	defer cancel()

	system, user := persona.DirectivePrompt(userText)
	out, err := a.provider.Chat(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user},
	}, llm.ChatOptions{JSON: true})
	if err != nil {
		log.Printf("[persona] 指令抽取调用失败: %v", err)
		return
	}

	d, err := persona.ParseDirective(out)
	if err != nil {
		log.Printf("[persona] 指令抽取结果不可用: %v（模型原始输出：%s）", err, out)
		return
	}
	if !d.IsDirective {
		return // 不是长期要求，安静收场
	}

	personaID, copiedFrom, err := a.writablePersona()
	if err != nil {
		log.Printf("[persona] 找不到可写人格: %v", err)
		return
	}
	if _, err := a.personas.SaveRule(persona.PersonaRule{
		PersonaID: personaID,
		Slot:      d.Slot,
		Value:     d.Value,
		Source:    persona.SourceExplicit,
		// 用户明说 → 归入 recent 层：它是"当前的偏好"，参与预算与将来的淘汰；
		// 想让它永久生效，在设置里把它提升到 core（那是人工的权限）
		Tier:     persona.TierRecent,
		Evidence: userText,
	}); err != nil {
		log.Printf("[persona] 保存人格规则失败: %v", err)
		return
	}

	summary := fmt.Sprintf("已记住：%s → %s", persona.SlotLabel(d.Slot), d.Value)
	if copiedFrom != "" {
		summary += "（当前是内置人格，已为你复制一份可编辑的）"
	}
	log.Printf("[persona] %s", summary)
	runtime.EventsEmit(a.ctx, ui.EventPersonaChanged, ui.PersonaChangedPayload{
		PersonaID: personaID,
		Action:    persona.ActionUpdate,
		Slot:      d.Slot,
		Value:     d.Value,
		Summary:   summary,
	})
}

// writablePersona 返回当前可写的人格 ID。
//
// 内置人格是只读的，而"人格随对话成长"要求它可写——所以当用户第一次对内置人格提出
// 个人化要求时，顺手复制一份"我的"并切过去。这样用户不必先理解"内置/自定义"的区别，
// 代价是会多出一个人格（回执里会说明，用户可在设置里删）。
func (a *App) writablePersona() (id string, copiedFrom string, err error) {
	snap := a.personas.Snapshot()
	active, ok := findPersona(snap.Personas, snap.ActiveID)
	if !ok {
		return "", "", fmt.Errorf("当前没有生效的人格")
	}
	if !active.IsBuiltin {
		return active.ID, "", nil
	}

	newID, err := a.personas.CreatePersona(active.Name+"（我的）", active.ID)
	if err != nil {
		return "", "", fmt.Errorf("复制内置人格失败: %w", err)
	}
	if err := a.personas.SetActivePersona(newID); err != nil {
		return "", "", fmt.Errorf("切换到新人格失败: %w", err)
	}
	return newID, active.ID, nil
}

// GetPersonaSnapshot 一次返回设置页所需的全部人格数据。
//
// 读走"一次拿全"、写走细粒度方法：小浮层里多次往返会有肉眼可见的卡顿。
func (a *App) GetPersonaSnapshot() (persona.Snapshot, error) {
	if err := a.requireStore(); err != nil {
		return persona.Snapshot{}, err
	}
	return a.personas.Snapshot(), nil
}

// GetPersonaRules 返回**指定**人格的规则。
//
// 快照里只有当前人格的规则，而编辑器要能改任意人格（含非当前人格）——
// 否则"想改 Miku 的规则"就得先切到 Miku，而切换会掐掉正在生成的那一轮、清空气泡，
// 副作用太大，不该由"编辑"这个动作触发。
func (a *App) GetPersonaRules(personaID string) ([]persona.PersonaRule, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	return a.personas.RulesOf(personaID)
}

// GetPersonaMeta 返回编辑器的静态元信息：槽位清单 + 各字段长度上限。
//
// 由后端提供而不是前端硬编码：槽位表与上限值是"唯一真相"，前端下拉、字数提示、
// 导入校验都该用同一份；否则加了槽位只改了 Go 那边，UI 里就选不到它。
func (a *App) GetPersonaMeta() persona.Meta {
	return persona.MetaInfo()
}

// GetPersonaChanges 返回指定人格的最近变更记录（时间倒序）。
//
// 与 GetPersonaRules 同理：快照只带当前人格的变更，而编辑器要能看任意人格的。
func (a *App) GetPersonaChanges(personaID string) ([]persona.PersonaChange, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	return a.personas.ChangesOf(personaID)
}

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

// SetActivePersona 切换当前生效的人格。
//
// 切换是立即生效的（人格每轮现拼）。历史按人格隔离，所以切换后会看到**那个人格自己的**对话，
// 而不是上一个人格聊过的内容——人格在用户眼里是独立个体。
func (a *App) SetActivePersona(id string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	if err := a.personas.SetActivePersona(id); err != nil {
		return err
	}

	// 先掐掉正在生成的那一轮：半截回复会以 canceled 状态留在**旧人格**的历史里，
	// 而不是继续往新人格的界面上写——那样就成了"两边串台"。
	a.Cancel()

	// action 用 "switch"：它不是规则变更（PersonaChange 的那套动作），只是"当前人格换人了"
	a.notifyPersonaChanged(id, "switch", fmt.Sprintf("已切换到「%s」", a.personaName(id)))
	return nil
}

// ---------- 人格设置的写入方法（⑦）----------
//
// 都是细粒度方法：设置浮层要能知道"哪一条变了"，才能只刷新那一处、并弹对应的回执。
// 每个成功的写入都会发 persona:changed，前端据此刷新菜单并用 summary 弹一条回执。

// SaveSeedText 改写主体文本。之所以要带 personaID：编辑器要能改列表里任意一个人格，
// 而不只是当前生效的那个。
func (a *App) SaveSeedText(personaID, text string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	if err := a.personas.SaveSeedText(personaID, text); err != nil {
		return err
	}
	a.notifyPersonaChanged(personaID, persona.ActionUpdate, "已保存主体人格")
	return nil
}

// CreatePersona 新建人格；copyFromID 非空表示从它复制（内置人格的"复制为我的"走这条），返回新人格 ID。
func (a *App) CreatePersona(name, copyFromID string) (string, error) {
	if err := a.requireStore(); err != nil {
		return "", err
	}
	id, err := a.personas.CreatePersona(name, copyFromID)
	if err != nil {
		return "", err
	}
	a.notifyPersonaChanged(id, persona.ActionCreate, fmt.Sprintf("已创建「%s」", name))
	return id, nil
}

// RenamePersona 重命名人格。
func (a *App) RenamePersona(id, name string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	if err := a.personas.RenamePersona(id, name); err != nil {
		return err
	}
	a.notifyPersonaChanged(id, persona.ActionUpdate, fmt.Sprintf("已重命名为「%s」", name))
	return nil
}

// DeletePersona 删除人格。
//
// 除了存储里的数据，还要清掉**它在内存里的对话历史**：那些记录的 personaID 已经失效，
// 留着既不显示又占内存，是典型的孤儿数据。
func (a *App) DeletePersona(id string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	name := a.personaName(id)

	// 只有删当前人格时才需要掐流：任何时刻在流的那一轮都属于当前人格
	//（切人格时 SetActivePersona 也会先取消，所以这条不变式成立）
	if a.activePersonaID() == id {
		a.Cancel()
	}
	if err := a.personas.DeletePersona(id); err != nil {
		return err
	}
	// 历史在 PG 侧有外键 CASCADE 兜着，但这里仍显式清一次：
	// 不让"数据被清掉"这件事依赖一个看不见的外键行为，内存实现也走同一条路径
	if a.history != nil {
		if err := a.history.DeletePersona(id); err != nil {
			log.Printf("[history] 清理已删除人格的历史失败: %v", err)
		}
	}

	a.mu.Lock()
	if a.deletedPersonas == nil {
		a.deletedPersonas = make(map[string]bool)
	}
	a.deletedPersonas[id] = true
	a.mu.Unlock()

	// 删掉的若是当前人格，存储层已把当前人格回退到第一个内置人格，回执里说清切到了谁
	summary := fmt.Sprintf("已删除「%s」", name)
	if active := a.activePersonaID(); active != "" && active != id {
		summary += fmt.Sprintf("，已切换到「%s」", a.personaName(active))
	}
	a.notifyPersonaChanged(id, persona.ActionDelete, summary)
	return nil
}

// SaveRule 新增或更新一条规则（rule.ID 为空即新增），返回规则 ID。
func (a *App) SaveRule(rule persona.PersonaRule) (string, error) {
	if err := a.requireStore(); err != nil {
		return "", err
	}
	id, err := a.personas.SaveRule(rule)
	if err != nil {
		return "", err
	}
	a.notifyPersonaChanged(rule.PersonaID, persona.ActionUpdate,
		fmt.Sprintf("已保存：%s → %s", persona.SlotLabel(rule.Slot), rule.Value))
	return id, nil
}

// DeleteRule 删除规则。
func (a *App) DeleteRule(id string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	label, personaID := a.ruleLabel(id)
	if err := a.personas.DeleteRule(id); err != nil {
		return err
	}
	a.notifyPersonaChanged(personaID, persona.ActionDelete, "已删除 "+label)
	return nil
}

// SetRuleEnabled 停用 / 启用规则（停用而不删，方便反悔）。
func (a *App) SetRuleEnabled(id string, enabled bool) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	label, personaID := a.ruleLabel(id)
	if err := a.personas.SetRuleEnabled(id, enabled); err != nil {
		return err
	}
	action, prefix := persona.ActionDisable, "已停用 "
	if enabled {
		action, prefix = persona.ActionEnable, "已启用 "
	}
	a.notifyPersonaChanged(personaID, action, prefix+label)
	return nil
}

// ExportPersonaToFile 弹保存对话框，把人格导出成可分享的 JSON，返回写入路径（用户取消则为空串）。
//
// 内置人格也允许导出——那是"产出自己的人格"最自然的通道：在应用里调好，导出来就是内置格式。
func (a *App) ExportPersonaToFile(id string) (string, error) {
	if a.ctx == nil {
		return "", fmt.Errorf("应用尚未准备就绪")
	}
	if err := a.requireStore(); err != nil {
		return "", err
	}

	file, err := a.personas.ExportFile(id)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化人格失败: %w", err)
	}

	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "导出人格",
		DefaultFilename: personaFileName(file.Persona.Name),
		Filters:         []runtime.FileFilter{{DisplayName: "人格文件 (*.json)", Pattern: "*.json"}},
	})
	if err != nil {
		return "", fmt.Errorf("打开保存对话框失败: %w", err)
	}
	if path == "" {
		return "", nil // 用户取消：不是错误，前端据此什么都不做
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	log.Printf("[persona] 已导出「%s」到 %s", file.Persona.Name, path)
	return path, nil
}

// ImportPersonaFromFile 弹打开对话框，导入一份人格文件；**同名不覆盖，改存副本**。
func (a *App) ImportPersonaFromFile() (persona.Persona, error) {
	if a.ctx == nil {
		return persona.Persona{}, fmt.Errorf("应用尚未准备就绪")
	}
	if err := a.requireStore(); err != nil {
		return persona.Persona{}, err
	}

	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "导入人格",
		Filters: []runtime.FileFilter{{DisplayName: "人格文件 (*.json)", Pattern: "*.json"}},
	})
	if err != nil {
		return persona.Persona{}, fmt.Errorf("打开文件对话框失败: %w", err)
	}
	if path == "" {
		return persona.Persona{}, nil // 用户取消
	}

	file, err := readPersonaFile(path)
	if err != nil {
		return persona.Persona{}, err
	}
	imported, err := a.personas.ImportFile(file)
	if err != nil {
		return persona.Persona{}, err
	}
	a.notifyPersonaChanged(imported.ID, persona.ActionCreate, fmt.Sprintf("已导入「%s」", imported.Name))
	return imported, nil
}

// maxImportFileBytes 限制导入文件的大小：文件是用户随手选的，不能无上限地读进内存。
const maxImportFileBytes = 256 << 10

// readPersonaFile 读文件并解析成人格文件（含格式与内容校验）。
func readPersonaFile(path string) (persona.PersonaFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return persona.PersonaFile{}, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxImportFileBytes))
	if err != nil {
		return persona.PersonaFile{}, fmt.Errorf("读取文件失败: %w", err)
	}
	return persona.ParsePersonaFile(data)
}

// personaFileName 由人格名生成默认文件名：人格名里可能有空格、斜杠、中文标点，
// 直接当文件名会在部分路径上失败或产生意外目录。
func personaFileName(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		return r
	}, strings.TrimSpace(name))
	if safe == "" {
		safe = "persona"
	}
	return safe + ".json"
}

// requireStore 统一处理"存储未就绪"。
func (a *App) requireStore() error {
	if a.personas == nil {
		return fmt.Errorf("人格存储未就绪")
	}
	return nil
}

// notifyPersonaChanged 通知前端"人格数据变了"：菜单与设置浮层据此刷新，并用 summary 弹一条回执。
func (a *App) notifyPersonaChanged(personaID, action, summary string) {
	log.Printf("[persona] %s", summary)
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, ui.EventPersonaChanged, ui.PersonaChangedPayload{
		PersonaID: personaID,
		Action:    action,
		Summary:   summary,
	})
}

// personaName 返回人格显示名；查不到就退回 ID（比如它刚被删掉）。
func (a *App) personaName(id string) string {
	if a.personas == nil {
		return id
	}
	if p, ok := findPersona(a.personas.Snapshot().Personas, id); ok {
		return p.Name
	}
	return id
}

// ruleLabel 拼出规则的显示文案（"称呼用户 → Mulili"），并返回它所属的人格。
//
// 快照里只有当前人格的规则，所以别的规则只能给个泛称——为一句回执文案多查一次库不值得。
func (a *App) ruleLabel(ruleID string) (label, personaID string) {
	if a.personas == nil {
		return "该规则", ""
	}
	for _, r := range a.personas.Snapshot().Rules {
		if r.ID == ruleID {
			return fmt.Sprintf("%s → %s", persona.SlotLabel(r.Slot), r.Value), r.PersonaID
		}
	}
	return "该规则", ""
}

// stream 消费 Provider 的通道，边收边推事件。
//
// sessionID / chunkID 决定这条回复写进哪段会话的哪一片；personaID 决定它属于哪个人格——
// 即使用户中途切了人格、或聊开了新话题，这条回复仍然属于"当初被问的那个人、那一次对话"。
//
// noThinking 是这一轮的思考开关（由 Ask 在锁内读出后传入，不在这里读 a 的字段）。
func (a *App) stream(ctx context.Context, id, sessionID, chunkID, personaID string, msgs []llm.Message, noThinking bool) {
	ch, err := a.provider.ChatStream(ctx, msgs, llm.ChatOptions{DisableThinking: noThinking})
	if err != nil {
		// 走到这里说明请求还没发出去（缺 Key、网络不通、4xx）。历史里保留用户这句，方便重试。
		runtime.EventsEmit(a.ctx, ui.EventChatError, ui.ChatErrorPayload{ID: id, Message: err.Error()})
		return
	}

	var full strings.Builder
	for chunk := range ch { // 必须读到底：提前 return 会让生产端 goroutine 永久阻塞
		if chunk.Err != nil {
			if errors.Is(chunk.Err, context.Canceled) {
				// 用户点了停止、发了新问题、或切了人格——这不是错误，安静收场。
				// 已收到的半截内容照样进历史，但标成 canceled：菜单要能把它和正常回复区分开。
				a.appendAssistant(sessionID, chunkID, personaID, full.String(), ui.StatusCanceled)
				return
			}
			runtime.EventsEmit(a.ctx, ui.EventChatError, ui.ChatErrorPayload{ID: id, Message: chunk.Err.Error()})
			return
		}
		if chunk.Content == "" {
			continue
		}
		full.WriteString(chunk.Content)
		runtime.EventsEmit(a.ctx, ui.EventChatChunk, ui.ChatChunkPayload{ID: id, Delta: chunk.Content})
	}

	a.appendAssistant(sessionID, chunkID, personaID, full.String(), ui.StatusOK)
	runtime.EventsEmit(a.ctx, ui.EventChatDone, ui.ChatDonePayload{ID: id})
}

// appendAssistant 把这一轮的回复写进**它所属的那一片**。
// status 区分"正常说完"与"被用户打断"。
//
// 写失败只记日志、不外抛：回复已经流式展示给用户了，收不回来；
// 而且这个函数跑在 goroutine 里，也没有调用方能接住错误。
func (a *App) appendAssistant(sessionID, chunkID, personaID, content, status string) {
	if content == "" {
		return
	}

	a.mu.Lock()
	deleted := a.deletedPersonas[personaID]
	a.mu.Unlock()
	// 人格刚被删掉：这条回复属于一个已不存在的人，写进去就是孤儿数据
	//（删人格时会掐掉那一轮，但它的收尾可能晚于删除动作）
	if deleted {
		return
	}

	// 写库是 IO，不持锁做：锁内只读那个小 map
	if _, err := a.history.AppendMessage(history.Message{
		SessionID: sessionID,
		ChunkID:   chunkID,
		PersonaID: personaID,
		Role:      llm.RoleAssistant,
		Content:   content,
		Status:    status,
	}); err != nil {
		log.Printf("[history] 写入回复失败（回复已展示给用户，无法回退）: %v", err)
	}
}

// ShowWindow 显示窗口。
func (a *App) ShowWindow() { a.win.Show() }

// HideWindow 隐藏窗口（收进托盘）。
func (a *App) HideWindow() { a.win.Hide() }

// ToggleWindow 在显示 / 隐藏之间切换。
func (a *App) ToggleWindow() { a.win.Toggle() }

// SetMenuOpen 由前端在打开 / 关闭历史菜单时调用，用于临时加高窗口。
func (a *App) SetMenuOpen(open bool) { a.win.SetMenuOpen(open) }

// Quit 退出应用。
func (a *App) Quit() { runtime.Quit(a.ctx) }
