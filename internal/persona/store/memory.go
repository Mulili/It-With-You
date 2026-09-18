package store

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"

	"github.com/google/uuid"
)

// MaxChangesKept 是变更记录在内存里保留的条数，超出丢最旧的。
const MaxChangesKept = 200

// MemoryStore 是 Store 的内存实现。
//
// 它的价值不只是"临时顶替 PG"：
//   - 阶段3 先用它跑通注入链路（④）与指令链路（⑤），不被数据库拖住；
//   - ⑥ 接入 PG 后，它仍是 PG 连不上时的降级路径（配合内置人格，应用照常可用）。
//
// 内置人格是只读的，由构造时注入，不参与增删改。
type MemoryStore struct {
	mu sync.RWMutex

	// builtinOrder 保留内置人格的加载顺序，用于列表排序
	builtinOrder []string

	personas map[string]persona.Persona
	rules    map[string][]persona.PersonaRule // personaID → 规则
	changes  []persona.PersonaChange
	activeID string

	// storageReady 表示"持久化存储是否可用"。内存实现自己恒为可用，
	// 但它被当作 PG 连不上时的降级路径时，前端要据此提示"数据库未连接"。
	storageReady bool

	// now 可注入，便于测试固定时间
	now func() int64
}

// NewMemoryStore 创建内存存储。内置人格由调用方注入（一般为 builtin.Load 的结果）。
//
// storageReady 传 true 表示"这就是正经的存储"（开发期与测试）；
// 传 false 表示"这是 PG 连不上时的降级路径"，前端会提示用户。
func NewMemoryStore(builtins []builtin.Entry, storageReady bool) *MemoryStore {
	s := &MemoryStore{
		personas:     make(map[string]persona.Persona, len(builtins)),
		rules:        make(map[string][]persona.PersonaRule, len(builtins)),
		now:          func() int64 { return time.Now().UnixMilli() },
		storageReady: storageReady,
	}
	for _, b := range builtins {
		s.builtinOrder = append(s.builtinOrder, b.Persona.ID)
		s.personas[b.Persona.ID] = b.Persona
		s.rules[b.Persona.ID] = b.Rules
	}
	// 默认选中第一个内置人格：首次启动不该是"没有人格"的状态
	if len(s.builtinOrder) > 0 {
		s.activeID = s.builtinOrder[0]
	}
	return s
}

// Snapshot 实现 Store。
func (s *MemoryStore) Snapshot() persona.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	personas := make([]persona.Persona, 0, len(s.personas))
	for _, id := range s.builtinOrder {
		personas = append(personas, s.personas[id])
	}
	// 用户人格按创建时间排在后面，保证列表顺序稳定（map 遍历是随机的）
	var users []persona.Persona
	for _, p := range s.personas {
		if !p.IsBuiltin {
			users = append(users, p)
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].CreatedAt < users[j].CreatedAt })
	personas = append(personas, users...)

	// 规则要复制一份再排序：直接排内部切片会把"内部顺序"也改掉
	rules := append([]persona.PersonaRule(nil), s.rules[s.activeID]...)
	persona.SortRules(rules)

	// 变更记录也只给当前人格的：它和对话历史一样，属于"这一个个体"，
	// 把别人格的变更混进来同样是串台
	changes := make([]persona.PersonaChange, 0, persona.SnapshotChangeLimit)
	for i := len(s.changes) - 1; i >= 0 && len(changes) < persona.SnapshotChangeLimit; i-- {
		if s.changes[i].PersonaID != s.activeID {
			continue
		}
		changes = append(changes, s.changes[i])
	}

	return persona.Snapshot{
		Personas:      personas,
		ActiveID:      s.activeID,
		Rules:         rules,
		RecentChanges: changes,
		StorageReady:  s.storageReady,
	}
}

// Close 让内存实现也满足 io.Closer：上层关闭存储时不必区分实现。
func (s *MemoryStore) Close() error { return nil }

