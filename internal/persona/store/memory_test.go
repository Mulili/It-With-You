package store

import (
	"strings"
	"testing"

	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"
)

// 测试用内置人格：两条规则分别落在 volatile 与 stable 槽位，方便验权限矩阵。
func testBuiltins() []builtin.Entry {
	return []builtin.Entry{
		{
			Persona: persona.Persona{ID: "builtin:a", Name: "甲", SeedText: "甲的人格种子", Origin: persona.OriginBuiltin, IsBuiltin: true},
			Rules: []persona.PersonaRule{
				{ID: "builtin:a#0", PersonaID: "builtin:a", Slot: "address_user", Value: "你",
					Source: persona.SourceManual, Tier: persona.TierCore, Kind: persona.KindVolatile, Priority: 10, Enabled: true},
				{ID: "builtin:a#1", PersonaID: "builtin:a", Slot: "taboo", Value: "不要长篇大论",
					Source: persona.SourceManual, Tier: persona.TierCore, Kind: persona.KindStable, Priority: 5, Enabled: true},
			},
		},
		{
			Persona: persona.Persona{ID: "builtin:b", Name: "乙", SeedText: "乙的人格种子", Origin: persona.OriginBuiltin, IsBuiltin: true},
		},
	}
}

// newTestStore 返回一个内存存储，并注入"每次调用 +10ms"的假时钟，让时间可预测。
func newTestStore() *MemoryStore {
	s := NewMemoryStore(testBuiltins(), true)
	tick := int64(1_000_000)
	s.now = func() int64 {
		tick += 10
		return tick
	}
	return s
}

func TestSnapshotDefaults(t *testing.T) {
	s := newTestStore()
	snap := s.Snapshot()

	if !snap.StorageReady {
		t.Error("内存实现应当始终 StorageReady")
	}
	if snap.ActiveID != "builtin:a" {
		t.Errorf("默认应选中第一个内置人格，实际 %q", snap.ActiveID)
	}
	if len(snap.Personas) != 2 {
		t.Fatalf("人格数 = %d，期望 2", len(snap.Personas))
	}
	if len(snap.Rules) != 2 {
		t.Fatalf("当前人格的规则数 = %d，期望 2", len(snap.Rules))
	}
	// 快照里的规则必须是副本：改它不该影响存储内部
	snap.Rules[0].Value = "被改了"
	if again := s.Snapshot(); again.Rules[0].Value == "被改了" {
		t.Error("快照返回的规则与内部数据共享了内存，外部改动污染了真源")
	}
}

