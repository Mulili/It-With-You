package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/ui"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// 这是应用的核心：App 的组装、生命周期、以及一轮对话的完整通路（Ask → stream → 落库）。
//
// 其余部分按域分在同一个包的几个文件里（Go 的分文件，不是分包——它们共享 App 的字段，
// 拆成独立包只会把依赖倒过来传一遍）：
//   - app_persona.go  人格：读取、写入、导入导出、指令抽取
//   - app_settle.go   收尾结算、话题判断、过期候选清理
//   - app_recall.go   检索注入（嵌入 + 两条线 + 注入文本渲染）
//   - app_bind.go     给界面的数据绑定：历史、记忆、应用级设置
//
// 这个划分与测试文件是对齐的（app_persona_test.go / app_settle_test.go / app_recall_test.go）。

type App struct {
	ctx context.Context
	win *ui.Window
	// trayIcon 是托盘图标的字节内容（Windows 的 systray 要求 .ico，给 png 会加载失败）。
	//
	// 由 main 注入而不是在这里 go:embed：go:embed **不允许 `..` 路径**，
	// 而图标在 build/windows/ 下，相对本包就是 ../../build/windows/icon.ico。
	// 这是 embed 的硬性限制，不是偷懒——所以只好让"离资源最近的那个包"（main）读进来再传。
	trayIcon []byte
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
	// 不结算、不抽取、不写索引、**也不检索**，对话照常——抽了却嵌不出向量，等于白花一次调用。
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
	// warmMu 保证同一时刻最多一个嵌入预热在跑（见 warmEmbedder）。
	warmMu sync.Mutex

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
// trayIcon 是托盘图标字节，见 App.trayIcon 的注释（为什么是传进来而不是 embed）。
func NewApp(provider llm.Provider, personas persona.Store, hist history.Store,
	mems memory.Store, embedder llm.Embedder, trayIcon []byte) *App {
	a := &App{provider: provider, personas: personas, history: hist,
		memories: mems, embedder: embedder, trayIcon: trayIcon}
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

// Startup 由 Wails 在应用启动时调用，此后 runtime 才可用。
//
// 导出是为了跨包：main 要把它作为 OnStartup 回调传进去（见 main.go 的 wails.Run）。
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	a.win = ui.NewWindow(ctx)

	ui.StartTray(ui.TrayOptions{
		Icon:           a.trayIcon,
		Tooltip:        "常驻助手",
		OnToggleWindow: a.ToggleWindow,
		OnSay:          func() { a.Say("我是助手哦") },
		OnQuit:         a.Quit,
	})

	log.Println("[app] 启动完成：窗口 + 系统托盘就绪")

	// 启动时把上次没结算完的补上：可能是上次退出得急，也可能是发消息那几轮没赶上。
	// 放后台跑，不拖慢启动——结算一次要几秒，而它跟"能不能开始聊"没有关系。
	go a.settlePending()

	// 顺手清一次过期的「她学到的」：应用关着的那段时间没有结算，候选不会自己过期。
	// 也放后台：它是维护动作，与"能不能开始聊"同样无关。
	go a.pruneStaleCandidates()
}

// Shutdown 由 Wails 在应用退出时调用（导出理由同 Startup）。
func (a *App) Shutdown(ctx context.Context) {
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
		return "", errors.New("应用尚未准备就绪")
	}
	if a.history == nil {
		return "", errors.New("历史存储未就绪")
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
	chunk, err := a.history.EnsureChunk(sess.ID, history.ChunkMaxRunes, history.ChunkMaxMessages)
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
	// 想在拼上下文之前：它是这一轮里唯一的嵌入调用，跑完才有东西可注入（见 recall）
	rec := a.recall(personaID, sess.ID, msgs)
	full := a.buildMessages(personaID, msgs, rec.Text)

	go a.stream(ctx, id, sess.ID, chunk.ID, personaID, full, noThinking, rec)
	// 顺带看一眼这句话里有没有"长期要求"（粗筛命中才会真的调一次模型）。
	// 它有自己的 ctx 与超时，不会因为用户紧接着发下一句而被取消。
	go a.handleDirective(text)
	// 再顺带判断这一句是否结束了当前话题。同样是后台任务：判定之后，
	// 下一轮 EnsureSession 就不会再复用这段会话，新会话自然开始（边界滞后一轮，无妨）。
	go a.judgeTopic(sess.ID, tailMessages(msgs, history.TopicJudgeContextLimit))
	return id, nil
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

// chatJSON 发一次"只输出 JSON"的调用，返回模型的原始输出。
//
// 抽出来是因为同样的三行在 judgeTopic / settleChunk / handleDirective 里各写了一遍。
// 那三处都不关心流式、也不关心多轮，只要一个 JSON 字符串——而手抄的写法一旦分叉
// （比如某处漏了 JSON: true），症状是"那个功能偶尔解析失败"，很难定位到是这里。
//
// 超时与取消由调用方通过 ctx 控制：三处的时间预算不同（判话题 30 秒、结算 90 秒）。
func (a *App) chatJSON(ctx context.Context, system, user string) (string, error) {
	return a.provider.Chat(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user},
	}, llm.ChatOptions{JSON: true})
}

