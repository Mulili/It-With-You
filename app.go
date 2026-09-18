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

	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/persona"
	errorcode "agent-for-you-love/internal/pkg/errorCode"
	"agent-for-you-love/internal/ui"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// DirectiveTimeout 是抽取显式指令的超时时间。
// 它跑在独立 ctx 上（不挂 prevCancel）：用户发下一句不该把"记住我的要求"这件事掐掉。
const DirectiveTimeout = 30 * time.Second

type App struct {
	ctx      context.Context
	win      *ui.Window
	provider llm.Provider
	// personas 是人格存储。阶段3 用内存实现，⑥ 换成 PG（同一接口）。
	personas persona.Store

	mu sync.Mutex
	// records 是会话历史的唯一真源，阶段2.5 只放内存：隐藏窗口不销毁进程，
	// 所以"关闭窗口不丢历史"这条验收成立；进程重启即丢，持久化留到阶段3 / 阶段4。
	//
	// 两个出口都从它投影而来，不另存副本——两份数据要成对 append，
	// 漏一处就会永久不同步，而症状是"菜单里少一条"这种极难定位的问题：
	//   - 发给模型：projectMessages（阶段4 的上下文截断加在那里）
	//   - 给前端菜单：History（截到最近 ui.HistoryDisplayLimit 条）
	records []record
	// prevCancel 是上一轮流的取消函数。context.CancelFunc 可重复调用，所以不必清理，
	// 每次新请求直接覆盖即可——省掉了"谁负责清空"的并发问题。
	prevCancel context.CancelFunc
	msgSeq     int
	// deletedPersonas 记住已被删除的人格 ID：删人格时那一轮可能还在收尾，
	// 它的"半截回复"不能再写进历史（personaID 已失效，写进去就是孤儿数据）。
	// 集合很小且只在删人格时增长，不做清理。
	deletedPersonas map[string]bool
}

// record 是内部记录：除了 llm.Message 还带上展示需要的编号、时间与状态。
//
// 这些字段必须留在这里，不能加进 llm.Message——后者会被整段序列化进请求体，
// 加什么字段都会原样发给模型。
type record struct {
	id string
	// personaID 标识这条消息属于哪个人格。
	//
	// 历史按人格隔离：在用户眼里每个人格都是一个独立个体，把 A 的对话带进 B 的上下文
	// 就是污染（B 会知道它本不该知道的事）。所以读历史、拼上下文、写回复都要认这个字段。
	personaID string
	message   llm.Message
	at        int64
	status    string
}

// nextID 返回进程内自增的记录编号。调用方必须持有 a.mu。
func (a *App) nextID() string {
	a.msgSeq++
	return fmt.Sprintf("m%d", a.msgSeq)
}

func NewApp(provider llm.Provider, personas persona.Store) *App {
	return &App{provider: provider, personas: personas}
}

// startup 由 Wails 在应用启动时调用，此后 runtime 才可用。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.win = ui.NewWindow(ctx)

	ui.StartTray(ui.TrayOptions{
		Icon:           trayIcon,
		Tooltip:        "AI 桌面伴侣",
		OnToggleWindow: a.ToggleWindow,
		OnSay:          func() { a.Say("你好，我是你的桌面伴侣～") },
		OnQuit:         a.Quit,
	})

	log.Println("[app] 启动完成：窗口 + 系统托盘就绪")
}

