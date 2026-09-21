package persona

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// FileSchemaVersion 是当前支持的人格文件格式版本。
// 以后格式不兼容地演进时递增它，解析时据此给出明确提示，而不是让旧文件静默出错。
const FileSchemaVersion = 1

// PersonaFile 是导入导出与内置人格共用的文件格式。
//
// 三处共用同一份格式是刻意的：内置人格因此有了顺手的产出通道——
// 在 app 里把人格调好、导出、把文件放进 internal/persona/builtin/ 即可。
type PersonaFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	ExportedAt    int64             `json:"exportedAt,omitempty"`
	Persona       PersonaFileHead   `json:"persona"`
	Rules         []PersonaFileRule `json:"rules"`
}

// PersonaFileHead 是文件里的人格头，只带"可携带"的字段。
//
// 不含时间戳：导入方会重新生成。id 则是一个**有例外的例外**——
// 导出文件刻意不带 id（沿用来源 ID 会与已有数据撞 ID），所以 BuildFile 不填它；
// 而内置人格**必须**写死它，理由见 ID 字段的注释。
type PersonaFileHead struct {
	// ID 只在内置人格文件里出现：导出时不写，导入时忽略（导入方生成新 id）。
	//
	// 内置人格为什么必须有固定 id：它在 personas 表里要占一行（sessions / messages /
	// memories 的 persona_id 有外键约束，而用户完全可能一直用内置人格聊），
	// 而这一行需要一个**跨重启、跨版本都不变**的 uuid。
	// 从文件名推 id 做不到这点——改个文件名就会让已存在的历史变成孤儿。
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	SeedText   string `json:"seedText"`
	Origin     string `json:"origin,omitempty"`
	AvatarPath string `json:"avatarPath,omitempty"`
}

