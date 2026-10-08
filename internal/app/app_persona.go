package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/ui"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// 这个文件装人格系统的全部对外方法：读取、写入、导入导出，以及"用户提长期要求"那条链路。
//
// 它们都走同一个范式：**校验与规则在 internal/persona 里，这里只做编排**——
// 取当前人格、调存储、发 persona:changed 回执。所有写入方法都发事件，
// 前端据此刷新菜单并用 summary 弹一条回执（见 notifyPersonaChanged）。

// DirectiveTimeout 是抽取显式指令的超时时间。
// 它跑在独立 ctx 上（不挂 prevCancel）：用户发下一句不该把"记住我的要求"这件事掐掉。
const DirectiveTimeout = 30 * time.Second

// candidateDisplayLimit 是一次最多返回几条「她学到的」。
//
// 它管的是**展示**：攒到几十条还没处理的用户需要的是"看一眼、批量清掉"，
// 而不是一屏几百行。数据本身不设上限（丢弃是显式操作，不该被动丢）。
const candidateDisplayLimit = 30

// moodInjectLimit 是注入时一回读几条候选。
//
// 比展示上限大，是为了防"某一个槽位攒了很多条、把别的槽位的候选挤出读取窗口"——
// 真正进提示词的还要过一道"每槽位只留最新一条"（见 persona.moodCandidates），
// 而 volatile 槽位统共几个，所以拉宽一点就够，不必精确。
const moodInjectLimit = 200