// shutdown 由 Wails 在应用退出时调用。
func (a *App) shutdown(ctx context.Context) {
	ui.StopTray()
	// 存储若持有资源（PG 连接池），在这里释放；内存实现也实现了 Close，是空操作
	if c, ok := a.personas.(io.Closer); ok {
		if err := c.Close(); err != nil {
			log.Printf("[persona] 关闭存储失败: %v", err)
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
	// 这一轮的 ID 同时就是这条 user 记录的 ID，流式事件也用它，前端据此过滤片段
	id := a.nextID()
	a.records = append(a.records, record{
		id:        id,
		personaID: personaID,
		message:   llm.Message{Role: llm.RoleUser, Content: text},
		at:        time.Now().UnixMilli(),
		status:    ui.StatusOK, // 用户这句话在发出时就是完整的
	})
	// 拷贝一份给 goroutine：之后 records 还会被 append，共享底层数组会读到意料之外的内容。
	msgs := a.buildMessages(personaID, a.recordsOf(personaID))
	a.mu.Unlock()

	go a.stream(ctx, id, personaID, msgs)
	// 顺带看一眼这句话里有没有"长期要求"（粗筛命中才会真的调一次模型）。
	// 它有自己的 ctx 与超时，不会因为用户紧接着发下一句而被取消。
	go a.handleDirective(text)
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

// projectMessages 把内部记录投影成发给模型的消息。
//
// 现在只是把 message 摘出来，但它有意做成一函数而不是一段内联代码：
// 阶段3 要在头部拼人格 System Prompt、阶段4 要「只发最近 N 轮」，
// 那两件事都加在这里，而不是去动 records 本身。
//
// 纯函数、不碰锁：调用方持有 a.mu 时使用。
func projectMessages(records []record) []llm.Message {
	msgs := make([]llm.Message, 0, len(records))
	for _, r := range records {
		msgs = append(msgs, r.message)
	}
	return msgs
}

// History 返回**当前人格**最近的对话，供前端菜单展示（只读，拉模式）。
//
// 按人格过滤是刻意的：人格在用户眼里是独立个体，A 的对话不该出现在 B 的历史里。
// 返回的是新切片，与内部 records 不共享底层数组，前端随便改都影响不到真源。
func (a *App) History() []ui.HistoryItem {
	a.mu.Lock()
	defer a.mu.Unlock()

	mine := a.recordsOf(a.activePersonaID())
	start := len(mine) - ui.HistoryDisplayLimit
	if start < 0 {
		start = 0
	}
	items := make([]ui.HistoryItem, 0, len(mine)-start)
	for _, r := range mine[start:] {
		items = append(items, ui.HistoryItem{
			ID:     r.id,
			Role:   r.message.Role,
			Text:   r.message.Content,
			At:     r.at,
			Status: r.status,
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

// recordsOf 取出某个人格的历史。调用方需持有 a.mu。
func (a *App) recordsOf(personaID string) []record {
	out := make([]record, 0, len(a.records))
	for _, r := range a.records {
		if r.personaID == personaID {
			out = append(out, r)
		}
	}
	return out
}

// buildMessages 组装发给模型的消息：人格 system（若有）+ **该人格的**对话历史。
//
// 三件事刻意如此：
//   - system **不写进 records**：用户不该在历史列表里看到系统提示词，切换人格也才会立即生效；
//   - 人格每轮现拼：所以改人格、改规则、用户提了新要求，下一轮就生效，不需要重启；
//   - 只喂当前人格的历史：别的人格聊过的内容不该出现在它的上下文里；
//   - 预算截断发生在 persona.BuildSystemPrompt 内（超预算先截 recent 层，主体与 core 永不截）。
func (a *App) buildMessages(personaID string, mine []record) []llm.Message {
	history := projectMessages(mine)
	if a.personas == nil {
		return history
	}

	snap := a.personas.Snapshot()
	active, ok := findPersona(snap.Personas, personaID)
	if !ok {
		return history
	}
	system, dropped := persona.BuildSystemPrompt(active, snap.Rules)
	if dropped > 0 {
		log.Printf("[persona] 「%s」的 recent 层有 %d 条规则超出注入预算，本轮未注入", active.Name, dropped)
	}
	if strings.TrimSpace(system) == "" {
		return history
	}

	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: system})
	return append(msgs, history...)
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
	if a.personas == nil {
		return persona.Snapshot{}, fmt.Errorf("人格存储未就绪")
	}
	return a.personas.Snapshot(), nil
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

	a.mu.Lock()
	if a.deletedPersonas == nil {
		a.deletedPersonas = make(map[string]bool)
	}
	a.deletedPersonas[id] = true
	// 显式新建切片而不是原地过滤：原地复用底层数组会给"同时持有旧切片"的地方埋雷
	kept := make([]record, 0, len(a.records))
	for _, r := range a.records {
		if r.personaID != id {
			kept = append(kept, r)
		}
	}
	a.records = kept
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
// personaID 是这一轮所属的人格：回复要写回它的历史——即使用户中途切了人格，
// 这条回复仍然属于"当初被问的那个人"。
func (a *App) stream(ctx context.Context, id, personaID string, msgs []llm.Message) {
	ch, err := a.provider.ChatStream(ctx, msgs)
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
				a.appendAssistant(personaID, full.String(), ui.StatusCanceled)
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

	a.appendAssistant(personaID, full.String(), ui.StatusOK)
	runtime.EventsEmit(a.ctx, ui.EventChatDone, ui.ChatDonePayload{ID: id})
}

// appendAssistant 把这一轮的回复写进**它所属人格**的历史。
// status 区分"正常说完"与"被用户打断"。
func (a *App) appendAssistant(personaID, content, status string) {
	if content == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// 人格刚被删掉：这条回复属于一个已不存在的人，写进去就是孤儿数据
	//（删人格时会掐掉那一轮，但它的收尾可能晚于删除动作）
	if a.deletedPersonas[personaID] {
		return
	}
	a.records = append(a.records, record{
		id:        a.nextID(),
		personaID: personaID,
		message:   llm.Message{Role: llm.RoleAssistant, Content: content},
		at:        time.Now().UnixMilli(),
		status:    status,
	})
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