func TestBuiltinIsReadOnly(t *testing.T) {
	s := newTestStore()

	cases := []struct {
		name string
		call func() error
	}{
		{"改种子文本", func() error { return s.SaveSeedText("builtin:a", "新文本") }},
		{"改名字", func() error { return s.RenamePersona("builtin:a", "新名字") }},
		{"删人格", func() error { return s.DeletePersona("builtin:a") }},
		{"加规则", func() error {
			_, err := s.SaveRule(persona.PersonaRule{PersonaID: "builtin:a", Slot: "tone", Value: "冷淡"})
			return err
		}},
		{"删规则", func() error { return s.DeleteRule("builtin:a#0") }},
		{"停用规则", func() error { return s.SetRuleEnabled("builtin:a#0", false) }},
	}
	for _, c := range cases {
		err := c.call()
		if err == nil {
			t.Errorf("%s：应当被拒绝", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "内置人格") {
			t.Errorf("%s：报错应说明是内置人格，实际 %v", c.name, err)
		}
	}
}

func TestCreateFromBuiltinCopiesRules(t *testing.T) {
	s := newTestStore()

	id, err := s.CreatePersona("我的甲", "builtin:a")
	if err != nil {
		t.Fatalf("从内置复制失败: %v", err)
	}
	if id == "" || id == "builtin:a" {
		t.Fatalf("新人格 ID 不合法: %q", id)
	}
	if err := s.SetActivePersona(id); err != nil {
		t.Fatalf("切到新人格失败: %v", err)
	}

	snap := s.Snapshot()
	var created persona.Persona
	for _, p := range snap.Personas {
		if p.ID == id {
			created = p
		}
	}
	if created.Origin != persona.OriginUser || created.IsBuiltin {
		t.Errorf("复制出来的人格应当是用户人格: %+v", created)
	}
	if created.SeedText != "甲的人格种子" {
		t.Errorf("种子文本没被复制: %q", created.SeedText)
	}
	if len(snap.Rules) != 2 {
		t.Fatalf("规则没被复制，实际 %d 条", len(snap.Rules))
	}
	for _, r := range snap.Rules {
		if r.PersonaID != id {
			t.Errorf("复制出的规则没归属到新人格: %+v", r)
		}
		if strings.HasPrefix(r.ID, "builtin:") {
			t.Errorf("复制出的规则沿用了内置规则的 ID: %s", r.ID)
		}
		if r.Source != persona.SourceManual {
			t.Errorf("复制出的规则来源应为 manual（它已经是用户自己的东西）: %s", r.Source)
		}
	}

	// 原内置人格的规则不能被改动
	builtinRules := s.rules["builtin:a"]
	if builtinRules[0].ID != "builtin:a#0" || len(builtinRules) != 2 {
		t.Errorf("内置人格的规则被复制过程改动了: %+v", builtinRules)
	}
}

func TestSaveRuleSlotConsistency(t *testing.T) {
	s := newTestStore()
	id, err := s.CreatePersona("测试", "")
	if err != nil {
		t.Fatal(err)
	}

	firstID, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "address_user", Value: "你"})
	if err != nil {
		t.Fatalf("第一条单值槽位规则应当能写入: %v", err)
	}
	// 单值槽位再写入 = **覆盖**（同槽位不并存），且沿用原规则 ID
	secondID, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "address_user", Value: "主人"})
	if err != nil {
		t.Fatalf("单值槽位再次写入应当覆盖而不是报错: %v", err)
	}
	if secondID != firstID {
		t.Errorf("覆盖应沿用原规则 ID，实际 %s → %s", firstID, secondID)
	}
	if got := storedRules(s, id); len(got) == 0 || got[0].Value != "主人" {
		t.Errorf("覆盖后取值不对: %+v", got)
	}

	// 多值槽位可以多条，但同值要去重
	if _, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "taboo", Value: "别提工作"}); err != nil {
		t.Fatalf("多值槽位第一条应当能写入: %v", err)
	}
	if _, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "taboo", Value: "别说脏话"}); err != nil {
		t.Fatalf("多值槽位第二条应当能写入: %v", err)
	}
	_, err = s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "taboo", Value: "别提工作"})
	if err == nil || !strings.Contains(err.Error(), "相同取值") {
		t.Fatalf("多值槽位重复取值应被拒绝，实际: %v", err)
	}

	// kind 与槽位表不符 → 拒绝
	_, err = s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "address_user", Kind: persona.KindStable, Value: "x"})
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("kind 写错应被拒绝，实际: %v", err)
	}

	// 槽位不存在 → 拒绝
	_, err = s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "mood", Value: "x"})
	if err == nil || !strings.Contains(err.Error(), "不是规范槽位") {
		t.Fatalf("未知槽位应被拒绝，实际: %v", err)
	}
}