// SaveSeedText 改写主体人格文本。
func (s *MemoryStore) SaveSeedText(personaID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.writableLocked(personaID)
	if err != nil {
		return err
	}
	text, err = persona.ValidateSeedText(text, len(s.rules[p.ID]) > 0)
	if err != nil {
		return err
	}

	now := s.now()
	old := p.SeedText
	p.SeedText = text
	p.UpdatedAt = now
	s.personas[p.ID] = p
	s.logLocked(persona.PersonaChange{PersonaID: p.ID, Action: persona.ActionUpdate, Field: "seedText",
		OldValue: old, NewValue: text, Source: persona.SourceManual, CreatedAt: now})
	return nil
}

// SetActivePersona 切换当前生效的人格。
func (s *MemoryStore) SetActivePersona(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.personas[id]; !ok {
		return fmt.Errorf("人格 %s 不存在", id)
	}
	s.activeID = id
	return nil
}

// CreatePersona 新建人格（可从内置或已有的人格复制），返回新人格 ID。
func (s *MemoryStore) CreatePersona(name, copyFromID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name, err := persona.ValidateName(name)
	if err != nil {
		return "", err
	}

	now := s.now()
	id := uuid.NewString()
	p := persona.Persona{ID: id, Name: name, Origin: persona.OriginUser, CreatedAt: now, UpdatedAt: now}

	if copyFromID != "" {
		src, ok := s.personas[copyFromID]
		if !ok {
			return "", fmt.Errorf("要复制的人格 %s 不存在", copyFromID)
		}
		p.SeedText = src.SeedText
		p.AvatarPath = src.AvatarPath
		for _, r := range s.rules[copyFromID] {
			nr := r
			// 换 ID、改归属：这是一份独立副本，之后怎么改都不会影响来源
			nr.ID = uuid.NewString()
			nr.PersonaID = id
			nr.Source = persona.SourceManual
			nr.CreatedAt = now
			nr.UpdatedAt = now
			nr.Evidence = ""
			s.rules[id] = append(s.rules[id], nr)
		}
	}

	s.personas[id] = p
	return id, nil
}

// RenamePersona 改人格名。
func (s *MemoryStore) RenamePersona(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.writableLocked(id)
	if err != nil {
		return err
	}
	name, err = persona.ValidateName(name)
	if err != nil {
		return err
	}

	now := s.now()
	old := p.Name
	p.Name = name
	p.UpdatedAt = now
	s.personas[p.ID] = p
	s.logLocked(persona.PersonaChange{PersonaID: p.ID, Action: persona.ActionUpdate, Field: "name",
		OldValue: old, NewValue: name, Source: persona.SourceManual, CreatedAt: now})
	return nil
}

// DeletePersona 删除人格及其规则与变更记录。内置人格不可删。
func (s *MemoryStore) DeletePersona(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.writableLocked(id); err != nil {
		return err
	}
	delete(s.personas, id)
	delete(s.rules, id)

	// 变更记录跟着一起清掉：留着会指向一个不存在的人格，在设置页里很费解
	kept := s.changes[:0]
	for _, c := range s.changes {
		if c.PersonaID != id {
			kept = append(kept, c)
		}
	}
	s.changes = kept

	if s.activeID == id {
		// 删掉的是当前人格：回退到第一个内置人格，避免"当前人格指向不存在的东西"
		if len(s.builtinOrder) > 0 {
			s.activeID = s.builtinIDAt(0)
		} else {
			s.activeID = ""
		}
	}
	return nil
}

