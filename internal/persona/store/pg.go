package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenStore 打开人格存储：给了可用的连接池就用 PG，否则退回内存实现。
//
// 刻意不返回错误：数据库没起来不该让桌宠起不来（对齐 main.go 对"缺 API Key 只提示不 Fatal"
// 的处理姿态）。退回内存后内置人格照常可用，只是自建人格不持久——
// Snapshot.StorageReady 会告诉前端"数据库未连接"。
//
// 连接池由 main 通过 internal/db 建好再传进来：同一个库上还有会话与记忆，三者共用池。
// pool 为 nil 表示没配或连不上。
func OpenStore(pool *pgxpool.Pool, builtins []builtin.Entry) persona.Store {
	if pool == nil {
		log.Printf("[persona] 没有可用的数据库连接，人格只存在内存里（进程重启即丢）")
		return NewMemoryStore(builtins, false)
	}
	log.Printf("[persona] 数据库就绪，人格持久化已启用")
	return NewPgStore(pool, builtins)
}

// PgStore 是 Store 的 PostgreSQL 实现。
//
// 内置人格不落库（随 exe 分发、只读），所以它们仍由构造时注入、放在内存里；
// 自建人格、规则、变更记录都在 PG。写入规则（校验、缺省值、覆盖语义）与内存实现
// 共用 rules.go 里的那套函数，避免两种存储行为分叉。
type PgStore struct {
	pool        *pgxpool.Pool
	builtins    []builtin.Entry
	builtinByID map[string]builtin.Entry
}

// NewPgStore 用已有的连接池组装人格存储。表结构由 internal/db 在 Open 时保证就位。
func NewPgStore(pool *pgxpool.Pool, builtins []builtin.Entry) *PgStore {
	s := &PgStore{
		pool:        pool,
		builtins:    builtins,
		builtinByID: make(map[string]builtin.Entry, len(builtins)),
	}
	for _, b := range builtins {
		s.builtinByID[b.Persona.ID] = b
	}
	return s
}

// Close 关闭连接池（应用退出时调用）。
//
// 池是这个库上所有 store（人格 / 会话 / 记忆）共用的，但关闭只需一次，
// 而应用退出时只有这一条路径在关，所以由人格 store 代劳——
// 不为"谁该负责关闭"再引入一层所有者概念。pgxpool 的 Close 可重复调用。
func (s *PgStore) Close() error {
	s.pool.Close()
	return nil
}

// Snapshot 实现 Store。
//
// 每个子查询失败都只记日志并降级：宁可给前端一份不完整的数据，也不要让设置页直接打不开。
func (s *PgStore) Snapshot() persona.Snapshot {
	ctx, cancel := s.ctx()
	defer cancel()

	personas, err := s.listPersonas(ctx)
	if err != nil {
		log.Printf("[persona] 读取人格列表失败: %v", err)
		personas = s.builtinPersonas()
	}

	activeID, err := s.activePersonaID(ctx, personas)
	if err != nil {
		log.Printf("[persona] 读取当前人格失败: %v", err)
		activeID = firstPersonaID(personas)
	}

	rules, err := s.rulesFor(ctx, activeID)
	if err != nil {
		log.Printf("[persona] 读取规则失败: %v", err)
	}

	changes, err := s.recentChanges(ctx, activeID)
	if err != nil {
		log.Printf("[persona] 读取变更记录失败: %v", err)
	}

	return persona.Snapshot{
		Personas:      personas,
		ActiveID:      activeID,
		Rules:         rules,
		RecentChanges: changes,
		StorageReady:  true,
	}
}

// RulesOf 实现 Store：返回指定人格的规则（已排序）。
//
// 内置人格取其注入时带进来的那份（不查库）；自建人格先确认存在，避免把
// "人格 ID 打错了"和"这个人格还没规则"混成同一个空结果。
func (s *PgStore) RulesOf(personaID string) ([]persona.PersonaRule, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	if _, ok := s.builtinByID[personaID]; !ok {
		if _, err := s.loadPersona(ctx, personaID); err != nil {
			return nil, err
		}
	}
	return s.rulesOf(ctx, personaID)
}