// buildMessages 组装发给模型的消息：人格 system（若有）+ **当前片的**消息 + 本轮的回忆。
//
// 五点刻意如此：
//   - system **不写进历史**：用户不该在历史列表里看到系统提示词，切换人格也才会立即生效；
//   - 人格每轮现拼：所以改人格、改规则、用户提了新要求，下一轮就生效，不需要重启；
//   - 只喂**当前片**：分片带来的收益就在这里——上下文边界天然给出，
//     不再需要"只发最近 N 轮"那种截断。更早的内容要靠记忆召回，而不是一路全带上；
//   - 预算截断发生在 persona.BuildSystemPrompt 内（超预算先截 recent 层，主体与 core 永不截）；
//   - recall **不写进历史**，且位置压在末尾（见下面注释）。
func (a *App) buildMessages(personaID string, msgs []llm.Message, recall string) []llm.Message {
	if a.personas == nil {
		return msgs
	}

	snap := a.personas.Snapshot()
	active, ok := findPersona(snap.Personas, personaID)
	if !ok {
		return msgs
	}
	// 她自己在对话里琢磨出来的倾向。拿不到就当这一轮没有情调——
	// 少一段"偶尔可以这样"不该让整轮对话失败，而它本来也不是每轮非有不可的东西。
	mood, err := a.personas.ListCandidates(personaID, moodInjectLimit)
	if err != nil {
		log.Printf("[persona] 读取「她学到的」失败，本轮不注入: %v", err)
		mood = nil
	}
	system, dropped := persona.BuildSystemPrompt(active, snap.Rules, mood)
	if dropped > 0 {
		log.Printf("[persona] 「%s」有 %d 条规则或倾向超出注入预算，本轮未注入", active.Name, dropped)
	}

	out := make([]llm.Message, 0, len(msgs)+2)
	if strings.TrimSpace(system) != "" {
		out = append(out, llm.Message{Role: llm.RoleSystem, Content: system})
	}
	if recall == "" {
		return append(out, msgs...)
	}
	if len(msgs) == 0 {
		return append(out, llm.Message{Role: llm.RoleSystem, Content: recall})
	}

	// 回忆插在**最后一条消息之前**（那是本轮用户说的话），而不是接在人格提示词后面：
	// 人格与历史那一段才是"每轮都一样"的前缀，把这条每轮都在变的回忆压到末尾，
	// 前缀缓存才不会被它打掉（4.7 记过这个坑）。
	out = append(out, msgs[:len(msgs)-1]...)
	out = append(out, llm.Message{Role: llm.RoleSystem, Content: recall})
	return append(out, msgs[len(msgs)-1])
}

// requireStore 统一处理"存储未就绪"。
func (a *App) requireStore() error {
	if a.personas == nil {
		return fmt.Errorf("人格存储未就绪")
	}
	return nil
}