// 权限矩阵：这是防人格漂移的主闸门，单独钉一遍。
func TestCheckWriteMatrix(t *testing.T) {
	cases := []struct {
		source string
		slot   string
		tier   string
		ok     bool
	}{
		// manual 全放开
		{persona.SourceManual, "personality", persona.TierCore, true},
		{persona.SourceManual, "taboo", persona.TierArchived, true},
		// explicit：任何槽位，但只能 core / recent
		{persona.SourceExplicit, "personality", persona.TierCore, true},
		{persona.SourceExplicit, "taboo", persona.TierRecent, true},
		{persona.SourceExplicit, "taboo", persona.TierArchived, false},
		// inferred：只能 volatile 的 recent
		{persona.SourceInferred, "address_user", persona.TierRecent, true},
		{persona.SourceInferred, "personality", persona.TierRecent, false}, // stable 槽位
		{persona.SourceInferred, "taboo", persona.TierRecent, false},       // stable 槽位
		{persona.SourceInferred, "address_user", persona.TierCore, false},  // 只能 recent
		// 非法输入
		{"guessed", "tone", persona.TierCore, false},
		{persona.SourceManual, "mood", persona.TierCore, false},
		{persona.SourceManual, "tone", "longterm", false},
	}
	for _, c := range cases {
		err := persona.CheckWrite(c.source, c.slot, c.tier)
		if c.ok && err != nil {
			t.Errorf("CheckWrite(%s, %s, %s) 应当通过，实际: %v", c.source, c.slot, c.tier, err)
		}
		if !c.ok && err == nil {
			t.Errorf("CheckWrite(%s, %s, %s) 应当被拒绝", c.source, c.slot, c.tier)
		}
	}
}

func TestSaveRuleRejectsInferredOnStableSlot(t *testing.T) {
	s := newTestStore()
	id, _ := s.CreatePersona("测试", "")

	_, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "taboo", Value: "别提工作",
		Source: persona.SourceInferred, Tier: persona.TierRecent})
	if err == nil {
		t.Fatal("模型自动抽取写 stable 槽位应当被拒绝")
	}
	t.Logf("按预期拒绝: %v", err)

	// 同一个槽位由用户明说就可以写
	if _, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "taboo", Value: "别提工作",
		Source: persona.SourceExplicit, Tier: persona.TierRecent, Evidence: "以后别跟我提工作"}); err != nil {
		t.Fatalf("用户明说的指令应当能写 stable 槽位: %v", err)
	}
}

func TestChangeLogAndSnapshot(t *testing.T) {
	s := newTestStore()
	id, _ := s.CreatePersona("测试", "")
	if err := s.SetActivePersona(id); err != nil { // 快照里的变更记录也按当前人格过滤
		t.Fatal(err)
	}

	if err := s.SaveSeedText(id, "你是谁谁谁"); err != nil {
		t.Fatal(err)
	}
	ruleID, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "tone", Value: "温和"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRuleEnabled(ruleID, false); err != nil {
		t.Fatal(err)
	}

	snap := s.Snapshot()
	if len(snap.RecentChanges) < 3 {
		t.Fatalf("变更记录应有 3 条，实际 %d 条", len(snap.RecentChanges))
	}
	if snap.RecentChanges[0].Action != persona.ActionDisable {
		t.Errorf("变更记录应当最新的在前，实际首条是 %s", snap.RecentChanges[0].Action)
	}
	// 变更记录里要能看出"改了什么"
	var found bool
	for _, c := range snap.RecentChanges {
		if c.Field == "seedText" && c.NewValue == "你是谁谁谁" {
			found = true
		}
	}
	if !found {
		t.Errorf("主体文本的改动没进变更记录: %+v", snap.RecentChanges)
	}
}

