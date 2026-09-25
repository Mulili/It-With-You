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

// IndexChunk 实现 memory.Store。
func (s *PgStore) IndexChunk(ci memory.ChunkIndex, vec []float32) error {
	ctx, cancel := s.ctx()
	defer cancel()

	if ci.CreatedAt == 0 {
		ci.CreatedAt = memory.NowMillis()
	}
	// 冲突时只覆盖摘要与向量，**保留原有的 created_at**：它表示"这片是什么时候被索引的"，
	// 而重放不该把它刷新成现在（否则排查"索引是什么时候建的"就永远看到的是最近一次重放）
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO chunk_index (chunk_id, session_id, persona_id, summary, embedding, created_at)
		VALUES ($1, $2, $3, $4, $5::vector, $6)
		ON CONFLICT (chunk_id) DO UPDATE
		   SET summary = EXCLUDED.summary,
		       embedding = EXCLUDED.embedding`,
		ci.ChunkID, ci.SessionID, ci.PersonaID, ci.Summary, vectorLiteral(vec), ci.CreatedAt); err != nil {
		return fmt.Errorf("写入片索引失败: %w", err)
	}
	return nil
}

// Search 实现 memory.Store。
func (s *PgStore) Search(personaID string, vec []float32, limit int) ([]memory.MemoryHit, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// <=> 是余弦距离（1 - 相似度），HNSW 索引直接服务这个排序。
	// persona_id 传 NULL 时 "= NULL" 恒为 NULL、不会匹配任何行，于是只剩 IS NULL 那一支
	// ——正是"只看公共记忆"的语义（与 List 同一套路）
	rows, err := s.pool.Query(ctx, `
		SELECT `+memoryColumns+`, 1 - (embedding <=> $1::vector) AS score
		FROM memories
		WHERE persona_id IS NULL OR persona_id = $2::uuid
		ORDER BY embedding <=> $1::vector
		LIMIT $3`, vectorLiteral(vec), nullIfEmpty(personaID), limit)
	if err != nil {
		return nil, fmt.Errorf("检索记忆失败: %w", err)
	}
	defer rows.Close()

	out := make([]memory.MemoryHit, 0, 8)
	for rows.Next() {
		var h memory.MemoryHit
		if err := rows.Scan(&h.Memory.ID, &h.Memory.PersonaID, &h.Memory.Content, &h.Memory.Kind,
			&h.Memory.Importance, &h.Memory.Evidence, &h.Memory.FollowUpAt,
			&h.Memory.LastRecalledAt, &h.Memory.CreatedAt, &h.Memory.UpdatedAt, &h.Score); err != nil {
			return nil, fmt.Errorf("解析检索结果失败: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历检索结果失败: %w", err)
	}
	return out, nil
}

// SearchChunks 实现 memory.Store。
func (s *PgStore) SearchChunks(personaID string, vec []float32, limit int) ([]memory.ChunkHit, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// chunk_index.persona_id 不可空（片必然属于某个人格），所以这里是严格相等，
	// 与"记忆可公共"的语义不同
	rows, err := s.pool.Query(ctx, `
		SELECT chunk_id, session_id, persona_id, summary, created_at,
		       1 - (embedding <=> $1::vector) AS score
		FROM chunk_index
		WHERE persona_id = $2::uuid
		ORDER BY embedding <=> $1::vector
		LIMIT $3`, vectorLiteral(vec), personaID, limit)
	if err != nil {
		return nil, fmt.Errorf("检索片索引失败: %w", err)
	}
	defer rows.Close()

	out := make([]memory.ChunkHit, 0, 4)
	for rows.Next() {
		var h memory.ChunkHit
		if err := rows.Scan(&h.Chunk.ChunkID, &h.Chunk.SessionID, &h.Chunk.PersonaID,
			&h.Chunk.Summary, &h.Chunk.CreatedAt, &h.Score); err != nil {
			return nil, fmt.Errorf("解析片索引失败: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历片索引失败: %w", err)
	}
	return out, nil
}

// MarkRecalled 实现 memory.Store。
func (s *PgStore) MarkRecalled(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := s.ctx()
	defer cancel()

	// 逐条 UPDATE，而不是 ANY($1::uuid[])：把 []string 编码成 uuid[] 要靠 pgx 的类型推断，
	// 传错只会在运行时炸；而这里最多三条，多几次往返不值得为它冒这个险
	now := memory.NowMillis()
	for _, id := range ids {
		if _, err := s.pool.Exec(ctx,
			`UPDATE memories SET last_recalled_at = $2 WHERE id = $1::uuid`, id, now); err != nil {
			return fmt.Errorf("记录记忆回想的时刻失败: %w", err)
		}
	}
	return nil
}

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

// Delete 实现 memory.Store。
func (s *PgStore) Delete(id string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	// 影响 0 行不报错（与内存实现一致）。注意**格式非法**的 id 会真报错（22P02）——
	// 那属于调用方传错了东西，与"这条已经不存在"不是一回事，让它露出来更好
	if _, err := s.pool.Exec(ctx, `DELETE FROM memories WHERE id = $1::uuid`, id); err != nil {
		return fmt.Errorf("删除记忆失败: %w", err)
	}
	return nil
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
