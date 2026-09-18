package persona

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 这个文件放"与存储无关的写入规则"：长度校验、缺省值、槽位一致性、名称唯一化。
//
// 内存实现与 PG 实现都调它，保证两种存储的行为逐字一致——否则同一个操作
// 在开发环境（内存）与生产（PG）会表现不同，这类差异极难排查。

// NormalizeRule 补全缺省值并校验一条规则的合法性（只校验自身，不涉及与其它规则的关系）。
//
// 缺省：tier → core、kind → 沿用槽位表、source → manual。
func NormalizeRule(r PersonaRule) (PersonaRule, error) {
	spec, ok := LookupSlot(r.Slot)
	if !ok {
		return PersonaRule{}, fmt.Errorf("槽位 %q 不是规范槽位（可用：%s）", r.Slot, slotKeyList())
	}

	r.Value = strings.TrimSpace(r.Value)
	if r.Value == "" {
		return PersonaRule{}, fmt.Errorf("规则取值不能为空")
	}
	if n := utf8.RuneCountInString(r.Value); n > MaxRuleValueRunes {
		return PersonaRule{}, fmt.Errorf("规则取值长度 %d 超过上限 %d", n, MaxRuleValueRunes)
	}
	if n := utf8.RuneCountInString(r.Evidence); n > MaxEvidenceRunes {
		return PersonaRule{}, fmt.Errorf("依据长度 %d 超过上限 %d", n, MaxEvidenceRunes)
	}

	if r.Tier == "" {
		r.Tier = TierCore
	}
	if r.Kind == "" {
		r.Kind = spec.Kind
	}
	if r.Source == "" {
		r.Source = SourceManual
	}
	if r.Kind != spec.Kind {
		return PersonaRule{}, fmt.Errorf("规则 kind = %q 与槽位 %s 的规定（%s）不一致；若确实想改这个维度，应换槽位", r.Kind, spec.Key, spec.Kind)
	}
	if err := CheckWrite(r.Source, r.Slot, r.Tier); err != nil {
		return PersonaRule{}, err
	}
	return r, nil
}

// CheckRuleAgainst 校验一条规则与既有规则集合的一致性。
//
// 只处理"多值槽位的重复取值"：单值槽位的"覆盖"由调用方处理——
// 内存实现是原地替换，PG 实现是 UPDATE 同一行，两者的外部表现必须一致。
func CheckRuleAgainst(rules []PersonaRule, r PersonaRule, excludeID string) error {
	spec, ok := LookupSlot(r.Slot)
	if !ok || !spec.Multi {
		return nil
	}
	for _, other := range rules {
		if other.ID == excludeID || other.Slot != r.Slot {
			continue
		}
		if other.Value == r.Value {
			return fmt.Errorf("槽位 %s 已有相同取值「%s」", spec.Key, r.Value)
		}
	}
	return nil
}

// FindRuleBySlot 返回某槽位既有规则的下标；没有则 -1。
// 单值槽位靠它实现"再写入即覆盖"。
func FindRuleBySlot(rules []PersonaRule, slot string) int {
	for i, r := range rules {
		if r.Slot == slot {
			return i
		}
	}
	return -1
}

// ValidateName 清洗并校验人格名。
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("人格名不能为空")
	}
	if n := utf8.RuneCountInString(name); n > MaxNameRunes {
		return "", fmt.Errorf("人格名长度 %d 超过上限 %d", n, MaxNameRunes)
	}
	return name, nil
}

// ValidateSeedText 校验主体人格文本。hasRules 为 false 时不允许为空，
// 否则这个人格既没有说话方式也没有行为准则。
func ValidateSeedText(text string, hasRules bool) (string, error) {
	if n := utf8.RuneCountInString(text); n > MaxSeedTextRunes {
		return "", fmt.Errorf("主体人格长度 %d 超过上限 %d", n, MaxSeedTextRunes)
	}
	if strings.TrimSpace(text) == "" && !hasRules {
		return "", fmt.Errorf("主体人格与规则不能同时为空，那样这个人格什么都不会说")
	}
	return text, nil
}

// UniqueName 在已有名字集合里给出一个不冲突的名字：重名时加" (2)"、" (3)"后缀。
// 导入用的就是它——同名不覆盖，改存副本。
func UniqueName(existing map[string]bool, name string) string {
	if !existing[name] {
		return name
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)", name, i)
		if !existing[candidate] {
			return candidate
		}
	}
	return fmt.Sprintf("%s (%d)", name, len(existing)+1)
}
