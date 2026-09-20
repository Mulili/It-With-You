package persona

import (
	"encoding/json"
	"strings"
	"testing"
)

// 槽位收敛是"写入前收敛"的核心：近义说法必须落到同一个槽位，否则同槽位覆盖会失效。
func TestCanonicalizeSlot(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// 规范 key 原样
		{"address_user", "address_user"},
		{"tone", "tone"},
		{"other", "other"},
		// 大小写与分隔符归一
		{"Address_User", "address_user"},
		{"address user", "address_user"},
		{"  TONE  ", "tone"},
		// 中文近义说法（现在写在 SlotSpec.Aliases 里）
		{"称呼", "address_user"},
		{"对我的称呼", "address_user"},
		{"如何称呼我", "address_user"},
		{"自称", "address_self"},
		{"说话语气", "tone"},
		{"口吻", "tone"},
		{"详细程度", "verbosity"},
		{"人设", "personality"},
		{"雷区", "taboo"},
		{"口头禅", "catchphrase"},
		// 槽位的 Label 也应能收敛回自己（模型/用户很可能直接用标签称呼它）
		{"称呼用户", "address_user"},
		{"性格倾向", "personality"},
		{"回答长度", "verbosity"},
		{"未归并", SlotOther},
		// 英文同义词
		{"address", "address_user"},
		{"unknown", SlotOther},
		// 认不出来 → 落 other，而不是硬塞进某个槽位（错并会覆盖掉正确的值）
		{"风格", SlotOther},
		{"气质", SlotOther},
		{"", SlotOther},
		{"完全不认识的东西", SlotOther},
	}
	for _, c := range cases {
		if got := CanonicalizeSlot(c.in); got != c.want {
			t.Errorf("CanonicalizeSlot(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 槽位表的数据自洽性：这些是加槽位/加别名时最容易犯的错，用测试钉住。
func TestSlotDataInvariants(t *testing.T) {
	specs := SlotSpecs()
	if len(specs) == 0 {
		t.Fatal("槽位表是空的")
	}

	// 归一化后的说法 → 声明它的槽位，用于查冲突
	claimed := make(map[string]string)
	hasOther := false

	for _, s := range specs {
		if s.Key == "" || s.Label == "" {
			t.Errorf("槽位 %+v 的 Key 或 Label 为空", s)
		}
		if strings.TrimSpace(s.Desc) == "" {
			t.Errorf("槽位 %s 缺少 Desc：抽取提示词与 UI 提示都靠它说明「这个槽位装什么」", s.Key)
		}
		if s.Kind != KindStable && s.Kind != KindVolatile {
			t.Errorf("槽位 %s 的 kind = %q 非法", s.Key, s.Kind)
		}
		if s.Key == SlotOther {
			hasOther = true
			if !s.Multi {
				t.Errorf("兜底槽位 %s 应当是多值，否则第二条未归并规则会覆盖第一条", SlotOther)
			}
		}

		// Key 与 Label 必须能收敛回自己
		for _, name := range []string{s.Key, s.Label} {
			if got := CanonicalizeSlot(name); got != s.Key {
				t.Errorf("CanonicalizeSlot(%q) = %q，期望 %q", name, got, s.Key)
			}
		}

		names := append([]string{s.Key, s.Label}, s.Aliases...)
		for _, n := range names {
			key := normalizeSlot(n)
			if key == "" {
				t.Errorf("槽位 %s 有一个空白别名", s.Key)
				continue
			}
			if prev, dup := claimed[key]; dup {
				t.Errorf("说法 %q 同时被 %s 与 %s 声明——同一说法只能属于一个槽位，否则归并结果取决于遍历顺序", n, prev, s.Key)
				continue
			}
			claimed[key] = s.Key
		}
	}

	if !hasOther {
		t.Errorf("槽位表里缺少兜底槽位 %s", SlotOther)
	}
}

// 校验必须能挡住手写文件里的典型错误，并且报错要指出位置。
func TestValidateRejects(t *testing.T) {
	ok := func() PersonaFile {
		return PersonaFile{
			SchemaVersion: FileSchemaVersion,
			Persona:       PersonaFileHead{Name: "测试人格", SeedText: "你是测试人格。"},
			Rules: []PersonaFileRule{
				{Slot: "address_user", Value: "你"},
			},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*PersonaFile)
		wantMsg string
	}{
		{"版本不支持", func(f *PersonaFile) { f.SchemaVersion = 99 }, "schemaVersion"},
		{"名称为空", func(f *PersonaFile) { f.Persona.Name = "  " }, "persona.name"},
		{"种子与规则同时为空", func(f *PersonaFile) {
			f.Persona.SeedText = ""
			f.Rules = nil
		}, "不能同时为空"},
		{"槽位不认识", func(f *PersonaFile) { f.Rules[0].Slot = "mood" }, "不是规范槽位"},
		{"kind 与槽位规定不符", func(f *PersonaFile) { f.Rules[0].Kind = KindStable }, "不一致"},
		{"tier 非法", func(f *PersonaFile) { f.Rules[0].Tier = "longterm" }, "tier"},
		{"source 非法", func(f *PersonaFile) { f.Rules[0].Source = "guessed" }, "source"},
		{"值超长", func(f *PersonaFile) {
			f.Rules[0].Value = strings.Repeat("字", MaxRuleValueRunes+1)
		}, "超过上限"},
		{"单值槽位重复", func(f *PersonaFile) {
			f.Rules = append(f.Rules, PersonaFileRule{Slot: "address_user", Value: "主人"})
		}, "只能有一条"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := ok()
			c.mutate(&f)
			err := f.Validate()
			if err == nil {
				t.Fatalf("期望报错，但校验通过了")
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Fatalf("报错信息里没有 %q：%v", c.wantMsg, err)
			}
			t.Logf("按预期拒绝: %v", err)
		})
	}
}

// 缺省值补全：作者少写字段应该得到合理默认，而不是静默变成"停用"或空 tier。
func TestRulesForDefaults(t *testing.T) {
	f := PersonaFile{
		SchemaVersion: FileSchemaVersion,
		Persona:       PersonaFileHead{Name: "默认值测试", SeedText: "……"},
		Rules: []PersonaFileRule{
			{Slot: "称呼", Value: "主人"},                               // 别名 + 全缺省
			{Slot: "taboo", Value: "别提工作", Tier: TierRecent},        // 显式 tier
			{Slot: "taboo", Value: "别说脏话", Enabled: boolPtr(false)}, // 显式停用
		},
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("这份文件应当是合法的: %v", err)
	}

	rules := f.RulesFor("builtin:test")
	if len(rules) != 3 {
		t.Fatalf("规则数 = %d，期望 3", len(rules))
	}

	first := rules[0]
	if first.Slot != "address_user" {
		t.Errorf("别名没有收敛到规范槽位: %q", first.Slot)
	}
	if first.Tier != TierCore {
		t.Errorf("tier 缺省应为 %s，实际 %q", TierCore, first.Tier)
	}
	if first.Kind != KindVolatile {
		t.Errorf("kind 应沿用槽位表（address_user = volatile），实际 %q", first.Kind)
	}
	if first.Source != SourceManual {
		t.Errorf("source 缺省应为 %s，实际 %q", SourceManual, first.Source)
	}
	if !first.Enabled {
		t.Error("enabled 缺省应为 true——漏填字段不该静默变成停用")
	}
	if rules[1].Tier != TierRecent {
		t.Errorf("显式 tier 被覆盖了: %q", rules[1].Tier)
	}
	if rules[2].Enabled {
		t.Error("显式 enabled:false 没有生效")
	}
	if rules[0].PersonaID != "builtin:test" || rules[0].ID == "" {
		t.Errorf("规则没有正确归属人格: %+v", rules[0])
	}
}

// 严格解析：完全不认识的字段名必须报错（例如把 seedText 写成 seed_text）。
//
// 但有个 encoding/json 的既有限制要记住：它匹配字段名时**大小写不敏感**，
// 所以 "seedtext" 这种只错大小写的写法会被静默接受、不会报错。
// 结论：这份文件仍要照模板的字段名写，严格模式只拦得住"完全对不上"的字段。
func TestParsePersonaFileStrict(t *testing.T) {
	_, err := ParsePersonaFile([]byte(`{"schemaVersion":1,"persona":{"name":"x","seed_text":"字段名完全不对"}}`))
	if err == nil {
		t.Fatal("字段名完全对不上（seed_text）应当报错")
	}
	t.Logf("按预期拒绝: %v", err)

	// 把上面那条限制固定成断言，免得以后有人以为严格模式能抓住大小写笔误
	var f PersonaFile
	if err := json.Unmarshal([]byte(`{"schemaVersion":1,"persona":{"name":"x","seedtext":"仅大小写不同"}}`), &f); err != nil {
		t.Fatalf("encoding/json 对 key 大小写不敏感，这里不该报错: %v", err)
	}
	if f.Persona.SeedText != "仅大小写不同" {
		t.Fatalf("大小写不同的 key 会被匹配上，实际 seedText = %q", f.Persona.SeedText)
	}
}

// 给前端的结构体必须带全 json tag：漏了就会被序列化成 Go 字段名（Key / Label / Multi），
// 而前端读的是小写——症状是"槽位下拉一片空白"，而且不报任何错，极难定位。
// 这条把"给前端的字段名契约"钉住：名字是小写，前端才读得到。
func TestSlotSpecJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(MetaInfo())
	if err != nil {
		t.Fatalf("序列化 Meta 失败: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("反序列化 Meta 失败: %v", err)
	}

	for _, k := range []string{"slots", "seedTextRunes", "ruleValueRunes", "injectBudgetRunes"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("Meta 缺字段 %q —— 前端读的就是这个名字", k)
		}
	}

	slots, ok := raw["slots"].([]any)
	if !ok || len(slots) == 0 {
		t.Fatalf("Meta.slots 应当是非空数组，实际 %T", raw["slots"])
	}
	first, ok := slots[0].(map[string]any)
	if !ok {
		t.Fatalf("slots[0] 不是对象：%T", slots[0])
	}
	for _, k := range []string{"key", "label", "desc", "aliases", "kind", "multi"} {
		if _, ok := first[k]; !ok {
			t.Errorf("SlotSpec 缺字段 %q —— 前端 s.%s 只能读到 undefined", k, k)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
