package app

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	"agent-for-you-love/internal/ui"
	"agent-for-you-love/internal/web"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// 这个文件装「给界面的数据绑定」：历史、记忆、应用级设置。
//
// 它们的共同点是"读为主、失败也不值得打断对话"——菜单是"看一眼"的地方，
// 弹一个错误框比少显示几条更打扰。所以这里的读方法失败一律返回空 + 记日志，
// 只有真正的写操作（删记忆、存开关）才把错误交给前端。

// ---------- 上下文占用（界面顶部那条状态条）----------

// ContextStat 返回**最近一轮**的上下文构成。
//
// 为什么要有这个"主动来问"的入口：事件（EventContextStat）只在拼上下文那一刻推一次，
// 而前端可能在它之后才打开/重开窗口——那时它没听见。两边取的是同一个字段，不会打架。
//
// 还没聊过任何一轮时返回全零：那时确实没有"上下文"可言，界面上那条自然是空的。
func (a *App) ContextStat() ui.ContextStat {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastCtxStat
}

// ---------- 应用级设置（⑨）----------

// AppSettings 是「设置」浮层需要的全局设置。
//
// 刻意与 persona.Snapshot 分开：那个快照装的是"某个人格"的数据，而思考开关不属于任何人格，
// 混进去会让"读某个人格的快照"顺带读到全局状态。
type AppSettings struct {
	// ThinkingDisabled 为 true 表示用户关掉了思考模式；默认 false = 跟随官方默认（思考开启）
	ThinkingDisabled bool `json:"thinkingDisabled"`
	// SearchDisabled 为 true 表示用户关掉了联网；默认 false = 允许她上网
	SearchDisabled bool `json:"searchDisabled"`
	// SearchURL 是**最终生效**的搜索服务地址（已经补过默认，见 searchURLOrDefault），
	// 设置页直接显示它、不必自己拼默认值。用户改了就存下来。
	SearchURL string `json:"searchUrl"`
	// SearchExe 是用户填的搜索服务程序路径；空串 = 按默认位置找（见 searchExePath）。
	SearchExe string `json:"searchExe"`
	// SearchService 是搜索服务此刻的状况（不探测，见 web.State）。
	// 设置页靠它显示"运行中 / 没找到程序 / 地址不是本机"，让失败**看得见**。
	SearchService web.State `json:"searchService"`
	// Embed 是嵌入后端的状态（是否就绪 + 模型路径 / 失败原因）。
	//
	// 它比搜索服务更要紧：搜索不可用只是"少一个能力"，嵌入不可用等于**记忆整体失效**，
	// 而记忆是这款应用的核心。所以它同时驱动**启动拦截**（Ready=false 时前端进阻断态）。
	//
	// ⚠️ Detail 里是**实际正在跑**的那个模型（由 main 在启动时定位），不是设置里的值：
	// 用户刚改了目录但还没重启时，两者不一样，而"现在到底用的哪个"才是他要知道的。
	Embed llm.EmbedStatus `json:"embed"`
	// EmbedModelDir 是用户选的嵌入模型目录（**原值**，空串 = 按默认位置找）。
	// 与 Embed.Detail 的区别见上：这个是"下次启动要用哪个"。
	EmbedModelDir string `json:"embedModelDir"`
}

// GetSettings 返回全局设置。
func (a *App) GetSettings() AppSettings {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := web.State{Message: "未启动"}
	if a.searchService != nil {
		st = a.searchService.Status()
	}
	return AppSettings{
		ThinkingDisabled: a.thinkingDisabled,
		SearchDisabled:   a.searchDisabled,
		SearchURL:        a.searchURLOrDefaultLocked(),
		SearchExe:        a.searchExe,
		SearchService:    st,
		Embed:            a.embedStatus,
		EmbedModelDir:    a.embedModelDir,
	}
}

// searchURLOrDefaultLocked 是 searchURLOrDefault 的**加锁版**，供已在锁内的地方复用。
func (a *App) searchURLOrDefaultLocked() string {
	if v := strings.TrimSpace(a.searchURL); v != "" {
		return v
	}
	return defaultSearchURL
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

// SetSearchDisabled 保存联网总开关。
//
// 关掉它的效果不只是"不搜"：她会**连联网工具都看不见**（搜索与读网页，见 tool.Availability）——
// 让她拿着一个被禁止的能力反复试，比干脆不给她更糟（用户看到的是"她老说查不到"）。
//
// 默认不关：这是用户的偏好，不替他做决定。
func (a *App) SetSearchDisabled(disabled bool) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	if err := a.personas.SetSearchDisabled(disabled); err != nil {
		return err
	}

	a.mu.Lock()
	a.searchDisabled = disabled
	a.mu.Unlock()

	state := "已允许她上网"
	if disabled {
		state = "已关闭联网（她连搜索工具都看不见）"
		// 关掉联网就把它拉起的那个进程也收走：没有理由让它继续在后台跑着占内存。
		// 用户自己起的那个不碰（见 web.Service.Stop）。
		if a.searchService != nil {
			a.searchService.Stop()
		}
	} else {
		// 重新打开就顺手拉起来，免得她下一次要搜时才现起（那要等 1~2 秒）
		a.ensureSearchServiceAfterSettingChange()
	}
	log.Printf("[app] %s", state)
	return nil
}