// stream 消费 Provider 的通道，边收边推事件。
//
// sessionID / chunkID 决定这条回复写进哪段会话的哪一片；personaID 决定它属于哪个人格——
// 即使用户中途切了人格、或聊开了新话题，这条回复仍然属于"当初被问的那个人、那一次对话"。
//
// noThinking 是这一轮的思考开关（由 Ask 在锁内读出后传入，不在这里读 a 的字段）。
func (a *App) stream(ctx context.Context, id, sessionID, chunkID, personaID string, msgs []llm.Message, noThinking bool, rec recallResult) {
	ch, err := a.provider.ChatStream(ctx, msgs, llm.ChatOptions{DisableThinking: noThinking})
	if err != nil {
		// 走到这里说明请求还没发出去（缺 Key、网络不通、4xx）。历史里保留用户这句，方便重试。
		runtime.EventsEmit(a.ctx, ui.EventChatError, ui.ChatErrorPayload{ID: id, Message: err.Error()})
		return
	}

	var full strings.Builder    // 她的完整输出，含可能出现在末尾的 [[used:…]]
	var pending strings.Builder // 还没判定"能不能推给前端"的尾巴
	for chunk := range ch {     // 必须读到底：提前 return 会让生产端 goroutine 永久阻塞
		if chunk.Err != nil {
			if errors.Is(chunk.Err, context.Canceled) {
				// 用户点了停止、发了新问题、或切了人格——这不是错误，安静收场。
				// 已收到的半截内容照样进历史，但标成 canceled：菜单要能把它和正常回复区分开。
				body, _ := stripRecallUsage(full.String())
				a.appendAssistant(sessionID, chunkID, personaID, body, ui.StatusCanceled)
				return
			}
			runtime.EventsEmit(a.ctx, ui.EventChatError, ui.ChatErrorPayload{ID: id, Message: chunk.Err.Error()})
			return
		}
		if chunk.Content == "" {
			continue
		}
		full.WriteString(chunk.Content)
		pending.WriteString(chunk.Content)

		// 只推"确定不是标记开头"的那一段：标记在最末尾，而一个块可能正好把 "[[used:" 切开，
		// 这一块里认不出来。尾巴留着，等下一块拼齐再决定——否则它会在界面上闪一下。
		if s := pending.String(); true {
			if n := safePrefixLen(s); n > 0 {
				runtime.EventsEmit(a.ctx, ui.EventChatChunk, ui.ChatChunkPayload{ID: id, Delta: s[:n]})
				pending.Reset()
				pending.WriteString(s[n:])
			}
		}
	}

	// 收尾。pending 里可能还扣着几个字符（正好是标记开头的前缀，或者标记本身）——
	// 到这一步已经能精确判断，属于正文的补推出去，标记及其之后丢弃。
	if s := pending.String(); s != "" {
		if i := strings.Index(s, recallUsageOpen); i < 0 {
			runtime.EventsEmit(a.ctx, ui.EventChatChunk, ui.ChatChunkPayload{ID: id, Delta: s})
		} else if i > 0 {
			runtime.EventsEmit(a.ctx, ui.EventChatChunk, ui.ChatChunkPayload{ID: id, Delta: s[:i]})
		}
	}

	body, used := stripRecallUsage(full.String())
	a.appendAssistant(sessionID, chunkID, personaID, body, ui.StatusOK)
	runtime.EventsEmit(a.ctx, ui.EventChatDone, ui.ChatDonePayload{ID: id})

	// 计数归口：只给她**真的用到了**的那些 +1（见 markUsed）。
	// 放在这里而不是 recall 里，是因为只有读完整段回复才知道她到底用到了哪几条。
	a.markUsed(rec.Refs, used, sessionID)

	// 回复落库之后再扫一次待结算的片（第三个触发点，另外两个见 settlePending 的注释）。
	//
	// 为什么偏偏在这里：**话题一收尾，那段内容当场就离开上下文**——下一轮 EnsureSession 开的是
	// 新会话、新片，chunkMessages 只带新片的消息，旧话题从此只能靠记忆召回。
	// 而结算要一次 LLM 调用（几秒到十几秒）；若等下一轮开头才开始，用户下一句问起旧话题时
	// 摘要还没进库，检索必然是空的——正好错过最需要它的那一刻。
	// 放在这里，它就能在用户读回复、打字的那几秒里结算完。
	go a.settlePending()
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