// ChangesOf 实现 Store：返回指定人格的最近变更记录（时间倒序）。
//
// 与 RulesOf 对称：自建人格先确认存在，避免把"人格 ID 打错了"和"这个人格还没改过"
// 混成同一个空结果；内置人格跳过查库——它没有变更记录，recentChanges 直接返回空。
func (s *PgStore) ChangesOf(personaID string) ([]persona.PersonaChange, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	if _, ok := s.builtinByID[personaID]; !ok {
		if _, err := s.loadPersona(ctx, personaID); err != nil {
			return nil, err
		}
	}
	return s.recentChanges(ctx, personaID)
}

// SaveSeedText 改写主体人格文本。
func (s *PgStore) SaveSeedText(personaID, text string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	p, err := s.loadWritablePersona(ctx, personaID)
	if err != nil {
		return err
	}
	var ruleCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM persona_rules WHERE persona_id = $1::uuid`, p.ID).Scan(&ruleCount); err != nil {
		return fmt.Errorf("读取规则数量失败: %w", err)
	}
	text, err = persona.ValidateSeedText(text, ruleCount > 0)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE personas SET seed_text = $1, updated_at = $2 WHERE id = $3::uuid`,
			text, now, p.ID); err != nil {
			return err
		}
		return s.logChange(ctx, tx, persona.PersonaChange{
			PersonaID: p.ID, Action: persona.ActionUpdate, Field: "seedText",
			OldValue: p.SeedText, NewValue: text, Source: persona.SourceManual, CreatedAt: now,
		})
	})
}

// SetActivePersona 切换当前生效的人格。
func (s *PgStore) SetActivePersona(id string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	if _, ok := s.builtinByID[id]; !ok {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM personas WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("检查人格是否存在失败: %w", err)
		}
		if !exists {
			return fmt.Errorf("人格 %s 不存在", id)
		}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO app_settings (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		settingActivePersona, id)
	if err != nil {
		return fmt.Errorf("保存当前人格失败: %w", err)
	}
	return nil
}

// ThinkingDisabled 实现 Store：读 app_settings，没存过则为 false。
//
// 把"没这个 key"当成 false（而不是报错或当 true）是刻意的：官方默认就是思考开启，
// 所以首次启动不需要写库，行为也和老版本一致。
func (s *PgStore) ThinkingDisabled() (bool, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	var v string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM app_settings WHERE key = $1`, settingThinkingDisabled).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取思考开关失败: %w", err)
	}
	return v == settingTrue, nil
}

// SetThinkingDisabled 实现 Store。
func (s *PgStore) SetThinkingDisabled(disabled bool) error {
	ctx, cancel := s.ctx()
	defer cancel()

	v := settingFalse
	if disabled {
		v = settingTrue
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO app_settings (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		settingThinkingDisabled, v); err != nil {
		return fmt.Errorf("保存思考开关失败: %w", err)
	}
	return nil
}

// CreatePersona 新建人格（可从内置或已有的人格复制），返回新人格 ID。
func (s *PgStore) CreatePersona(name, copyFromID string) (string, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	name, err := persona.ValidateName(name)
	if err != nil {
		return "", err
	}

	var src persona.Persona
	var srcRules []persona.PersonaRule
	if copyFromID != "" {
		if b, ok := s.builtinByID[copyFromID]; ok {
			src, srcRules = b.Persona, b.Rules
		} else {
			if src, err = s.loadPersona(ctx, copyFromID); err != nil {
				return "", fmt.Errorf("要复制的人格不可用: %w", err)
			}
			if srcRules, err = s.rulesOf(ctx, copyFromID); err != nil {
				return "", err
			}
		}
	}

	now := time.Now().UnixMilli()
	id := uuid.NewString()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO personas (id, name, seed_text, origin, avatar_path, created_at, updated_at)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6, $6)`,
			id, name, src.SeedText, persona.OriginUser, src.AvatarPath, now); err != nil {
			return err
		}
		for _, r := range srcRules {
			// 换 ID、改归属：这是一份独立副本，之后怎么改都不会影响来源
			nr := r
			nr.ID = uuid.NewString()
			nr.PersonaID = id
			nr.Source = persona.SourceManual
			nr.Evidence = ""
			nr.CreatedAt, nr.UpdatedAt = now, now
			if err := s.insertRule(ctx, tx, nr); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// RenamePersona 改人格名。
func (s *PgStore) RenamePersona(id, name string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	p, err := s.loadWritablePersona(ctx, id)
	if err != nil {
		return err
	}
	name, err = persona.ValidateName(name)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE personas SET name = $1, updated_at = $2 WHERE id = $3::uuid`,
			name, now, p.ID); err != nil {
			return err
		}
		return s.logChange(ctx, tx, persona.PersonaChange{
			PersonaID: p.ID, Action: persona.ActionUpdate, Field: "name",
			OldValue: p.Name, NewValue: name, Source: persona.SourceManual, CreatedAt: now,
		})
	})
}

// DeletePersona 删除人格及其规则与变更记录（靠外键级联）。内置人格不在库里，删不到。
func (s *PgStore) DeletePersona(id string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	p, err := s.loadWritablePersona(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM personas WHERE id = $1::uuid`, p.ID); err != nil {
		return fmt.Errorf("删除人格失败: %w", err)
	}

	// 删掉的若是当前人格，回退到第一个内置人格——避免"当前人格指向不存在的东西"
	active, err := s.activePersonaID(ctx, s.builtinPersonas())
	if err == nil && active == p.ID {
		return s.SetActivePersona(s.firstBuiltinID())
	}
	return nil
}

