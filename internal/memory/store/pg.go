package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/memory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore 把记忆存在 PostgreSQL 里（真源）。
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore 用已有的连接池组装记忆存储。表结构由 internal/db 在 Open 时保证就位。
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Open 返回可用的记忆存储：给了连接池就用 PG，否则退回内存实现。
//
// 与 persona / history 同一姿态：连不上不该让应用起不来。
// 注意"存储可用"与"嵌入可用"是两件事——后者不可用时，调用方不会走到这里（不抽记忆）。
func Open(pool *pgxpool.Pool) memory.Store {
	if pool == nil {
		log.Printf("[memory] 没有可用的数据库连接，记忆只存在内存里（进程重启即丢）")
		return NewMemoryStore()
	}
	return NewPgStore(pool)
}

func (s *PgStore) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), db.QueryTimeout)
}

// Save 实现 memory.Store。
func (s *PgStore) Save(m memory.Memory, vec []float32, threshold float64) (memory.SaveResult, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	now := memory.NowMillis()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.Importance == 0 {
		m.Importance = memory.DefaultImportance
	}
	vecLit := vectorLiteral(vec)

	// 去重：在该人格可见的范围里取最相似的一条，<=> 是余弦距离，HNSW 索引直接服务这个查询。
	// Kind 条件必须带上——相似度高但类型不同（"喜欢猫" vs "养过猫"）不该合并。
	var (
		dupID  string
		dupSim float64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, 1 - (embedding <=> $1::vector)
		FROM memories
		WHERE kind = $2 AND (persona_id IS NULL OR persona_id = $3::uuid)
		ORDER BY embedding <=> $1::vector
		LIMIT 1`, vecLit, m.Kind, nullIfEmpty(m.PersonaID)).Scan(&dupID, &dupSim)
	switch {
	case err == nil && dupSim >= threshold:
		if _, err := s.pool.Exec(ctx, `
			UPDATE memories
			   SET content = $2, evidence = $3,
			       importance = GREATEST(importance, $4),
			       follow_up_at = COALESCE($5, follow_up_at),
			       embedding = $6::vector, updated_at = $7
			 WHERE id = $1`,
			dupID, m.Content, m.Evidence, m.Importance,
			nullIfZero(m.FollowUpAt), vecLit, now); err != nil {
			return memory.SaveResult{}, fmt.Errorf("更新记忆失败: %w", err)
		}
		return memory.SaveResult{ID: dupID, Updated: true}, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return memory.SaveResult{}, fmt.Errorf("查询重复记忆失败: %w", err)
	}

	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memories (id, persona_id, content, kind, importance, embedding, evidence,
		                      follow_up_at, last_recalled_at, created_at, updated_at)
		VALUES ($1, $2::uuid, $3, $4, $5, $6::vector, $7, $8, NULL, $9, $9)`,
		m.ID, nullIfEmpty(m.PersonaID), m.Content, m.Kind, m.Importance, vecLit,
		m.Evidence, nullIfZero(m.FollowUpAt), m.CreatedAt); err != nil {
		return memory.SaveResult{}, fmt.Errorf("写入记忆失败: %w", err)
	}
	return memory.SaveResult{ID: m.ID}, nil
}

// memoryColumns 是读记忆的列清单，与各处的 Scan 顺序一一对应。
//
// 可空列用 COALESCE 兜成零值：让 NULL 透到 Go 会逼每个调用点都处理一遍可空性，
// 不如在 SQL 边界上收敛掉（与 history 那边同一套路）。
const memoryColumns = `id, COALESCE(persona_id::text, ''), content, kind, importance, evidence, COALESCE(follow_up_at, 0), COALESCE(last_recalled_at, 0), created_at, updated_at`

// List 实现 memory.Store。
func (s *PgStore) List(personaID string, limit int) ([]memory.Memory, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// persona_id 传 NULL 时，"= NULL" 恒为 NULL、不会匹配任何行，
	// 于是只剩下 IS NULL 那一支——正是"只看公共记忆"的语义。
	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE persona_id IS NULL OR persona_id = $1::uuid
		ORDER BY created_at DESC
		LIMIT $2`, nullIfEmpty(personaID), limit)
	if err != nil {
		return nil, fmt.Errorf("读取记忆列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]memory.Memory, 0, 16)
	for rows.Next() {
		var m memory.Memory
		if err := rows.Scan(&m.ID, &m.PersonaID, &m.Content, &m.Kind, &m.Importance, &m.Evidence,
			&m.FollowUpAt, &m.LastRecalledAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析记忆失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历记忆失败: %w", err)
	}
	return out, nil
}

// DeletePersona 实现 memory.Store。
func (s *PgStore) DeletePersona(personaID string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	// 只删私有记忆：公共记忆（persona_id IS NULL）保留——
	// 人格没了，但"用户养了猫"这类事实仍然成立
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM memories WHERE persona_id = $1::uuid`, personaID); err != nil {
		return fmt.Errorf("删除记忆失败: %w", err)
	}
	return nil
}

// Close 实现 io.Closer。同样**不关连接池**：池是全应用共用的那一个，由 persona store 负责关。
func (s *PgStore) Close() error { return nil }

// vectorLiteral 把向量转成 pgvector 的字面量（形如 [0.1,0.2]）。
//
// pgx v5 不认 []float32、也不认识 vector 类型，所以只能以文本传入再 ::vector 转换。
// 精度取 32 位（与 float32 一致），避免打印出一串无意义的长尾数字。
func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// nullIfEmpty 把空字符串转成 SQL NULL（uuid 列传 "" 会直接报错）。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullIfZero 把 0 转成 SQL NULL（用于可空的 bigint 列）。
func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
