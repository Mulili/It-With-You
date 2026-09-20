package persona

import (
	"fmt"
	"sort"
)

// SnapshotChangeLimit 是快照里带回的变更记录条数。
const SnapshotChangeLimit = 30

// Snapshot 是给前端"一次拿全"的数据（对应绑定方法 GetPersonaSnapshot）。
//
// 小浮层里做多次往返会明显卡顿，所以读走快照、写走细粒度方法（见 operation.md 契约）。
type Snapshot struct {
	Personas []Persona `json:"personas"`
	// ActiveID 是当前生效的人格 ID
	ActiveID string `json:"activeId"`
	// Rules 只含当前人格的规则，已按注入顺序排好
	Rules []PersonaRule `json:"rules"`
	// RecentChanges 按时间倒序，最多 SnapshotChangeLimit 条
	RecentChanges []PersonaChange `json:"recentChanges"`
	// StorageReady 为 false 表示存储没连上（当前用的是内置人格 + 内存数据）。
	// 前端据此提示"数据库未连接"。内存实现恒为 true。
	StorageReady bool `json:"storageReady"`
}

// Store 是人格存储的抽象。
//
// 阶段3 只有内存实现（MemoryStore），⑥ 再加 PG 实现——上层（app.go 与前端绑定）
// 只依赖这个接口，换实现不改变调用方。
type Store interface {
	// Snapshot 返回前端渲染设置页所需的全部数据
	Snapshot() Snapshot

	// RulesOf 返回**指定**人格的规则（已排序）。
	// 编辑器要能改任意人格（含非当前人格），而 Snapshot 只带当前人格的规则。
	RulesOf(personaID string) ([]PersonaRule, error)

	// ChangesOf 返回**指定**人格的最近变更记录（时间倒序，最多 SnapshotChangeLimit 条）。
	// 与 RulesOf 同理：变更记录也是"这一个个体"的私事，而编辑器要能看任意人格的。
	ChangesOf(personaID string) ([]PersonaChange, error)

	SaveSeedText(personaID, text string) error
	SetActivePersona(id string) error

	// ThinkingDisabled 报告用户是否关掉了思考模式。
	//
	// 默认 false = 跟随官方默认（DeepSeek 的思考模式默认开启）。
	// 它是**应用级**设置、不属于任何人格，所以不放进 Snapshot——那里装的是"这个人格"的数据。
	ThinkingDisabled() (bool, error)
	// SetThinkingDisabled 保存思考开关（落 app_settings；PG 连不上时随内存一起丢，与人格同命运）。
	SetThinkingDisabled(disabled bool) error

	CreatePersona(name, copyFromID string) (string, error)
	RenamePersona(id, name string) error
	DeletePersona(id string) error

	// SaveRule 新增或更新一条规则（r.ID 为空即新增），返回规则 ID
	SaveRule(r PersonaRule) (string, error)
	DeleteRule(id string) error
	SetRuleEnabled(id string, enabled bool) error

	ExportFile(id string) (PersonaFile, error)
	ImportFile(f PersonaFile) (Persona, error)
}

// tierRank 用于排序：core 在前（最稳定），archived 在后（不参与注入）
var tierRank = map[string]int{TierCore: 0, TierRecent: 1, TierArchived: 2}

// SortRules 按注入顺序排序：先 tier（core → recent → archived），
// 同 tier 内 priority 大的在前，再同则 updatedAt 新的在前。
//
// 注入链路（④）与设置页展示共用这个顺序，避免"UI 里看到的顺序"和"实际注入顺序"不一致。
func SortRules(rules []PersonaRule) {
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if ra, rb := tierRank[a.Tier], tierRank[b.Tier]; ra != rb {
			return ra < rb
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.UpdatedAt > b.UpdatedAt
	})
}

// CheckWrite 是写入权限矩阵（见 operation.md 契约）：
//
//	manual   人工在 UI 改     → 任何槽位、任何层级
//	explicit 用户明说的指令   → 任何槽位，但只能落 core / recent
//	inferred 模型自动抽取     → 只能写 volatile 槽位的 recent 层
//
// 这道闸门是防人格漂移的主要手段：身份、底线、禁忌这些 stable 维度不允许被模型自动改写。
func CheckWrite(source, slot, tier string) error {
	spec, ok := LookupSlot(slot)
	if !ok {
		return fmt.Errorf("槽位 %q 不是规范槽位（可用：%s）", slot, slotKeyList())
	}
	switch tier {
	case TierCore, TierRecent, TierArchived:
	default:
		return fmt.Errorf("层级 %q 非法（可用：%s / %s / %s）", tier, TierCore, TierRecent, TierArchived)
	}

	switch source {
	case SourceManual:
		return nil
	case SourceExplicit:
		if tier == TierArchived {
			// 归档层是"降级后的历史"，新写入不该直接进去——那是懒归档的活
			return fmt.Errorf("用户指令不能直接写入 %s 层", TierArchived)
		}
		return nil
	case SourceInferred:
		if spec.Kind != KindVolatile {
			return fmt.Errorf("槽位 %s 是 %s 维度，模型自动抽取无权改写（只能由用户明说或人工编辑）", spec.Key, KindStable)
		}
		if tier != TierRecent {
			return fmt.Errorf("模型自动抽取只能写入 %s 层，不能写 %s", TierRecent, tier)
		}
		return nil
	default:
		return fmt.Errorf("写入来源 %q 非法（可用：%s / %s / %s）", source, SourceManual, SourceExplicit, SourceInferred)
	}
}
