package builtin_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"
	"agent-for-you-love/internal/persona/store"
)

// tierRank 与根包 store.go 里 SortRules 用的排序权重一致：core → recent → archived。
// 这是本测试包自己的副本——根包那份不导出，外部测试包拿不到。
var tierRank = map[string]int{persona.TierCore: 0, persona.TierRecent: 1, persona.TierArchived: 2}

// ④ 注入链路的静态预览：把真实内置人格拼成 system 文本并打出来。
//
// 这是**不启动应用**就能检查"我写的人格会被怎么交代给模型"的地方——
// 跑 -v 直接看拼装结果，比在真机上猜"它为什么不按我写的说话"高效得多。
func TestRealBuiltinInjectionPreview(t *testing.T) {
	builtins, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}

	for _, bp := range builtins {
		system, dropped := persona.BuildSystemPrompt(bp.Persona, bp.Rules)
		if dropped != 0 {
			t.Errorf("%s：有 %d 条 recent 规则被预算截断", bp.Persona.Name, dropped)
		}
		if bp.Persona.SeedText != "" && !strings.Contains(system, bp.Persona.SeedText) {
			t.Errorf("%s：主体种子没进 system", bp.Persona.Name)
		}
		for _, r := range bp.Rules {
			if !r.Enabled || r.Tier == persona.TierArchived {
				continue
			}
			if !strings.Contains(system, r.Value) {
				t.Errorf("%s：规则 %s 的取值没进 system", bp.Persona.Name, r.Slot)
			}
		}
		t.Logf("【%s】注入 %d 字：\n%s\n", bp.Persona.Name, utf8.RuneCountInString(system), system)
	}
}

// 这个文件里的测试跑的是**真实的内置人格文件**（builtin/*.json），不是测试夹具。
//
// 它与同包的 TestBuiltinPersonasValid（builtin_test.go）的分工：
//   - 前者只管"文件格式合法"；
//   - 这里管"写好的东西真的能被当人格用起来"——装进存储、切换、取快照、规则可用，
//     并把每个人格的构成打出来（跑 -v 就能看到），顺便按注入预算自查。
//
// 用法：
//
//	go test ./internal/persona/builtin -run TestRealBuiltin -v
func TestRealBuiltinPersonasUsable(t *testing.T) {
	builtins, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}
	if len(builtins) == 0 {
		t.Fatal("没有内置人格可测")
	}

	s := store.NewMemoryStore(builtins, true)

	for _, want := range builtins {
		if err := s.SetActivePersona(want.Persona.ID); err != nil {
			t.Fatalf("切换内置人格 %s 失败: %v", want.Persona.ID, err)
		}
		snap := s.Snapshot()

		if snap.ActiveID != want.Persona.ID {
			t.Errorf("切换后 ActiveID = %q，期望 %q", snap.ActiveID, want.Persona.ID)
		}

		// 快照只返回当前人格的规则，且必须已被 SortRules 排好（core 在前）
		if len(snap.Rules) != len(want.Rules) {
			t.Errorf("%s：快照规则数 %d，期望 %d", want.Persona.Name, len(snap.Rules), len(want.Rules))
		}
		lastRank := -1
		for _, r := range snap.Rules {
			if r.PersonaID != want.Persona.ID {
				t.Errorf("%s：快照里混入了别人格的规则 %s", want.Persona.Name, r.ID)
			}
			spec, ok := persona.LookupSlot(r.Slot)
			if !ok {
				t.Errorf("%s：规则槽位 %q 不是规范槽位", want.Persona.Name, r.Slot)
				continue
			}
			if r.Kind != spec.Kind {
				t.Errorf("%s：规则 %s 的 kind(%s) 与槽位表(%s) 不一致", want.Persona.Name, r.Slot, r.Kind, spec.Kind)
			}
			if rank, ok := tierRank[r.Tier]; ok {
				if rank < lastRank {
					t.Errorf("%s：快照规则顺序不是 tier 升序（%s 出现在更靠后的层级之后）", want.Persona.Name, r.Tier)
				}
				lastRank = rank
			} else {
				t.Errorf("%s：规则 %s 的 tier = %q 非法", want.Persona.Name, r.Slot, r.Tier)
			}
		}

		// 预算自查：人格是每轮必带的固定开销，超了就必须瘦身
		used := estimateInjectBudget(want.Persona, want.Rules)
		if used > persona.InjectBudgetRunes {
			t.Errorf("%s：预计注入 %d 字，超过预算 %d 字，请精简种子文本或规则", want.Persona.Name, used, persona.InjectBudgetRunes)
		}

		// 把它的构成打出来：跑 -v 时这份输出就是"我写的人格会被怎么用"的直观检查
		t.Logf("【%s】ID=%s 种子 %d 字，规则 %d 条，预计注入 %d/%d 字",
			want.Persona.Name, want.Persona.ID,
			utf8.RuneCountInString(want.Persona.SeedText), len(want.Rules), used, persona.InjectBudgetRunes)
		for _, r := range snap.Rules {
			state := "启用"
			if !r.Enabled {
				state = "停用"
			}
			label := r.Slot
			if spec, ok := persona.LookupSlot(r.Slot); ok {
				label = spec.Label
			}
			t.Logf("    %-8s %-14s %-6s %-4s 优先%d  %s = %s",
				r.Tier, r.Slot, r.Kind, state, r.Priority, label, r.Value)
		}
	}

	// 每个人格都应该能被"复制为我的"——这是用户想改内置人格时的唯一入口
	for _, b := range builtins {
		id, err := s.CreatePersona(b.Persona.Name+" 的副本", b.Persona.ID)
		if err != nil {
			t.Fatalf("从 %s 复制失败: %v", b.Persona.Name, err)
		}
		if err := s.SaveSeedText(id, "复制后改一下看看能不能保存"); err != nil {
			t.Fatalf("复制出来的人格应当可写，实际: %v", err)
		}
	}
}

// estimateInjectBudget 估算一个人格占用多少注入预算（字符数）。
//
// 这是**估算**：真正的拼装格式在 ④ 注入链路里定（还包含固定提示语与 few-shot），
// 所以这里只算种子文本 + 会被注入的规则（启用的、非归档层的）的槽位与取值字数。
func estimateInjectBudget(p persona.Persona, rules []persona.PersonaRule) int {
	used := utf8.RuneCountInString(p.SeedText)
	for _, r := range rules {
		if !r.Enabled || r.Tier == persona.TierArchived {
			continue
		}
		used += utf8.RuneCountInString(r.Slot) + utf8.RuneCountInString(r.Value)
	}
	return used
}