// activePersonaID 返回当前生效人格的 ID；没有人格时返回空串。
func (a *App) activePersonaID() string {
	if a.personas == nil {
		return ""
	}
	return a.personas.Snapshot().ActiveID
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
	out, err := a.chatJSON(ctx, system, user)
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
	a.emit(ui.EventPersonaChanged, ui.PersonaChangedPayload{
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

// RuleCandidates 返回**指定人格**「她学到的」——她自己从对话里琢磨出来的说话倾向。
//
// 带 personaID 而不是只给当前人格，理由与 RulesOf 一样：编辑器要能改任意人格。
//
// 注意这些东西**已经在生效了**（以【偶尔可以这样】的措辞注入，见 persona.BuildSystemPrompt），
// 不是一张待办清单。界面上的两个动作是"让她一直这样"（提升成真规则）与"删掉"，
// 而不是"准不准她用"——用户的体验应当是"她已经会了，不喜欢可以撤"。
func (a *App) RuleCandidates(personaID string) []persona.Candidate {
	if a.personas == nil || personaID == "" {
		return nil
	}
	rows, err := a.personas.ListCandidates(personaID, candidateDisplayLimit)
	if err != nil {
		log.Printf("[persona] 读取「她学到的」失败: %v", err)
		return nil
	}
	return rows
}

// PromoteRuleCandidate 把她琢磨出来的一条倾向**提升成真规则**：「让她一直这样」。
//
// 顺序不能反（先写规则、再删候选）：反过来的话，写规则失败时候选已经没了——
// 用户会以为"提升成功但它没生效"，而且再也找不回那条东西。反过来（规则成了、删候选失败）
// 只是候选多留一条，再点一次就是幂等的覆盖。两害相权，留下看得见的痕迹。
//
// 两个关键决定：
//   - Source 记 **promoted 而不是 inferred**。内容是模型抽的没错，但"以后就这么说"这个决定
//     是用户下的，这条规则从此是基准，进【最近用户希望你】那一段，不再是"偶尔可以这样"。
//     记成 inferred 会让它永远停在情调强度上，用户点了半天没反应。
//     ⚠️ 也不用 manual 了（2026-09-29 改）：manual 是"用户亲手写的"，而这条是**她学来的**——
//     预算快满时要请谁让位，靠的正是这个区别（见 persona.DowngradeCandidate）。
//   - Tier 仍是 recent。提升的是**强度**，不是身份——她说话的语气不该混进 core
//     （那一层是"她是谁"）。唯一的例外是覆盖到已有的 core 单值槽位（如称呼），
//     那时 SaveRule 的"单值覆盖"会沿用旧的 tier，那是**用户主动**要替换基准，符合预期。
//
// 校验里有一处**不能**交给 SaveRule：槽位必须是 volatile。
// 因为矩阵是按 source 判权限的，而这里 source 与 manual 同级放行 stable 槽位——
// 于是"先往候选表塞一条 personality、再点提升"就能绕开权限矩阵。这条约束只属于提升路径。
func (a *App) PromoteRuleCandidate(c persona.Candidate) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	slot := persona.CanonicalizeSlot(c.Slot)
	spec, ok := persona.LookupSlot(slot)
	if !ok || spec.Kind != persona.KindVolatile {
		// 候选的语义就是"自动演化允许的那些维度的观察"，stable 出现在这里说明数据已经坏了
		//（抽取层保证不会）。用户想改"她是谁"应当去规则编辑器，不是靠提升一条观察。
		return fmt.Errorf("槽位 %s 不能从候选提升（候选只涵盖可自动演化的维度）", persona.SlotLabel(slot))
	}
	if _, err := a.personas.SaveRule(persona.PersonaRule{
		PersonaID: c.PersonaID,
		Slot:      slot,
		Value:     c.Value,
		Source:    persona.SourcePromoted,
		Tier:      persona.TierRecent,
		Evidence:  c.Evidence,
	}); err != nil {
		return fmt.Errorf("提升规则候选失败: %w", err)
	}

	if err := a.personas.DeleteCandidate(c.ID); err != nil {
		// 规则已经生效了，这一步失败只是候选多留一条：不往外抛，
		// 报错会让用户以为没提升成功，再点一次反而重复
		log.Printf("[persona] 提升成功，但清理候选失败（候选可以再点一次删掉）: %v", err)
	}
	a.notifyPersonaChanged(c.PersonaID, persona.ActionUpdate,
		fmt.Sprintf("她以后会一直这样：%s → %s", persona.SlotLabel(slot), c.Value))
	return nil
}

// DeleteRuleCandidate 删掉一条「她学到的」（不写规则）。
//
// 它可能正在以情调方式生效，删掉即不再注入。
func (a *App) DeleteRuleCandidate(id string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("没有指定要删掉的候选")
	}
	if err := a.personas.DeleteCandidate(id); err != nil {
		return fmt.Errorf("删掉候选失败: %w", err)
	}
	return nil
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
	// 它的归档索引要跟着走：PG 侧靠 rule_index 的外键级联（ON DELETE CASCADE）自动清，
	// 内存实现没有级联，靠这次扫描按"索引还在、规则已经不在归档层"把它删掉。
	// 不删的症状是"检索命中一条已经不存在的规则"，她就会说起一件并不存在的事
	go a.indexArchivedRules()
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
	// 停用的若是归档规则，它的索引得跟着撤掉（归档索引只收"启用的归档规则"）；
	// 启用一条归档规则同理要补上。这次扫描顺带把两边都对平
	go a.indexArchivedRules()
	action, prefix := persona.ActionDisable, "已停用 "
	if enabled {
		action, prefix = persona.ActionEnable, "已启用 "
	}
	a.notifyPersonaChanged(personaID, action, prefix+label)
	return nil
}

// pendingDowngrade 从规则里挑出"她问过、还在等他回话"的那一条。
//
// 判据全是规则上已有的字段，**不需要额外的状态**：
//   - 问过（DowngradeAskedAt ≠ 0）、没被拒绝（DowngradeRefusedAt = 0）；
//   - 还在近期层——用户点头之后它就被收进归档层了，于是自动不再是"待回答"。
//
// 它是个纯函数（吃快照里那份规则），因为它要在两个地方用：buildMessages 每轮都要读
// （决定这一轮要不要注入那段商量），而 PendingDowngrade 给界面读。两处各查一次库没必要。
func pendingDowngrade(rules []persona.PersonaRule) (persona.PersonaRule, bool) {
	for _, r := range rules {
		if r.Tier != persona.TierRecent || !r.Enabled {
			continue
		}
		if r.DowngradeAskedAt != 0 && r.DowngradeRefusedAt == 0 {
			return r, true
		}
	}
	return persona.PersonaRule{}, false
}

// downgradeAskNote 是她"想跟他商量收起来某条做法"时注入的那一小段。
//
// 只给**大意与要点**，措辞交给她——这正是这条设计要的效果：一个能自然开口的请求，
// 而不是一条系统通知。三件事必须写进要点里：
//  1. 是哪条做法（否则她只会说一句空洞的"我最近变了"）；
//  2. 她现在可以不用再守着它了（否则她会以为自己在违背什么）；
//  3. **他不同意就继续守着**——少了这句，"问"就变成了通知。
//
// 外加两条克制：别硬插、只问这一次（她要是没接话，这件事就此放下）。
//
// ⚠️ 不要提"预算""位置"这类词：那是系统内部的账，她嘴里的理由应该是"我最近不太这样了"。
func downgradeAskNote(r persona.PersonaRule) string {
	return fmt.Sprintf(`
【想跟他商量一件事】
你学到的这条，你现在觉得自己已经不太这样了：%s：%s
这一轮找个自然的时候，用你自己的话问问他：以后还要不要继续这样。
要说清三件事：① 是哪条做法；② 你已经不太这样了；③ 他要是不同意，你就继续守着。
⚠️ 别硬插、别反复提；他要是没接话，这件事就此放下。
`, persona.SlotLabel(r.Slot), strings.TrimSpace(r.Value))
}

// PendingDowngrade 返回"她问过、还在等你回话"的那条规则（没有就是 nil）。
//
// 界面据此显示一条"她在等你回话"，给两个按钮。**这是那条提议目前的唯一正式回答入口**：
// 口头回答（"好呀"）暂时不会被识别——因为"误判成同意"会直接改掉人格，方向太危险；
// 先把确定的入口做通，识别口头回答留到看过真机上她问得怎么样之后再说。
func (a *App) PendingDowngrade() (*persona.PersonaRule, error) {
	if a.personas == nil {
		return nil, nil
	}
	r, ok := pendingDowngrade(a.personas.Snapshot().Rules)
	if !ok {
		return nil, nil
	}
	return &r, nil
}

// ApproveDowngrade 你点头了：把那条收起来（降到归档层，之后仍能被检索回来）。
//
// 直接复用 ArchiveRule：收起来这件事只有一条路，不然"她提议的"与"我手动收的"
// 迟早会在某处走样（比如漏了补索引那一步）。
func (a *App) ApproveDowngrade(ruleID string) error {
	return a.ArchiveRule(ruleID)
}

// RefuseDowngrade 你不愿意：那就继续守着，而且**这条以后不再提**。
//
// 记的是 RefusedAt 而不是清掉 AskedAt：只清"问过"的话，她过一阵又会拿同一件事来问，
// 那是很讨厌的执着（见 persona.PersonaRule 上那两列的说明）。
func (a *App) RefuseDowngrade(ruleID string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	label, personaID := a.ruleLabel(ruleID)
	if err := a.personas.MarkDowngradeRefused(ruleID, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("记下你的答复失败: %w", err)
	}
	// action 只用于客户端的记账（前端目前只读 summary），这里没有更贴切的取值
	a.notifyPersonaChanged(personaID, persona.ActionUpdate, "好，那就不收，继续这样："+label)
	return nil
}

// ArchiveRule 把一条规则收进归档层（界面上是「收起来」）。
//
// 归档之后它**不再每轮注入**，但可以被检索回来（见 persona.TierArchived 与 indexArchivedRules）。
// 所以这比"删掉"温和：她不再保持这个做法，但需要的时候还能想起来自己以前是这样。
//
// 只对「近期」层开放（core 是"她是谁"，要取消就直说删除），见 persona.Store.ArchiveRule。
func (a *App) ArchiveRule(id string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	label, personaID := a.ruleLabel(id)
	if err := a.personas.ArchiveRule(id); err != nil {
		return err
	}
	// 立刻补索引，而且**后台跑**：不补的话它检索不回来，这次"收起来"就等于删掉；
	// 但嵌入要几十毫秒到几秒，界面不该等它（补不上也没关系，下次写规则或启动时会再补）
	go a.indexArchivedRules()
	a.notifyPersonaChanged(personaID, persona.ActionArchive, "已收起来 "+label+"（需要时她还能想起来）")
	return nil
}

// ReviveRule 把一条归档规则放回近期层（界面上是「放回来」）。
//
// 与 ArchiveRule 对称——**只有降没有升，归档就等于删除**。
// 除了用户手动点，检索命中之后她自标"用到了"也会走这条（见 markUsed）：
// 那是这套机制里的复位阀门，"她其实还在用"就该回来。
func (a *App) ReviveRule(id string) error {
	if err := a.requireStore(); err != nil {
		return err
	}
	label, personaID := a.ruleLabel(id)
	if err := a.personas.ReviveRule(id); err != nil {
		return err
	}
	// 索引**当场**删掉、不等后台扫描：它回到活跃层之后每轮都注入 system，
	// 索引留着会让同一件事既在人格提示词里、又在回忆块里再出现一遍
	if a.memories != nil {
		if err := a.memories.DropRuleIndex(id); err != nil {
			log.Printf("[archive] 放回规则时清理它的索引失败: %v", err)
		}
	}
	a.notifyPersonaChanged(personaID, persona.ActionRevive, "已放回来 "+label)
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

// notifyPersonaChanged 通知前端"人格数据变了"：菜单与设置浮层据此刷新，并用 summary 弹一条回执。
func (a *App) notifyPersonaChanged(personaID, action, summary string) {
	log.Printf("[persona] %s", summary)
	a.emit(ui.EventPersonaChanged, ui.PersonaChangedPayload{
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