// SetSearchURL 保存搜索服务地址（空串 = 清回默认，见 defaultSearchURL）。
//
// 存的是**原值**：清空后 GetSettings 会显示回默认地址，但库里留下的是空——
// 于是"我从没改过"和"我改成了默认那个值"仍然分得清。
func (a *App) SetSearchURL(url string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	url = strings.TrimSpace(url)
	if err := a.personas.SetSearchURL(url); err != nil {
		return err
	}

	a.mu.Lock()
	a.searchURL = url
	a.mu.Unlock()

	if url == "" {
		log.Printf("[app] 搜索服务地址已清回默认（%s）", defaultSearchURL)
	} else {
		log.Printf("[app] 搜索服务地址已设为 %s", url)
	}
	// 换了地址就要让"新地址那头"有人：用户改了端口，正是想让服务起在那个端口上
	a.ensureSearchServiceAfterSettingChange()
	return nil
}

// SetSearchExe 保存搜索服务程序路径（空串 = 回到默认查找）。
//
// 存完**顺手（同步）尝试拉起一次**：用户填完路径就是要它工作，让他再去点一下"重试"
// 是没道理的。通常 ~2 秒；路径不对会立刻失败（exec 起不来），不会卡满超时。
func (a *App) SetSearchExe(path string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if err := a.personas.SetSearchExe(path); err != nil {
		return err
	}

	a.mu.Lock()
	a.searchExe = path
	a.mu.Unlock()

	if path == "" {
		log.Printf("[app] 搜索服务程序路径已清空（按默认位置查找）")
	} else {
		log.Printf("[app] 搜索服务程序路径已设为 %s", path)
	}
	a.ensureSearchServiceAfterSettingChange()
	return nil
}

// ensureSearchServiceAfterSettingChange 在**用户改完联网设置之后**同步尝试拉起一次。
//
// 为什么同步、以及为什么给 12 秒：调用方是设置页上的一个按钮，它 await 这个返回值。
// 同步才能让紧接着的那次 GetSettings 显示**真实**的新状态（而不是"改完还是旧结论"）。
// 12 秒是上限：正常 ~2 秒，路径写错会立刻失败，只有"进程起来了却始终不就绪"才会撞到它。
func (a *App) ensureSearchServiceAfterSettingChange() {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := a.ensureSearchService(ctx); err != nil {
		log.Printf("[app] 改了联网设置，但搜索服务仍不可用：%v", err)
	}
}

// ---------- 记忆索引重建 ----------

// ReindexTimeout 是一次重建索引的上限。
//
// 它由界面上的按钮 await，所以不能给无限长；但库大时确实要跑一会儿
// （现在几十条是秒级，几千条也就几分钟）。给 3 分钟：正常的库够用，
// 真超了说明出了问题，不该让按钮一直转下去。
const ReindexTimeout = 3 * time.Minute

// ReindexEmbeddings 把三张表的向量按**当前**嵌入后端重算一遍。
//
// 什么时候需要（用户要求过"不允许出现聊完了没记忆"，而这条正会让它**静默**变差）：
// 换了嵌入模型或实现之后。同一段文本在新旧模型下算出的向量不在同一个坐标系，
// 混在一张表里检索会让分数整体漂移——症状只是"记忆有时召回有时不召回"，
// 从界面上完全看不出根因。
//
// 为什么做成**手动入口**而不是启动时自动检测：自动检测得先存一份"模型指纹"再比对，
// 而维度、文件名这些都不足以区分（本项目换的这次，维度就是一样的 1024）。
// 多存一份状态就多一处会走样的地方；而界面上的状态行已经把当前模型显示出来了，
// 配合这个按钮，用户能看出该不该点。
//
// 只重算向量：记忆内容、创建时间、已提过几次，一律不动（见 memory.Store.ReembedAll）。
func (a *App) ReindexEmbeddings() (memory.ReembedResult, error) {
	if a.memories == nil {
		return memory.ReembedResult{}, fmt.Errorf("记忆存储未就绪")
	}
	if a.embedder == nil {
		return memory.ReembedResult{}, fmt.Errorf("嵌入后端未就绪，无法重建索引")
	}
	base := a.ctx
	if base == nil {
		base = context.Background() // 测试里 ctx 可能没注入
	}
	ctx, cancel := context.WithTimeout(base, ReindexTimeout)
	defer cancel()

	res, err := a.memories.ReembedAll(func(texts []string) ([][]float32, error) {
		return a.embedder.Embed(ctx, texts)
	})
	if err != nil {
		return res, fmt.Errorf("重建索引失败: %w", err)
	}
	log.Printf("[embed] 已重建记忆索引：%d 条事实 + %d 片往事 + %d 条归档规则",
		res.Memories, res.Chunks, res.Rules)
	return res, nil
}

