// Package store 存"语气种子的生成状态"：每个人格一行，记她说过多少条、生成过哪一版。
//
// 不存种子本身（那是可读可改的 markdown 文件）；这里只解决一件事：
// "要不要现在生成"必须有一个跨重启的答案，否则每次启动都会重新烧一次调用。
package store

import (
	"context"
	"fmt"
	"log"
	"sync"

	"agent-for-you-love/internal/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State 是某个人格的生成状态。
//
// ProfileVersion / SeedsVersion 记的是"生成那一刻的助手消息条数"，不是版本序号：
// 这样"什么时候该重生成"只需和当前条数比，不必另存时间戳。
type State struct {
	PersonaID      string
	AssistantCount int
	ProfileVersion int
	SeedsVersion   int
	LastAttemptAt  int64
}

// Store 是生成状态的存储契约。
type Store interface {
	// Get 返回状态；没有记录时返回零值（PersonaID 为空），不当作错误。
	Get(personaID string) (State, error)
	// Save 整体覆盖这一行（幂等）。
	Save(s State) error
}

// Open 返回可用的状态存储：给了连接池就用 PG，否则退回内存实现。
//
// 内存实现只服务探针与测试；真实运行下没有数据库时情绪种子本来也长不出来
// （历史也在内存里，重启即丢），不必为它设计降级。
func Open(pool *pgxpool.Pool) Store {
	if pool == nil {
		log.Printf("[emotion] 没有数据库连接，种子生成状态只存在内存里")
		return NewMemory()
	}
	return &PG{pool: pool}
}

// Memory 是进程内实现。
type Memory struct {
	mu sync.Mutex
	m  map[string]State
}

// NewMemory 建一个空的内存状态存储。
func NewMemory() *Memory { return &Memory{m: map[string]State{}} }

// Get 实现 Store。
func (s *Memory) Get(personaID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[personaID], nil
}

// Save 实现 Store。
func (s *Memory) Save(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[st.PersonaID] = st
	return nil
}

// PG 是 PostgreSQL 实现。
type PG struct{ pool *pgxpool.Pool }

func (s *PG) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), db.QueryTimeout)
}

// Get 实现 Store。
func (s *PG) Get(personaID string) (State, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	st := State{PersonaID: personaID}
	err := s.pool.QueryRow(ctx, `
		SELECT assistant_count, profile_version, seeds_version, last_attempt_at
		FROM persona_emotion_state WHERE persona_id = $1`, personaID).
		Scan(&st.AssistantCount, &st.ProfileVersion, &st.SeedsVersion, &st.LastAttemptAt)
	if err == pgx.ErrNoRows {
		return st, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("读取种子生成状态失败: %w", err)
	}
	return st, nil
}

// Save 实现 Store。
func (s *PG) Save(st State) error {
	ctx, cancel := s.ctx()
	defer cancel()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO persona_emotion_state
			(persona_id, assistant_count, profile_version, seeds_version, last_attempt_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (persona_id) DO UPDATE SET
			assistant_count = EXCLUDED.assistant_count,
			profile_version = EXCLUDED.profile_version,
			seeds_version   = EXCLUDED.seeds_version,
			last_attempt_at = EXCLUDED.last_attempt_at`,
		st.PersonaID, st.AssistantCount, st.ProfileVersion, st.SeedsVersion, st.LastAttemptAt)
	if err != nil {
		return fmt.Errorf("写入种子生成状态失败: %w", err)
	}
	return nil
}