// SaveRule 新增或更新一条规则（ID 为空即新增），返回规则 ID。
func (s *MemoryStore) SaveRule(r persona.PersonaRule) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.writableLocked(r.PersonaID)
	if err != nil {
		return "", err
	}
	r, err = persona.NormalizeRule(r)
	if err != nil {
		return "", err
	}
	spec, _ := persona.LookupSlot(r.Slot) // persona.NormalizeRule 已保证槽位存在

	now := s.now()
	idx, exists := s.findRuleLocked(p.ID, r.ID)

	if !exists && r.ID != "" {
		return "", fmt.Errorf("规则 %s 不存在", r.ID)
	}

	// 单值槽位：再写入就是**覆盖**（契约要求同槽位不并存），所以定位到已有那条去改。
	// 不报错让调用方自己找，是因为调用方往往不知道现状——用户说"以后叫我主人"时，
	// 这条通路并不知道当前"称呼"是哪一条规则。
	replaceBySlot := false
	if !exists && !spec.Multi {
		if i := persona.FindRuleBySlot(s.rules[p.ID], r.Slot); i >= 0 {
			idx, exists, replaceBySlot = i, true, true
			r.ID = s.rules[p.ID][i].ID
		}
	}
	if !exists {
		if err := persona.CheckRuleAgainst(s.rules[p.ID], r, ""); err != nil {
			return "", err
		}
	}

	r.PersonaID = p.ID
	if exists {
		old := s.rules[p.ID][idx]
		r.ID = old.ID
		r.CreatedAt = old.CreatedAt
		r.UpdatedAt = now
		// 启停只走 SetRuleEnabled，避免更新取值时顺手把它覆盖掉……
		r.Enabled = old.Enabled
		if replaceBySlot {
			// ……但"用户重新指定了同一个槽位"算新指令，必须真的生效：
			// 否则会出现"用户说了却没反应"，而原因藏在一张被停用的旧规则里。
			r.Enabled = true
			// 层级沿用旧值：原本属于 core 的身份级信息（如称呼）不该因为改个值就降级成 recent；
			// 但若旧规则已被懒归档（阶段4），就用调用方给的层级把它复活回活跃层。
			if old.Tier != persona.TierArchived {
				r.Tier = old.Tier
			}
		}
		s.rules[p.ID][idx] = r
		s.logLocked(persona.PersonaChange{PersonaID: p.ID, RuleID: r.ID, Action: persona.ActionUpdate, Field: r.Slot,
			OldValue: old.Value, NewValue: r.Value, Source: r.Source, Evidence: r.Evidence, CreatedAt: now})
		return r.ID, nil
	}

	r.ID = uuid.NewString()
	r.Enabled = true // 新规则默认启用
	r.CreatedAt = now
	r.UpdatedAt = now
	s.rules[p.ID] = append(s.rules[p.ID], r)
	s.logLocked(persona.PersonaChange{PersonaID: p.ID, RuleID: r.ID, Action: persona.ActionCreate, Field: r.Slot,
		NewValue: r.Value, Source: r.Source, Evidence: r.Evidence, CreatedAt: now})
	return r.ID, nil
}

// DeleteRule 删除规则。
func (s *MemoryStore) DeleteRule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	personaID, idx, p, err := s.locateRuleLocked(id)
	if err != nil {
		return err
	}
	if _, err := s.writableLocked(personaID); err != nil {
		return err
	}

	removed := s.rules[personaID][idx]
	s.rules[personaID] = append(s.rules[personaID][:idx], s.rules[personaID][idx+1:]...)
	s.logLocked(persona.PersonaChange{PersonaID: p.ID, RuleID: removed.ID, Action: persona.ActionDelete, Field: removed.Slot,
		OldValue: removed.Value, Source: persona.SourceManual, CreatedAt: s.now()})
	return nil
}

// SetRuleEnabled 停用 / 启用规则。
func (s *MemoryStore) SetRuleEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	personaID, idx, p, err := s.locateRuleLocked(id)
	if err != nil {
		return err
	}
	if _, err := s.writableLocked(personaID); err != nil {
		return err
	}

	now := s.now()
	s.rules[personaID][idx].Enabled = enabled
	s.rules[personaID][idx].UpdatedAt = now

	action := persona.ActionDisable
	if enabled {
		action = persona.ActionEnable
	}
	s.logLocked(persona.PersonaChange{PersonaID: p.ID, RuleID: id, Action: action, Field: s.rules[personaID][idx].Slot,
		Source: persona.SourceManual, CreatedAt: now})
	return nil
}