// SaveRule 新增或更新一条规则（ID 为空即新增），返回规则 ID。
func (s *PgStore) SaveRule(r persona.PersonaRule) (string, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	p, err := s.loadWritablePersona(ctx, r.PersonaID)
	if err != nil {
		return "", err
	}
	r, err = persona.NormalizeRule(r)
	if err != nil {
		return "", err
	}
	spec, _ := persona.LookupSlot(r.Slot) // persona.NormalizeRule 已保证槽位存在

	existing, err := s.rulesOf(ctx, p.ID)
	if err != nil {
		return "", err
	}

	now := time.Now().UnixMilli()
	idx := -1
	replaceBySlot := false
	if r.ID != "" {
		for i, e := range existing {
			if e.ID == r.ID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return "", fmt.Errorf("规则 %s 不存在", r.ID)
		}
	} else if !spec.Multi {
		// 单值槽位：再写入就是覆盖（同槽位不并存）
		if i := persona.FindRuleBySlot(existing, r.Slot); i >= 0 {
			idx, replaceBySlot = i, true
		}
	}
	if idx < 0 {
		if err := persona.CheckRuleAgainst(existing, r, ""); err != nil {
			return "", err
		}
	}

	r.PersonaID = p.ID
	action := persona.ActionCreate
	var old persona.PersonaRule
	if idx >= 0 {
		old = existing[idx]
		action = persona.ActionUpdate
		r.ID = old.ID
		r.CreatedAt = old.CreatedAt
		r.UpdatedAt = now
		// 启停只走 SetRuleEnabled，避免更新取值时顺手覆盖……
		r.Enabled = old.Enabled
		if replaceBySlot {
			// ……但"用户重新指定了同一个槽位"算新指令，必须真的生效
			r.Enabled = true
			// 层级沿用旧值；若旧规则已被归档，就用调用方给的层级把它复活回活跃层
			if old.Tier != persona.TierArchived {
				r.Tier = old.Tier
			}
		}
	} else {
		r.ID = uuid.NewString()
		r.Enabled = true
		r.CreatedAt, r.UpdatedAt = now, now
	}

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if idx >= 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE persona_rules
				    SET value = $1, source = $2, evidence = $3, tier = $4, kind = $5,
				        priority = $6, enabled = $7, updated_at = $8
				  WHERE id = $9::uuid`,
				r.Value, r.Source, r.Evidence, r.Tier, r.Kind, r.Priority, r.Enabled, r.UpdatedAt, r.ID); err != nil {
				return err
			}
		} else if err := s.insertRule(ctx, tx, r); err != nil {
			return err
		}
		return s.logChange(ctx, tx, persona.PersonaChange{
			PersonaID: p.ID, RuleID: r.ID, Action: action, Field: r.Slot,
			OldValue: old.Value, NewValue: r.Value, Source: r.Source, Evidence: r.Evidence, CreatedAt: now,
		})
	})
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// DeleteRule 删除规则。
func (s *PgStore) DeleteRule(id string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	personaID, rule, err := s.locateRule(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.loadWritablePersona(ctx, personaID); err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM persona_rules WHERE id = $1::uuid`, id); err != nil {
			return err
		}
		return s.logChange(ctx, tx, persona.PersonaChange{
			PersonaID: personaID, RuleID: id, Action: persona.ActionDelete, Field: rule.Slot,
			OldValue: rule.Value, Source: persona.SourceManual, CreatedAt: now,
		})
	})
}

