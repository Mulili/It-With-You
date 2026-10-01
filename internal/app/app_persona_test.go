package app

import (
	"strings"
	"testing"
	"time"

	"agent-for-you-love/internal/history"
	historystore "agent-for-you-love/internal/history/store"
	"agent-for-you-love/internal/llm"
	memorystore "agent-for-you-love/internal/memory/store"
	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"
	"agent-for-you-love/internal/persona/store"
)

// 这一个内置人格够用：验证"删除自建人格后历史怎么清"不需要更多数据。
func testBuiltins() []builtin.Entry {
	return []builtin.Entry{{
		Persona: persona.Persona{
			ID: "builtin:a", Name: "甲", SeedText: "测试用种子",
			Origin: persona.OriginBuiltin, IsBuiltin: true,
		},
		Rules: []persona.PersonaRule{{
			ID: "builtin:a#0", PersonaID: "builtin:a", Slot: "address_user", Value: "你",
			Source: persona.SourceManual, Tier: persona.TierCore,
		}},
	}}
}

// 这些测试不需要 Wails 运行时：事件发送有 a.ctx == nil 的保护，只会走日志。
//
// 历史用内存实现：这一组关心的是"人格与历史怎么联动"，而不是 PG 本身
// （PG 那条路径由 internal/history/store 的集成测试覆盖）。
//
// 记忆与嵌入都传空：embedder 为 nil 时收尾结算整体停用（见 settlePending），
// 于是这一组测试不会因为后台去调模型或嵌入服务而变得不稳定。
func newPersonaApp(t *testing.T) (*App, persona.Store, history.Store) {
	t.Helper()
	st := store.NewMemoryStore(testBuiltins(), true)
	hist := historystore.NewMemoryStore()
	return NewApp(nil, st, hist, memorystore.NewMemoryStore(), nil, nil), st, hist
}