// ---------- 自定义嵌入模型目录 ----------

// probeEmbedTimeout 是一次"试装模型"的上限。
//
// 它要加载几百 MB 的模型文件（实测约 2 秒，冷盘更慢）再跑一次前向，所以不能按普通接口
// 的几秒来给。60 秒是"慢盘也够、卡死也不至于让按钮一直转"的折中。
const probeEmbedTimeout = 60 * time.Second

// ChooseEmbedModelDir 弹目录选择框，**校验通过后保存**，返回最终保存的目录。
//
// 返回空串表示用户取消了（不报错、也不改任何设置）。
//
// 为什么把"选 + 校验 + 存"合成一个绑定：这三步是用户眼里的**一个动作**，而中间任何一步
// 失败都只能用同一条提示说清（"这个目录不能用，因为…"）。拆开只会让前端也去维护
// "选了但还没校验"这个中间态。
func (a *App) ChooseEmbedModelDir() (string, error) {
	if a.ctx == nil {
		return "", fmt.Errorf("应用尚未准备就绪")
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "选择嵌入模型目录（里面要有 .onnx 与 tokenizer.json）",
	})
	if err != nil {
		return "", fmt.Errorf("打开目录对话框失败: %w", err)
	}
	if dir == "" {
		return "", nil // 用户取消
	}
	if err := a.SetEmbedModelDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// SetEmbedModelDir 校验并保存嵌入模型目录（空串 = 回到默认位置）。
//
// **存之前一定先真装一次**（llm.ProbeONNXModel：定位 → 加载 → 跑一次嵌入）：
//   - 它把"目录里没有 .onnx / 少了 tokenizer.json / 有多个 .onnx 不知道用哪个"
//     当场说清楚，而不是等下次启动才在阻断页上蹦出来；
//   - 更要紧的是**维度**：记忆库的向量列固定是 vector(1024)。用户拿一个 768 维的
//     embedding 模型进来，如果被静默存下，症状要等到他下次写记忆才出现（写不进库），
//     而那时报错来自 pgvector、看着与嵌入毫无关系。
//
// ⚠️ 保存后**不热切换**：嵌入器由 main 在启动时建好（真加载几百 MB 并自检），
// 换它要处理正在跑的检索与结算，风险不划算。所以这里只管"验 + 存"，生效在下次启动——
// 设置页会把这句话明写出来，不让用户以为点了就换好了。
//
// ⚠️ 校验通过**不等于**已经可用：它只说明"那个目录里的模型跑得起来、维度也对"，
// 而这些向量与库里已有的不在同一个坐标系（见 ReindexEmbeddings），
// 所以重启之后还要点一次「重建记忆索引」。
func (a *App) SetEmbedModelDir(dir string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	dir = strings.TrimSpace(dir)

	if dir != "" {
		base := a.ctx
		if base == nil {
			base = context.Background() // 测试里 ctx 可能没注入
		}
		ctx, cancel := context.WithTimeout(base, probeEmbedTimeout)
		defer cancel()

		files, err := llm.ProbeONNXModel(ctx, dir)
		if err != nil {
			return fmt.Errorf("这个目录不能用：%w", err)
		}
		log.Printf("[embed] 已校验自定义模型目录 %s（用的是 %s）", dir, filepath.Base(files.Model))
	}

	if err := a.personas.SetEmbedModelDir(dir); err != nil {
		return err
	}
	a.mu.Lock()
	a.embedModelDir = dir
	a.mu.Unlock()

	if dir == "" {
		log.Printf("[embed] 嵌入模型目录已清回默认（下次启动生效）")
	} else {
		log.Printf("[embed] 嵌入模型目录已设为 %s（下次启动生效）", dir)
	}
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