// PersonaFileRule 是文件里的一条规则。
//
// 与 PersonaRule 分开定义，有两个原因：
//   - 文件里不该出现 id / personaId / 时间戳（由导入方生成）；
//   - Enabled 需要"缺省即启用"的语义，所以用 *bool。若直接用 bool，手写 JSON 时
//     漏填 enabled 会静默变成"停用"——规则看起来加载成功却永不生效，极难排查。
type PersonaFileRule struct {
	Slot     string `json:"slot"`
	Value    string `json:"value"`
	Source   string `json:"source,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// ParsePersonaFile 解析一份人格文件。
//
// 用严格模式（不认识的字段直接报错）：这份 JSON 主要是手写的，
// 把 seedText 写成 seed_text 这类笔误必须立刻暴露，不能静默当成空值。
func ParsePersonaFile(data []byte) (PersonaFile, error) {
	var f PersonaFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return PersonaFile{}, fmt.Errorf("解析人格文件失败: %w", err)
	}
	return f, nil
}

// Validate 校验人格文件的内容。
//
// 错误信息要能直接指导修改——写这份 JSON 的是人，报错应当指出"哪个字段、错在哪、可选值是什么"。
func (f PersonaFile) Validate() error {
	if f.SchemaVersion != FileSchemaVersion {
		return fmt.Errorf("schemaVersion = %d，本版本只支持 %d", f.SchemaVersion, FileSchemaVersion)
	}
	if strings.TrimSpace(f.Persona.Name) == "" {
		return fmt.Errorf("persona.name 不能为空")
	}
	if n := utf8.RuneCountInString(f.Persona.Name); n > MaxNameRunes {
		return fmt.Errorf("persona.name 长度 %d 超过上限 %d", n, MaxNameRunes)
	}
	if n := utf8.RuneCountInString(f.Persona.SeedText); n > MaxSeedTextRunes {
		return fmt.Errorf("persona.seedText 长度 %d 超过上限 %d（提示词预算不允许更大的种子文本）", n, MaxSeedTextRunes)
	}
	if strings.TrimSpace(f.Persona.SeedText) == "" && len(f.Rules) == 0 {
		return fmt.Errorf("persona.seedText 与 rules 不能同时为空，那样这个人格什么都不会说")
	}

	singleValueSeen := make(map[string]int, len(f.Rules)) // 槽位 → 第几条规则
	for i, r := range f.Rules {
		where := fmt.Sprintf("rules[%d]", i)

		// 槽位：接受规范 key，也接受别名（会归一到规范 key）；都不认识就报错。
		// 这里刻意不落 other——other 是给模型输出兜底用的，手写文件里的错别字应该报错。
		key := CanonicalizeSlot(r.Slot)
		if key == SlotOther && !strings.EqualFold(strings.TrimSpace(r.Slot), SlotOther) {
			return fmt.Errorf("%s.slot = %q 不是规范槽位（可用：%s）", where, r.Slot, slotKeyList())
		}
		spec, ok := LookupSlot(key)
		if !ok {
			return fmt.Errorf("%s.slot = %q 不是规范槽位（可用：%s）", where, r.Slot, slotKeyList())
		}

		if strings.TrimSpace(r.Value) == "" {
			return fmt.Errorf("%s.value 不能为空", where)
		}
		if n := utf8.RuneCountInString(r.Value); n > MaxRuleValueRunes {
			return fmt.Errorf("%s.value 长度 %d 超过上限 %d", where, n, MaxRuleValueRunes)
		}
		if n := utf8.RuneCountInString(r.Evidence); n > MaxEvidenceRunes {
			return fmt.Errorf("%s.evidence 长度 %d 超过上限 %d", where, n, MaxEvidenceRunes)
		}

		if r.Tier != "" && r.Tier != TierCore && r.Tier != TierRecent && r.Tier != TierArchived {
			return fmt.Errorf("%s.tier = %q 非法（可用：%s / %s / %s）", where, r.Tier, TierCore, TierRecent, TierArchived)
		}
		// kind 与槽位表不一致通常意味着"槽位选错了"，这比 kind 本身写错更值得报出来
		if r.Kind != "" && r.Kind != spec.Kind {
			return fmt.Errorf("%s.kind = %q 与槽位 %s 的规定（%s）不一致；若确实想改这个维度，应换槽位", where, r.Kind, spec.Key, spec.Kind)
		}
		if r.Source != "" && r.Source != SourceExplicit && r.Source != SourceInferred && r.Source != SourceManual {
			return fmt.Errorf("%s.source = %q 非法（可用：%s / %s / %s）", where, r.Source, SourceExplicit, SourceInferred, SourceManual)
		}

		if !spec.Multi {
			if prev, dup := singleValueSeen[spec.Key]; dup {
				return fmt.Errorf("%s 与 rules[%d] 都写到单值槽位 %s；单值槽位只能有一条（多值槽位才允许追加）", where, prev, spec.Key)
			}
			singleValueSeen[spec.Key] = i
		}
	}
	return nil
}

// RulesFor 把文件里的规则转成内部结构，并补全缺省值、生成 ID 与归属人格。
//
// 缺省规则：tier 缺省为 core（内置人格里的规则都是作者精挑的，默认长期生效）、
// kind 沿用槽位表、source 缺省 manual、enabled 缺省 true。
func (f PersonaFile) RulesFor(personaID string) []PersonaRule {
	now := time.Now().UnixMilli()
	out := make([]PersonaRule, 0, len(f.Rules))
	for i, r := range f.Rules {
		slot := CanonicalizeSlot(r.Slot)
		spec, _ := LookupSlot(slot)

		tier := r.Tier
		if tier == "" {
			tier = TierCore
		}
		kind := r.Kind
		if kind == "" {
			kind = spec.Kind
		}
		source := r.Source
		if source == "" {
			source = SourceManual
		}
		enabled := true
		if r.Enabled != nil {
			enabled = *r.Enabled
		}

		out = append(out, PersonaRule{
			// 文件里没有 id，按序号生成；调用方（导入）会再换成新的 uuid
			ID:        fmt.Sprintf("%s#%d", personaID, i),
			PersonaID: personaID,
			Slot:      slot,
			Value:     r.Value,
			Source:    source,
			Evidence:  r.Evidence,
			Tier:      tier,
			Kind:      kind,
			Priority:  r.Priority,
			Enabled:   enabled,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	return out
}

// BuildFile 把人格与规则组装成可导出的文件内容。
//
// 刻意丢掉三类东西：id 与时间戳（导入方会重新生成）、evidence（用户原话，属敏感内容，
// 不该跟着分享文件流出去）。被停用的规则仍会导出（Enabled 显式写出），
// 这样"导出—导入"能完整还原人格，而不是悄悄丢掉几条。
func BuildFile(p Persona, rules []PersonaRule, exportedAt int64) PersonaFile {
	f := PersonaFile{
		SchemaVersion: FileSchemaVersion,
		ExportedAt:    exportedAt,
		Persona: PersonaFileHead{
			Name:       p.Name,
			SeedText:   p.SeedText,
			AvatarPath: p.AvatarPath,
		},
		Rules: make([]PersonaFileRule, 0, len(rules)),
	}
	if !p.IsBuiltin {
		// builtin 由加载器强制设置，文件里不必带；用户人格带上来源便于溯源
		f.Persona.Origin = p.Origin
	}
	for _, r := range rules {
		enabled := r.Enabled
		f.Rules = append(f.Rules, PersonaFileRule{
			Slot:     r.Slot,
			Value:    r.Value,
			Source:   r.Source,
			Tier:     r.Tier,
			Kind:     r.Kind,
			Priority: r.Priority,
			Enabled:  &enabled,
		})
	}
	return f
}