// 删除人格必须同时清掉它的对话历史：那些消息的 personaID 已失效，
// 留着既不显示、又永远占空间——就是孤儿数据。
func TestDeletePersonaClearsItsHistory(t *testing.T) {
	app, st, hist := newPersonaApp(t)

	id, err := st.CreatePersona("临时人格", "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}

	// 给两个人格各写一条历史（各用各自的会话与片）
	seed := func(personaID, text string) (history.Session, history.Chunk) {
		t.Helper()
		sess, err := hist.EnsureSession(personaID)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		c, err := hist.EnsureChunk(sess.ID, history.ChunkMaxRunes, history.ChunkMaxMessages)
		if err != nil {
			t.Fatalf("取当前片失败: %v", err)
		}
		if _, err := hist.AppendMessage(history.Message{
			SessionID: sess.ID, ChunkID: c.ID, PersonaID: personaID,
			Role: llm.RoleUser, Content: text, Status: history.StatusOK,
		}); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
		return sess, c
	}
	seed("builtin:a", "甲的对话")
	sess, chunk := seed(id, "临时的对话")

	if err := app.DeletePersona(id); err != nil {
		t.Fatalf("删除人格失败: %v", err)
	}

	gone, err := hist.RecentMessages(id, 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("删除后仍有它的历史：%+v", gone)
	}

	kept, err := hist.RecentMessages("builtin:a", 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(kept) != 1 || kept[0].PersonaID != "builtin:a" {
		t.Fatalf("别的人格的历史被误伤了：%+v", kept)
	}

	// 删除时被掐掉的那一轮可能在删除之后才收尾，这条"迟到的半截回复"不能再进历史
	app.appendAssistant(sess.ID, chunk.ID, id, "迟到的半截回复", history.StatusCanceled)
	after, err := hist.RecentMessages(id, 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("已删除人格的回复不该再进历史：%+v", after)
	}
}

// 内置人格只读：存储层就该拦住，别指望 UI 一定先判一次。
func TestDeleteBuiltinPersonaRefused(t *testing.T) {
	app, _, _ := newPersonaApp(t)
	if err := app.DeletePersona("builtin:a"); err == nil {
		t.Fatal("内置人格应当不可删除")
	}
}

// 删除当前人格后，存储层会回退到第一个内置人格；回执里要能说出切到了谁。
func TestDeleteActivePersonaFallsBack(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("会被删掉的", "builtin:a")
	if err != nil {
		t.Fatalf("复制人格失败: %v", err)
	}
	if err := st.SetActivePersona(id); err != nil {
		t.Fatalf("切换失败: %v", err)
	}

	if err := app.DeletePersona(id); err != nil {
		t.Fatalf("删除当前人格失败: %v", err)
	}
	if got := app.activePersonaID(); got != "builtin:a" {
		t.Fatalf("删除当前人格后应回退到内置人格，实际 %q", got)
	}
}

// 设置页要能改"非当前人格"，所以 SaveSeedText 必须带 personaID 而不是只看当前人格。
func TestSaveSeedTextTargetsGivenPersona(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("另一个", "builtin:a")
	if err != nil {
		t.Fatalf("复制人格失败: %v", err)
	}
	if err := app.SaveSeedText(id, "只改它"); err != nil {
		t.Fatalf("保存主体文本失败: %v", err)
	}

	snap := st.Snapshot()
	for _, p := range snap.Personas {
		if p.ID == id && p.SeedText != "只改它" {
			t.Fatalf("目标人格的主体文本没被改成「只改它」，实际 %q", p.SeedText)
		}
		if p.ID == "builtin:a" && p.SeedText != "测试用种子" {
			t.Fatalf("内置人格被连带改了：%q", p.SeedText)
		}
	}
	if err := app.SaveSeedText("builtin:a", "试着改内置"); err == nil {
		t.Fatal("内置人格应当只读")
	}
}

// 「她学到的」的"最后一次被提起"是**注入即算**——刷新点在拼上下文那一轮。
//
// 两条断言各钉一件事：
//   - 进了提示词的那些会被刷成"刚刚"：淘汰（30 天没被提起 → 进缓冲队列）看的就是它，
//     不刷新的话，一条她天天在用的说法会凭空过期；
//   - 刚刷过的不会再写一次（节流）：判据是 30 天，每轮都写纯属白花。
func TestBuildMessagesTouchesInjectedCandidates(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("最后提起测试", "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	fresh := time.Now().UnixMilli()
	if err := st.AddCandidates([]persona.Candidate{
		{PersonaID: id, Slot: "verbosity", Value: "说话简短一点", CreatedAt: old, LastUsedAt: old},
		{PersonaID: id, Slot: "catchphrase", Value: "好耶", CreatedAt: fresh, LastUsedAt: fresh},
	}); err != nil {
		t.Fatalf("准备候选失败: %v", err)
	}

	app.buildMessages(id, []llm.Message{{Role: llm.RoleUser, Content: "在吗"}}, recallResult{})

	got := app.RuleCandidates(id)
	if len(got) != 2 {
		t.Fatalf("应当还是 2 条候选（刷新时间不该动条数），实际 %d 条", len(got))
	}
	for _, c := range got {
		switch c.Value {
		case "说话简短一点":
			if c.LastUsedAt <= old {
				t.Errorf("进了提示词的候选应当刷新最后提起时间，实际还是 %d", c.LastUsedAt)
			}
			if c.CreatedAt != old {
				t.Errorf("刷新不该动创建时间，实际 %d", c.CreatedAt)
			}
		case "好耶":
			// 1 小时以内刚刷过 → 这一轮不写（节流）。判据是 30 天，分钟级精度没有意义
			if c.LastUsedAt != fresh {
				t.Errorf("刚刷过的不该再写一次，实际 %d（期望仍是 %d）", c.LastUsedAt, fresh)
			}
		}
	}
}

// 「她开口问一句」这条路的端到端：预算挤 + 有她学来的规则 → 她问 → 你点头才降 / 你拒绝就永不再提。
//
// 这是整个应用里**唯一一处会改人格的动作**，所以全程必须有人点头：
// 触发条件、问话的注入、两条答复路径，都在这里钉住。
func TestDowngradeAskFlow(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("降级测试", "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}
	if err := st.SetActivePersona(id); err != nil {
		t.Fatalf("切换人格失败: %v", err)
	}
	addPromoted := func(value string) string {
		t.Helper()
		ruleID, err := st.SaveRule(persona.PersonaRule{
			PersonaID: id, Slot: "catchphrase", Value: value,
			Source: persona.SourcePromoted, Tier: persona.TierRecent,
			Kind: persona.KindVolatile, Enabled: true,
		})
		if err != nil {
			t.Fatalf("写规则失败: %v", err)
		}
		return ruleID
	}
	tierOf := func(ruleID string) string {
		t.Helper()
		rules, err := st.RulesOf(id)
		if err != nil {
			t.Fatalf("读规则失败: %v", err)
		}
		for _, r := range rules {
			if r.ID == ruleID {
				return r.Tier
			}
		}
		t.Fatalf("规则 %s 不在了", ruleID)
		return ""
	}

	first := addPromoted("好耶")

	// ① 不挤的时候不许问：她只是学来了一条口头禅，还没到要腾位置的地步。
	//    少了这条约束，她会时不时就问一句"我要不要收起点什么"，很烦
	app.maybeAskDowngrade()
	if p, _ := app.PendingDowngrade(); p != nil {
		t.Fatal("预算还富余，不该去请她问收不收")
	}

	// ② 把主体写到上限 → 想要的注入量接近预算
	if err := st.SaveSeedText(id, strings.Repeat("主", persona.MaxSeedTextRunes)); err != nil {
		t.Fatalf("写主体失败: %v", err)
	}
	app.maybeAskDowngrade()
	pending, err := app.PendingDowngrade()
	if err != nil {
		t.Fatalf("读待回答失败: %v", err)
	}
	if pending == nil || pending.ID != first {
		t.Fatalf("应当有一条在等她回话（那条她学来的），实际 %+v", pending)
	}

	// ③ 这一轮她得真的开口：那段商量要进 system，而且要说清是哪条做法
	msgs := app.buildMessages(id, []llm.Message{{Role: llm.RoleUser, Content: "在吗"}}, recallResult{})
	var sys strings.Builder
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			sys.WriteString(m.Content)
		}
	}
	if !strings.Contains(sys.String(), "想跟他商量一件事") {
		t.Errorf("她该开口问一句，实际 system：\n%s", sys.String())
	}
	if !strings.Contains(sys.String(), "好耶") {
		t.Errorf("得说清是哪条做法，否则她只会讲一句空洞的「我最近变了」：\n%s", sys.String())
	}

	// ④ 你不同意 → 继续守着，而且这条**永不再提**
	if err := app.RefuseDowngrade(first); err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}
	if p, _ := app.PendingDowngrade(); p != nil {
		t.Error("拒绝了就不该还在等回话")
	}
	if got := tierOf(first); got != persona.TierRecent {
		t.Errorf("拒绝之后它该继续生效，实际在 %s 层", got)
	}
	app.maybeAskDowngrade()
	if p, _ := app.PendingDowngrade(); p != nil {
		t.Error("拒绝过的永不再提——反复问同一件事很讨厌")
	}

	// ⑤ 新学来的一条：她可以再问一次；你点头才真的收起来
	second := addPromoted("得嘞")
	app.maybeAskDowngrade()
	if p, _ := app.PendingDowngrade(); p == nil || p.ID != second {
		t.Fatalf("新学来的那条应当重新问一次，实际 %+v", p)
	}
	if err := app.ApproveDowngrade(second); err != nil {
		t.Fatalf("点头失败: %v", err)
	}
	if got := tierOf(second); got != persona.TierArchived {
		t.Errorf("点头之后应当收进归档层，实际在 %s 层", got)
	}
	if p, _ := app.PendingDowngrade(); p != nil {
		t.Error("收起来之后就不该还在等回话（它已经不在近期层了）")
	}
}

// 提升一条候选 = 写成真规则 + 删掉候选；规则落在 recent 层，且**来源记为 promoted**。
//
// source 是"提升"与"情调"的分水岭：候选是"偶尔可以这样"，提升之后进【最近用户希望你】那一段。
// 而 promoted 又特意与 manual 分开：manual 是"用户亲手写的"，promoted 是"她学来的"——
// 预算快满时能请谁让位，靠的正是这个区别（见 persona.DowngradeCandidate）。
func TestPromoteRuleCandidateWritesRuleAndRemovesCandidate(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("候选提升测试", "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}
	if err := st.AddCandidates([]persona.Candidate{{
		PersonaID: id, Slot: "verbosity", Value: "说话简短一点",
		Evidence: "你以后说话能不能别这么啰嗦",
	}}); err != nil {
		t.Fatalf("准备候选失败: %v", err)
	}

	got := app.RuleCandidates(id)
	if len(got) != 1 {
		t.Fatalf("应当有 1 条候选，实际 %d 条", len(got))
	}

	if err := app.PromoteRuleCandidate(got[0]); err != nil {
		t.Fatalf("提升失败: %v", err)
	}

	rules, err := st.RulesOf(id)
	if err != nil {
		t.Fatalf("读规则失败: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("应当写出 1 条规则，实际 %d 条：%+v", len(rules), rules)
	}
	r := rules[0]
	if r.Slot != "verbosity" || r.Value != "说话简短一点" {
		t.Errorf("规则内容不对：%+v", r)
	}
	if r.Source != persona.SourcePromoted || r.Tier != persona.TierRecent {
		t.Errorf("提升后的规则应当是 promoted + recent，实际 %s + %s", r.Source, r.Tier)
	}
	if !r.Enabled {
		t.Error("新规则应当默认启用")
	}
	if r.Evidence == "" {
		t.Error("依据（原话）应当带过来：用户要判断这条值不值得留就看它")
	}
	if left := app.RuleCandidates(id); len(left) != 0 {
		t.Errorf("提升之后候选应当消失，实际还剩 %d 条", len(left))
	}
}

// stable 槽位即使被塞进候选也**提升不了**。
//
// 这条模拟"候选表里混进了脏数据"（抽取阶段本该拦住，但历史数据、被改过的前端都可能塞进来）。
// 注意它挡的不是 SaveRule 那道矩阵——提升路径的 source 记 manual，而 manual 对 stable 槽位
// 本来就放行，闸门必须在 PromoteRuleCandidate 里单独设。挡不住的话，
// "先塞一条 personality 候选、再点提升"就能绕开权限矩阵改掉"她是谁"。
func TestPromoteRuleCandidateRejectsStableSlot(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("候选越权测试", "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}
	// 候选表本身不做校验（它只是一份"她观察到的"），所以这条塞得进去
	if err := st.AddCandidates([]persona.Candidate{{
		PersonaID: id, Slot: "personality", Value: "以后你是个高冷的人",
	}}); err != nil {
		t.Fatalf("准备候选失败: %v", err)
	}

	got := app.RuleCandidates(id)
	if len(got) != 1 {
		t.Fatalf("应当有 1 条候选，实际 %d 条", len(got))
	}
	if err := app.PromoteRuleCandidate(got[0]); err == nil {
		t.Error("stable 槽位的候选必须被拒绝")
	}

	rules, err := st.RulesOf(id)
	if err != nil {
		t.Fatalf("读规则失败: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("被拒了就不该留下规则，实际 %d 条：%+v", len(rules), rules)
	}
	// 候选也要留着：写失败了还把它删掉，等于"这条待办静默消失"，用户再也不知道她想过什么
	if left := app.RuleCandidates(id); len(left) != 1 {
		t.Errorf("提升失败时候选应当保留，实际 %d 条", len(left))
	}
}

// 删掉：候选没了，规则也不该多出来——她学的那条从此不再注入。
func TestDeleteRuleCandidateRemovesWithoutWritingRule(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("候选丢弃测试", "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}
	if err := st.AddCandidates([]persona.Candidate{{
		PersonaID: id, Slot: "catchphrase", Value: "好耶",
	}}); err != nil {
		t.Fatalf("准备候选失败: %v", err)
	}

	got := app.RuleCandidates(id)
	if len(got) != 1 {
		t.Fatalf("应当有 1 条候选，实际 %d 条", len(got))
	}
	if err := app.DeleteRuleCandidate(got[0].ID); err != nil {
		t.Fatalf("删掉失败: %v", err)
	}

	if left := app.RuleCandidates(id); len(left) != 0 {
		t.Errorf("删掉之后候选应当没了，实际还剩 %d 条", len(left))
	}
	rules, err := st.RulesOf(id)
	if err != nil {
		t.Fatalf("读规则失败: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("删掉不该写出规则，实际 %d 条", len(rules))
	}
}