// SetRuleEnabled 停用 / 启用规则。
func (s *PgStore) SetRuleEnabled(id string, enabled bool) error {
	ctx, cancel := s.ctx()
	defer cancel()

	personaID, rule, err := s.locateRule(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.loadWritablePersona(ctx, personaID); err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	action := persona.ActionDisable
	if enabled {
		action = persona.ActionEnable
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE persona_rules SET enabled = $1, updated_at = $2 WHERE id = $3::uuid`,
			enabled, now, id); err != nil {
			return err
		}
		return s.logChange(ctx, tx, persona.PersonaChange{
			PersonaID: personaID, RuleID: id, Action: action, Field: rule.Slot,
			Source: persona.SourceManual, CreatedAt: now,
		})
	})
}

// ExportFile 导出人格与规则为可分享的文件内容（内置人格也允许导出——那是内置人格的产出通道）。
func (s *PgStore) ExportFile(id string) (persona.PersonaFile, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	if b, ok := s.builtinByID[id]; ok {
		rules := append([]persona.PersonaRule(nil), b.Rules...)
		persona.SortRules(rules)
		return persona.BuildFile(b.Persona, rules, time.Now().UnixMilli()), nil
	}
	p, err := s.loadPersona(ctx, id)
	if err != nil {
		return persona.PersonaFile{}, err
	}
	rules, err := s.rulesOf(ctx, id)
	if err != nil {
		return persona.PersonaFile{}, err
	}
	persona.SortRules(rules)
	return persona.BuildFile(p, rules, time.Now().UnixMilli()), nil
}

// ImportFile 导入一份人格文件：**同名不覆盖（改存副本）**、规则重新生成 ID、不带入变更记录与 evidence。
func (s *PgStore) ImportFile(f persona.PersonaFile) (persona.Persona, error) {
	if err := f.Validate(); err != nil {
		return persona.Persona{}, err
	}
	ctx, cancel := s.ctx()
	defer cancel()

	names, err := s.allNames(ctx)
	if err != nil {
		return persona.Persona{}, err
	}

	now := time.Now().UnixMilli()
	id := uuid.NewString()
	p := persona.Persona{
		ID:         id,
		Name:       persona.UniqueName(names, strings.TrimSpace(f.Persona.Name)),
		SeedText:   f.Persona.SeedText,
		Origin:     persona.OriginImported,
		AvatarPath: f.Persona.AvatarPath,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO personas (id, name, seed_text, origin, avatar_path, created_at, updated_at)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6, $6)`,
			p.ID, p.Name, p.SeedText, p.Origin, p.AvatarPath, now); err != nil {
			return err
		}
		for _, r := range f.RulesFor(p.ID) {
			r.ID = uuid.NewString()
			r.Evidence = "" // 不带入来源机器的审计数据
			if err := s.insertRule(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return persona.Persona{}, err
	}
	return p, nil
}

// ---------- 内部工具 ----------

// app_settings 里各设置的 key。这张表是通用的 key-value，所以值一律存成字符串。
const (
	// settingActivePersona 是"当前生效的人格"。
	settingActivePersona = "active_persona_id"
	// settingThinkingDisabled 是思考模式开关。**没有这个 key** 表示"从没设置过"，
	// 语义上等于 false（跟随官方默认：思考模式开启）。
	settingThinkingDisabled = "thinking_disabled"
)

// app_settings 里布尔值的两种写法。
const (
	settingTrue  = "true"
	settingFalse = "false"
)

func (s *PgStore) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), db.QueryTimeout)
}