func TestRuleUpdateKeepsCreatedAtAndEnabled(t *testing.T) {
	s := newTestStore()
	id, _ := s.CreatePersona("测试", "")
	if err := s.SetActivePersona(id); err != nil { // 快照返回的是"当前人格"的规则
		t.Fatal(err)
	}
	ruleID, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "tone", Value: "温和"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRuleEnabled(ruleID, false); err != nil {
		t.Fatal(err)
	}

	snap := s.Snapshot()
	createdAt := snap.Rules[0].CreatedAt

	// 改值：不应复活被停用的规则，也不应改掉创建时间
	if _, err := s.SaveRule(persona.PersonaRule{ID: ruleID, PersonaID: id, Slot: "tone", Value: "冷淡", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	snap = s.Snapshot()
	if snap.Rules[0].Value != "冷淡" {
		t.Errorf("取值没更新: %+v", snap.Rules[0])
	}
	if snap.Rules[0].Enabled {
		t.Error("更新取值不应顺手把停用的规则启用（启停走专门的方法）")
	}
	if snap.Rules[0].CreatedAt != createdAt {
		t.Error("更新不应改掉创建时间")
	}
}

func TestDeletePersonaFallsBackToBuiltin(t *testing.T) {
	s := newTestStore()
	id, _ := s.CreatePersona("临时", "")
	if err := s.SetActivePersona(id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePersona(id); err != nil {
		t.Fatal(err)
	}

	snap := s.Snapshot()
	if snap.ActiveID != "builtin:a" {
		t.Errorf("删掉当前人格后应回退到内置人格，实际 %q", snap.ActiveID)
	}
	for _, p := range snap.Personas {
		if p.ID == id {
			t.Error("人格没被删掉")
		}
	}
}

func TestImportSameNameCreatesCopy(t *testing.T) {
	s := newTestStore()

	f := persona.PersonaFile{
		SchemaVersion: persona.FileSchemaVersion,
		Persona:       persona.PersonaFileHead{Name: "甲", SeedText: "导入的种子"},
		Rules: []persona.PersonaFileRule{
			{Slot: "tone", Value: "活泼", Evidence: "文件里不该带依据"},
		},
	}
	got, err := s.ImportFile(f)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if got.Origin != persona.OriginImported || got.IsBuiltin {
		t.Errorf("导入出来的人格来源不对: %+v", got)
	}
	if got.Name == "甲" {
		t.Error("与已有内置人格重名时应当改存副本，而不是同名并存")
	}
	if !strings.Contains(got.Name, "甲") {
		t.Errorf("副本名应当仍能看出原名，实际 %q", got.Name)
	}

	if err := s.SetActivePersona(got.ID); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if len(snap.Rules) != 1 {
		t.Fatalf("导入的规则数 = %d，期望 1", len(snap.Rules))
	}
	if snap.Rules[0].Evidence != "" {
		t.Error("导入不该带进 evidence（那是来源机器的审计数据）")
	}
	if strings.HasPrefix(snap.Rules[0].ID, "甲") || snap.Rules[0].ID == "" {
		t.Errorf("导入的规则应当重新生成 ID，实际 %q", snap.Rules[0].ID)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	s := newTestStore()

	// 导出内置人格（这正是产出内置人格文件的通道）
	f, err := s.ExportFile("builtin:a")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if f.SchemaVersion != persona.FileSchemaVersion || f.ExportedAt == 0 {
		t.Errorf("导出文件头不对: %+v", f)
	}
	if f.Persona.Name != "甲" || f.Persona.SeedText != "甲的人格种子" {
		t.Errorf("导出内容不对: %+v", f.Persona)
	}
	if len(f.Rules) != 2 {
		t.Fatalf("导出的规则数 = %d，期望 2", len(f.Rules))
	}

	// 导出 → 直接导入，内容应当能还原（除了 ID 与来源机器数据）
	got, err := s.ImportFile(f)
	if err != nil {
		t.Fatalf("重新导入失败: %v", err)
	}
	if got.SeedText != "甲的人格种子" {
		t.Errorf("往返后种子文本不一致: %q", got.SeedText)
	}
	if err := s.SetActivePersona(got.ID); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if len(snap.Rules) != 2 {
		t.Fatalf("往返后规则数 = %d，期望 2", len(snap.Rules))
	}
	for _, r := range snap.Rules {
		if r.Source == "" || r.Tier == "" || r.Kind == "" {
			t.Errorf("往返后规则丢了字段: %+v", r)
		}
	}
}

func TestSortRules(t *testing.T) {
	rules := []persona.PersonaRule{
		{Slot: "a", Tier: persona.TierArchived, Priority: 100, UpdatedAt: 3},
		{Slot: "b", Tier: persona.TierRecent, Priority: 1, UpdatedAt: 1},
		{Slot: "c", Tier: persona.TierCore, Priority: 5, UpdatedAt: 2},
		{Slot: "d", Tier: persona.TierCore, Priority: 9, UpdatedAt: 1},
		{Slot: "e", Tier: persona.TierCore, Priority: 9, UpdatedAt: 7},
	}
	persona.SortRules(rules)

	want := []string{"e", "d", "c", "b", "a"}
	for i, w := range want {
		if rules[i].Slot != w {
			t.Fatalf("排序结果 = %v，期望 %v", slotsOf(rules), want)
		}
	}
}

// 规则与变更记录都按人格隔离：人格在用户眼里是独立个体，切过去不该看到别人的东西。
func TestSnapshotIsPerPersona(t *testing.T) {
	s := newTestStore()
	aID, err := s.CreatePersona("甲的人格", "")
	if err != nil {
		t.Fatal(err)
	}
	bID, err := s.CreatePersona("乙的人格", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetActivePersona(aID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRule(persona.PersonaRule{PersonaID: aID, Slot: "tone", Value: "温和"}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetActivePersona(bID); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if len(snap.Rules) != 0 {
		t.Errorf("新人格不该有别人格的规则，实际 %d 条", len(snap.Rules))
	}
	if len(snap.RecentChanges) != 0 {
		t.Errorf("新人格不该看到别人格的变更，实际 %d 条：%+v", len(snap.RecentChanges), snap.RecentChanges)
	}

	if err := s.SetActivePersona(aID); err != nil {
		t.Fatal(err)
	}
	snap = s.Snapshot()
	if len(snap.Rules) != 1 {
		t.Errorf("切回来应当看到自己的 1 条规则，实际 %d 条", len(snap.Rules))
	}
	if len(snap.RecentChanges) == 0 {
		t.Error("切回来应当能看到自己人格的变更记录")
	}
}

func slotsOf(rules []persona.PersonaRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Slot)
	}
	return out
}

// storedRules 直接读存储内部的规则（同包测试），保留真实 ID。
// 特意不走 ExportFile：导出文件里的规则 ID 是按序号重新生成的。
func storedRules(s *MemoryStore, personaID string) []persona.PersonaRule {
	out := append([]persona.PersonaRule(nil), s.rules[personaID]...)
	persona.SortRules(out)
	return out
}

// 覆盖语义的两个关键点：必须真的生效（不能被旧的停用状态吃掉），也不该顺手改变层级。
func TestSaveRuleUpsertReenablesAndKeepsTier(t *testing.T) {
	s := newTestStore()
	id, err := s.CreatePersona("我的甲", "builtin:a") // 复制内置：address_user 原本在 core 层
	if err != nil {
		t.Fatal(err)
	}

	var ruleID string
	for _, r := range storedRules(s, id) {
		if r.Slot == "address_user" {
			ruleID = r.ID
			if r.Tier != persona.TierCore {
				t.Fatalf("复制出来的 address_user 原本应在 %s 层，实际 %s", persona.TierCore, r.Tier)
			}
		}
	}
	if ruleID == "" {
		t.Fatal("复制出来的人格应当带有 address_user 规则")
	}
	if err := s.SetRuleEnabled(ruleID, false); err != nil {
		t.Fatal(err)
	}

	// 用户改口："以后叫我老板"——走的是显式指令那条路（explicit + recent）
	newID, err := s.SaveRule(persona.PersonaRule{
		PersonaID: id, Slot: "address_user", Value: "老板",
		Source: persona.SourceExplicit, Tier: persona.TierRecent, Evidence: "以后叫我老板",
	})
	if err != nil {
		t.Fatalf("覆盖应成功: %v", err)
	}
	if newID != ruleID {
		t.Errorf("覆盖应沿用原规则 ID：%s → %s", ruleID, newID)
	}

	for _, r := range storedRules(s, id) {
		if r.Slot != "address_user" {
			continue
		}
		if r.Value != "老板" {
			t.Errorf("取值没被覆盖: %q", r.Value)
		}
		if !r.Enabled {
			t.Error("覆盖旧值时必须重新启用——否则用户明确说了却不生效，原因还藏在一张停用的旧规则里")
		}
		if r.Tier != persona.TierCore {
			t.Errorf("原本在 %s 层的身份级规则不该因为改个值被降级，实际 %q", persona.TierCore, r.Tier)
		}
	}
}

// 编辑器要能改"非当前人格"的规则：RulesOf 按 ID 取，而不是只看当前人格。
// 若它退化成"只返回当前人格的规则"，最直接的后果是想改别的人格必须先切过去——
// 而切换会掐掉正在生成的那一轮并清空气泡，副作用太大。
func TestRulesOfAnyPersona(t *testing.T) {
	s := newTestStore()

	builtinRules, err := s.RulesOf("builtin:a")
	if err != nil {
		t.Fatalf("取内置人格的规则失败: %v", err)
	}
	if len(builtinRules) == 0 {
		t.Fatal("内置人格应当有规则")
	}

	// 新建的副本不是当前人格，同样要能取到它自己的规则
	id, err := s.CreatePersona("甲 的副本", "builtin:a")
	if err != nil {
		t.Fatalf("复制人格失败: %v", err)
	}
	if s.Snapshot().ActiveID == id {
		t.Fatal("这一步的副本不该是当前人格，否则测不到「非当前人格」这条路径")
	}
	copied, err := s.RulesOf(id)
	if err != nil {
		t.Fatalf("取非当前人格的规则失败: %v", err)
	}
	if len(copied) != len(builtinRules) {
		t.Errorf("副本规则数 = %d，期望与来源一致（%d）", len(copied), len(builtinRules))
	}
	for _, r := range copied {
		if r.PersonaID != id {
			t.Errorf("返回了别人格的规则：%+v", r)
		}
	}

	// 返回顺序必须与注入顺序一致（与 SortRules 同一口径）
	lastRank := 0
	for i, r := range copied {
		rank, ok := map[string]int{persona.TierCore: 0, persona.TierRecent: 1, persona.TierArchived: 2}[r.Tier]
		if !ok {
			t.Fatalf("规则 %s 的 tier = %q 非法", r.Slot, r.Tier)
		}
		if i > 0 && rank < lastRank {
			t.Errorf("规则顺序不是 tier 升序：%s 出现在更靠后的层级之后", r.Tier)
		}
		lastRank = rank
	}

	// 人格不存在时要报错，而不是返回一个空列表——两者在界面上长得一样，但含义完全不同
	if _, err := s.RulesOf("builtin:不存在"); err == nil {
		t.Error("不存在的人格应当返回错误，而不是空规则列表")
	}
}

// 变更记录同样按人格隔离，且编辑器要能看"非当前人格"的变更。
func TestChangesOfAnyPersona(t *testing.T) {
	s := newTestStore()

	id, err := s.CreatePersona("甲 的副本", "builtin:a")
	if err != nil {
		t.Fatalf("复制人格失败: %v", err)
	}
	if err := s.SaveSeedText(id, "改过的种子"); err != nil {
		t.Fatalf("改主体文本失败: %v", err)
	}

	changes, err := s.ChangesOf(id)
	if err != nil {
		t.Fatalf("取非当前人格的变更失败: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("刚改过主体文本，应当留下变更记录")
	}
	for _, c := range changes {
		if c.PersonaID != id {
			t.Errorf("混入了别人格的变更：%+v", c)
		}
	}
	if len(changes) > 1 && changes[0].CreatedAt < changes[1].CreatedAt {
		t.Errorf("变更记录应当按时间倒序：%d 排在 %d 之前", changes[0].CreatedAt, changes[1].CreatedAt)
	}

	// 内置人格不落库、没有变更记录，但这是"空"而不是"错误"
	if got, err := s.ChangesOf("builtin:a"); err != nil || len(got) != 0 {
		t.Errorf("内置人格应当返回空变更且不报错，实际 len=%d err=%v", len(got), err)
	}

	if _, err := s.ChangesOf("builtin:不存在"); err == nil {
		t.Error("不存在的人格应当返回错误，而不是空变更列表")
	}
}