// ExportFile 导出人格与规则为可分享的文件内容。
// 内置人格也允许导出——这正是"内置人格"的产出通道：调好、导出、放进 builtin/ 目录。
func (s *MemoryStore) ExportFile(id string) (persona.PersonaFile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.personas[id]
	if !ok {
		return persona.PersonaFile{}, fmt.Errorf("人格 %s 不存在", id)
	}
	rules := append([]persona.PersonaRule(nil), s.rules[id]...)
	persona.SortRules(rules)
	return persona.BuildFile(p, rules, s.now()), nil
}

// ImportFile 导入一份人格文件。
//
// 三条与契约一致的规则：**同名不覆盖（改存副本）**、规则重新生成 ID（沿用来源 ID 会撞）、
// 不带入变更记录与 evidence（那是别人的审计数据）。
func (s *MemoryStore) ImportFile(f persona.PersonaFile) (persona.Persona, error) {
	if err := f.Validate(); err != nil {
		return persona.Persona{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	id := uuid.NewString()
	p := persona.Persona{
		ID:         id,
		Name:       persona.UniqueName(s.nameSetLocked(), strings.TrimSpace(f.Persona.Name)),
		SeedText:   f.Persona.SeedText,
		Origin:     persona.OriginImported,
		AvatarPath: f.Persona.AvatarPath,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	s.personas[id] = p
	for _, r := range f.RulesFor(id) {
		r.ID = uuid.NewString()
		r.Evidence = ""
		s.rules[id] = append(s.rules[id], r)
	}
	return p, nil
}

// ---------- 内部工具（调用方需持有写锁，除注明外） ----------

// writableLocked 取一个可写的人格：必须存在、且不是内置的。
func (s *MemoryStore) writableLocked(id string) (persona.Persona, error) {
	p, ok := s.personas[id]
	if !ok {
		return persona.Persona{}, fmt.Errorf("人格 %s 不存在", id)
	}
	if p.IsBuiltin {
		return persona.Persona{}, fmt.Errorf("「%s」是内置人格，只读；请先「复制为我的」再改", p.Name)
	}
	return p, nil
}

// locateRuleLocked 按规则 ID 找到它所属的人格与下标。
func (s *MemoryStore) locateRuleLocked(ruleID string) (personaID string, idx int, p persona.Persona, err error) {
	for pid, list := range s.rules {
		for i, r := range list {
			if r.ID == ruleID {
				return pid, i, s.personas[pid], nil
			}
		}
	}
	return "", 0, persona.Persona{}, fmt.Errorf("规则 %s 不存在", ruleID)
}

func (s *MemoryStore) findRuleLocked(personaID, ruleID string) (int, bool) {
	if ruleID == "" {
		return 0, false
	}
	for i, r := range s.rules[personaID] {
		if r.ID == ruleID {
			return i, true
		}
	}
	return 0, false
}

// nameSetLocked 返回当前已用的人格名集合（导入时查重用）。
func (s *MemoryStore) nameSetLocked() map[string]bool {
	used := make(map[string]bool, len(s.personas))
	for _, p := range s.personas {
		used[p.Name] = true
	}
	return used
}

func (s *MemoryStore) builtinIDAt(i int) string {
	if i < 0 || i >= len(s.builtinOrder) {
		return ""
	}
	return s.builtinOrder[i]
}

// logLocked 追加一条变更记录，并裁掉过旧的部分。
func (s *MemoryStore) logLocked(c persona.PersonaChange) {
	c.ID = uuid.NewString()
	s.changes = append(s.changes, c)
	if len(s.changes) > MaxChangesKept {
		s.changes = append([]persona.PersonaChange(nil), s.changes[len(s.changes)-MaxChangesKept:]...)
	}
}