// inTx 跑一个事务：出错自动回滚。
func (s *PgStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已提交时回滚是空操作

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// insertRule 插入一条规则（uuid 列显式转型，避免依赖驱动的类型推断）。
func (s *PgStore) insertRule(ctx context.Context, tx pgx.Tx, r persona.PersonaRule) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO persona_rules
		   (id, persona_id, slot, value, source, evidence, tier, kind, priority, enabled, created_at, updated_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		r.ID, r.PersonaID, r.Slot, r.Value, r.Source, r.Evidence, r.Tier, r.Kind,
		r.Priority, r.Enabled, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("写入规则失败: %w", err)
	}
	return nil
}

// logChange 追加一条变更记录。ruleID 为空时写 NULL（主体字段变更没有对应规则）。
func (s *PgStore) logChange(ctx context.Context, tx pgx.Tx, c persona.PersonaChange) error {
	var ruleID any
	if c.RuleID != "" {
		ruleID = c.RuleID
	}
	var personID any
	if c.PersonaID != "" {
		personID = c.PersonaID
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO persona_changes
		   (id, persona_id, rule_id, action, field, old_value, new_value, source, evidence, created_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10)`,
		uuid.NewString(), personID, ruleID, c.Action, c.Field, c.OldValue, c.NewValue,
		c.Source, c.Evidence, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("写入变更记录失败: %w", err)
	}
	return nil
}

// loadPersona 从库里取人格（内置人格不在库里，所以取不到即"不存在"）。
func (s *PgStore) loadPersona(ctx context.Context, id string) (persona.Persona, error) {
	var p persona.Persona
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, name, seed_text, origin, avatar_path, created_at, updated_at
		   FROM personas WHERE id = $1::uuid`, id).
		Scan(&p.ID, &p.Name, &p.SeedText, &p.Origin, &p.AvatarPath, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return persona.Persona{}, fmt.Errorf("人格 %s 不存在", id)
	}
	if err != nil {
		return persona.Persona{}, fmt.Errorf("读取人格失败: %w", err)
	}
	return p, nil
}

// loadWritablePersona 取一个可写的人格：内置只读，库里查不到就是不存在。
func (s *PgStore) loadWritablePersona(ctx context.Context, id string) (persona.Persona, error) {
	if b, ok := s.builtinByID[id]; ok {
		return persona.Persona{}, fmt.Errorf("「%s」是内置人格，只读；请先「复制为我的」再改", b.Persona.Name)
	}
	return s.loadPersona(ctx, id)
}

// rulesOf 读某人格的全部规则（内置人格取其注入时带进来的那份）。
func (s *PgStore) rulesOf(ctx context.Context, personaID string) ([]persona.PersonaRule, error) {
	if b, ok := s.builtinByID[personaID]; ok {
		out := append([]persona.PersonaRule(nil), b.Rules...)
		persona.SortRules(out)
		return out, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id::text, persona_id::text, slot, value, source, evidence, tier, kind, priority, enabled, created_at, updated_at
		   FROM persona_rules WHERE persona_id = $1::uuid ORDER BY created_at`, personaID)
	if err != nil {
		return nil, fmt.Errorf("读取规则失败: %w", err)
	}
	defer rows.Close()

	out := make([]persona.PersonaRule, 0, 8)
	for rows.Next() {
		var r persona.PersonaRule
		if err := rows.Scan(&r.ID, &r.PersonaID, &r.Slot, &r.Value, &r.Source, &r.Evidence,
			&r.Tier, &r.Kind, &r.Priority, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析规则失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取规则失败: %w", err)
	}
	persona.SortRules(out)
	return out, nil
}

// locateRule 按规则 ID 找到它所属的人格与内容。
func (s *PgStore) locateRule(ctx context.Context, ruleID string) (string, persona.PersonaRule, error) {
	var r persona.PersonaRule
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, persona_id::text, slot, value, source, evidence, tier, kind, priority, enabled, created_at, updated_at
		   FROM persona_rules WHERE id = $1::uuid`, ruleID).
		Scan(&r.ID, &r.PersonaID, &r.Slot, &r.Value, &r.Source, &r.Evidence,
			&r.Tier, &r.Kind, &r.Priority, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", persona.PersonaRule{}, fmt.Errorf("规则 %s 不存在", ruleID)
	}
	if err != nil {
		return "", persona.PersonaRule{}, fmt.Errorf("读取规则失败: %w", err)
	}
	return r.PersonaID, r, nil
}

