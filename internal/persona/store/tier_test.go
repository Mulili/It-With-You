package store

import (
	"testing"
	"time"

	"agent-for-you-love/internal/persona"
)

// 归档 / 放回（TierRecent ↔ TierArchived）的契约：同一套断言跑内存与 PG 两种实现。
//
// 这一对动作是"降层不等于删除"的落点：**只有降没有升，归档就等于删除**
//（用户看到的还是"她忘了"）。所以这里除了"能收、能放"，还要钉住三条边界：
//   - core 不能收（它是"她是谁"，要取消就直说删除）；
//   - 放回来一律回 recent，不回 core（否则"放着不用"会变成身份降级）；
//   - 内置人格只读。
func runRuleTierContract(t *testing.T, s persona.Store) {
	t.Helper()

	// 每个人格都自建：归档是改状态的操作，共用一个会互相干扰
	newPersona := func(t *testing.T) string {
		t.Helper()
		name := "层级测试 " + time.Now().Format("0102-150405.000000")
		id, err := s.CreatePersona(name, "")
		if err != nil {
			t.Fatalf("新建人格失败: %v", err)
		}
		t.Cleanup(func() { _ = s.DeletePersona(id) })
		return id
	}
	saveRule := func(t *testing.T, personaID, slot, tier string) string {
		t.Helper()
		id, err := s.SaveRule(persona.PersonaRule{
			PersonaID: personaID, Slot: slot, Value: "别太正经",
			Source: persona.SourceManual, Tier: tier, Kind: persona.KindVolatile, Enabled: true,
		})
		if err != nil {
			t.Fatalf("写规则失败: %v", err)
		}
		return id
	}
	tierOf := func(t *testing.T, personaID, ruleID string) string {
		t.Helper()
		rules, err := s.RulesOf(personaID)
		if err != nil {
			t.Fatalf("读规则失败: %v", err)
		}
		for _, r := range rules {
			if r.ID == ruleID {
				return r.Tier
			}
		}
		t.Fatalf("规则 %s 不在列表里", ruleID)
		return ""
	}

	t.Run("近期层的规则能收进归档层", func(t *testing.T) {
		pid := newPersona(t)
		id := saveRule(t, pid, "tone", persona.TierRecent)

		if err := s.ArchiveRule(id); err != nil {
			t.Fatalf("收起来失败: %v", err)
		}
		if got := tierOf(t, pid, id); got != persona.TierArchived {
			t.Errorf("层级应当是 %s，实际 %s", persona.TierArchived, got)
		}

		// 变更记录里要留痕：用户得能回溯"她这条是我什么时候收起来的"
		changes, err := s.ChangesOf(pid)
		if err != nil {
			t.Fatalf("读变更记录失败: %v", err)
		}
		var found bool
		for _, c := range changes {
			if c.RuleID == id && c.Action == persona.ActionArchive {
				found = true
			}
		}
		if !found {
			t.Errorf("收起来应当记一条 %s 变更，实际 %+v", persona.ActionArchive, changes)
		}

		// 放回来：回**近期**层（不是 core），并同样留痕
		if err := s.ReviveRule(id); err != nil {
			t.Fatalf("放回来失败: %v", err)
		}
		if got := tierOf(t, pid, id); got != persona.TierRecent {
			t.Errorf("放回来应当回到 %s，实际 %s", persona.TierRecent, got)
		}
		changes, err = s.ChangesOf(pid)
		if err != nil {
			t.Fatalf("读变更记录失败: %v", err)
		}
		found = false
		for _, c := range changes {
			if c.RuleID == id && c.Action == persona.ActionRevive {
				found = true
			}
		}
		if !found {
			t.Errorf("放回来应当记一条 %s 变更，实际 %+v", persona.ActionRevive, changes)
		}
	})

	t.Run("core 层的规则不能收起来", func(t *testing.T) {
		pid := newPersona(t)
		id := saveRule(t, pid, "address_user", persona.TierCore)

		if err := s.ArchiveRule(id); err == nil {
			t.Error("core 是「她是谁」，不该被收进归档层——要取消就直说删除")
		}
		if got := tierOf(t, pid, id); got != persona.TierCore {
			t.Errorf("被拒绝之后层级不该变，实际 %s", got)
		}
	})

	t.Run("不在归档层的规则不能放回来", func(t *testing.T) {
		pid := newPersona(t)
		id := saveRule(t, pid, "tone", persona.TierRecent)

		if err := s.ReviveRule(id); err == nil {
			t.Error("它本来就在近期层，放回来应当被拒绝（否则界面上的重复点击会静默通过）")
		}
		if got := tierOf(t, pid, id); got != persona.TierRecent {
			t.Errorf("被拒绝之后层级不该变，实际 %s", got)
		}
	})

	t.Run("内置人格只读：两条都被拒", func(t *testing.T) {
		// 内置人格的 ID 形如 builtin:xxx（它们不落库，见 persona.Origin）
		builtinID := ""
		for _, p := range s.Snapshot().Personas {
			if p.IsBuiltin {
				builtinID = p.ID
				break
			}
		}
		if builtinID == "" {
			t.Skip("这个存储里没有内置人格")
		}

		rules, err := s.RulesOf(builtinID)
		if err != nil {
			t.Fatalf("读内置人格的规则失败: %v", err)
		}
		if len(rules) == 0 {
			t.Skip("这个内置人格没有规则")
		}
		if err := s.ArchiveRule(rules[0].ID); err == nil {
			t.Error("内置人格只读，收起来应当被拒绝")
		}
	})

	t.Run("降级提议的两个时间戳：只记状态，不动规则本身", func(t *testing.T) {
		pid := newPersona(t)
		id := saveRule(t, pid, "tone", persona.TierRecent)
		ruleOf := func(t *testing.T) persona.PersonaRule {
			t.Helper()
			rules, err := s.RulesOf(pid)
			if err != nil {
				t.Fatalf("读规则失败: %v", err)
			}
			for _, r := range rules {
				if r.ID == id {
					return r
				}
			}
			t.Fatal("规则不在列表里")
			return persona.PersonaRule{}
		}
		changeCount := func(t *testing.T) int {
			t.Helper()
			changes, err := s.ChangesOf(pid)
			if err != nil {
				t.Fatalf("读变更记录失败: %v", err)
			}
			n := 0
			for _, c := range changes {
				if c.RuleID == id {
					n++
				}
			}
			return n
		}

		before, changes := ruleOf(t), changeCount(t)
		if err := s.MarkDowngradeAsked(id, 1234); err != nil {
			t.Fatalf("记下「问过」失败: %v", err)
		}
		if err := s.MarkDowngradeRefused(id, 5678); err != nil {
			t.Fatalf("记下「被拒绝」失败: %v", err)
		}

		after := ruleOf(t)
		if after.DowngradeAskedAt != 1234 || after.DowngradeRefusedAt != 5678 {
			t.Errorf("两个时间戳应当被记下，实际 %d / %d", after.DowngradeAskedAt, after.DowngradeRefusedAt)
		}
		// ⚠️ UpdatedAt 不能动：它在注入排序里代表"这条规则什么时候改过"，
		// 动了它会让"她问了一句要不要收"变成"这条刚改过、要优先注入"——完全不相干
		if after.UpdatedAt != before.UpdatedAt {
			t.Errorf("记状态不该动 UpdatedAt：%d → %d", before.UpdatedAt, after.UpdatedAt)
		}
		// 变更记录是"规则内容/层级变了"的账本，这两个标记不该往里写
		if got := changeCount(t); got != changes {
			t.Errorf("这两个标记不该写变更记录：%d → %d 条", changes, got)
		}
	})
}

func TestMemoryStoreRuleTier(t *testing.T) {
	runRuleTierContract(t, newTestStore())
}

func TestPgStoreRuleTier(t *testing.T) {
	runRuleTierContract(t, openTestPgStore(t))
}
