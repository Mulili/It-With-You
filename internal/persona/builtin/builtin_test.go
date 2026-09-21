package builtin_test

import (
	"testing"

	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"

	"github.com/google/uuid"
)

// 内置人格是随 exe 分发的数据，坏文件必须在开发期就被拦住。
// 这个测试是它的守门人：新增/修改 builtin/*.json 后跑一次即可。
func TestBuiltinPersonasValid(t *testing.T) {
	list, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("没有加载到任何内置人格（builtin/ 目录下至少要有一个 .json）")
	}

	names := make(map[string]bool, len(list))
	ids := make(map[string]bool, len(list))
	for _, bp := range list {
		// id 必须是合法 uuid 且唯一：它在 personas 表里当外键锚点，
		// 改动一次就会让已存在的历史与记忆变成孤儿
		if _, err := uuid.Parse(bp.Persona.ID); err != nil {
			t.Errorf("内置人格 ID 必须是合法 uuid，实际是 %q：%v", bp.Persona.ID, err)
		}
		if ids[bp.Persona.ID] {
			t.Errorf("内置人格 ID 重复: %s", bp.Persona.ID)
		}
		ids[bp.Persona.ID] = true

		if !bp.Persona.IsBuiltin || bp.Persona.Origin != persona.OriginBuiltin {
			t.Errorf("%s 的 IsBuiltin/Origin 没被正确设置: %+v", bp.Persona.ID, bp.Persona)
		}
		if names[bp.Persona.Name] {
			t.Errorf("人格名重复: %s", bp.Persona.Name)
		}
		names[bp.Persona.Name] = true

		if len(bp.Rules) == 0 {
			t.Errorf("%s 没有任何规则", bp.Persona.Name)
		}
		for _, r := range bp.Rules {
			if r.PersonaID != bp.Persona.ID {
				t.Errorf("规则 %s 的 PersonaID 没指向所属人格", r.ID)
			}
			if _, ok := persona.LookupSlot(r.Slot); !ok {
				t.Errorf("规则 %s 的槽位 %q 不是规范槽位", r.ID, r.Slot)
			}
			if !r.Enabled {
				t.Errorf("规则 %s 不应默认停用", r.ID)
			}
		}
	}
	t.Logf("共加载 %d 个内置人格：%v", len(list), mapKeys(names))
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