// listPersonas 返回内置人格（按加载顺序）+ 用户人格（按创建时间）。
func (s *PgStore) listPersonas(ctx context.Context) ([]persona.Persona, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, name, seed_text, origin, avatar_path, created_at, updated_at
		   FROM personas ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("读取人格列表失败: %w", err)
	}
	defer rows.Close()

	out := s.builtinPersonas()
	for rows.Next() {
		var p persona.Persona
		if err := rows.Scan(&p.ID, &p.Name, &p.SeedText, &p.Origin, &p.AvatarPath,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析人格失败: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PgStore) builtinPersonas() []persona.Persona {
	out := make([]persona.Persona, 0, len(s.builtins))
	for _, b := range s.builtins {
		out = append(out, b.Persona)
	}
	return out
}

// activePersonaID 读当前人格；没存过或指向已不存在的人格时，回退到第一个内置人格并写回库。
func (s *PgStore) activePersonaID(ctx context.Context, personas []persona.Persona) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, settingActivePersona).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("读取当前人格失败: %w", err)
	}

	for _, p := range personas {
		if p.ID == id {
			return id, nil
		}
	}

	fallback := firstPersonaID(personas)
	if fallback == "" {
		return "", nil
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO app_settings (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		settingActivePersona, fallback); err != nil {
		log.Printf("[persona] 写回当前人格失败: %v", err)
	}
	return fallback, nil
}

// recentChanges 读某人格最近的变更记录（按时间倒序）。
//
// 内置人格随 exe 分发、不落库，所以它没有变更记录——这是"空"，不是"错误"。
// 这个判断必须在**这里**做（与 rulesOf 同一套路）：内置人格的 ID 形如 "builtin:lapwing"，
// 直接拿去比 `persona_id = $1::uuid` 会被 PG 拒绝（22P02 无效的 uuid 输入语法）。
//
// 早先只在 ChangesOf 里判断，Snapshot 那条路径漏了——而 Snapshot 读的是**当前人格**，
// 一启动就可能是内置人格，于是每次打开菜单都在日志里刷 22P02（表现为变更记录静默为空）。
func (s *PgStore) recentChanges(ctx context.Context, personaID string) ([]persona.PersonaChange, error) {
	if personaID == "" {
		return nil, nil
	}
	if _, ok := s.builtinByID[personaID]; ok {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, persona_id::text, COALESCE(rule_id::text, ''), action, field,
		        old_value, new_value, source, evidence, created_at
		   FROM persona_changes WHERE persona_id = $1::uuid
		  ORDER BY created_at DESC LIMIT $2`, personaID, persona.SnapshotChangeLimit)
	if err != nil {
		return nil, fmt.Errorf("读取变更记录失败: %w", err)
	}
	defer rows.Close()

	out := make([]persona.PersonaChange, 0, persona.SnapshotChangeLimit)
	for rows.Next() {
		var c persona.PersonaChange
		if err := rows.Scan(&c.ID, &c.PersonaID, &c.RuleID, &c.Action, &c.Field,
			&c.OldValue, &c.NewValue, &c.Source, &c.Evidence, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析变更记录失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// rulesFor 取某人格用于注入的规则（内置与用户人格都能处理）。
func (s *PgStore) rulesFor(ctx context.Context, personaID string) ([]persona.PersonaRule, error) {
	if personaID == "" {
		return nil, nil
	}
	return s.rulesOf(ctx, personaID)
}

// allNames 返回所有已用的人格名（含内置），供导入时查重。
func (s *PgStore) allNames(ctx context.Context) (map[string]bool, error) {
	names := make(map[string]bool, len(s.builtins)+4)
	for _, b := range s.builtins {
		names[b.Persona.Name] = true
	}
	rows, err := s.pool.Query(ctx, `SELECT name FROM personas`)
	if err != nil {
		return nil, fmt.Errorf("读取人格名失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("解析人格名失败: %w", err)
		}
		names[name] = true
	}
	return names, rows.Err()
}

func (s *PgStore) firstBuiltinID() string {
	if len(s.builtins) == 0 {
		return ""
	}
	return s.builtins[0].Persona.ID
}

// firstPersonaID 取列表里第一个人的 ID（内置排在最前，所以这就是"默认人格"）。
func firstPersonaID(personas []persona.Persona) string {
	if len(personas) == 0 {
		return ""
	}
	return personas[0].ID
}
