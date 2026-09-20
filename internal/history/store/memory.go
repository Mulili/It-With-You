// Package store 是 history 的两种实现：内存（开发调试用）与 PostgreSQL（真源）。
package store

import (
	"sort"
	"sync"

	"agent-for-you-love/internal/history"

	"github.com/google/uuid"
)

// MemoryStore 把历史放在进程内存里。
//
// 它现在只服务开发调试与快速验 UI：数据库已是硬性要求（见 operation.md「数据库改为硬性要求」），
// 用户侧走不到这条路径。所以它不追求查询能力，只保证"对话能正常跑起来"。
type MemoryStore struct {
	mu       sync.RWMutex
	sessions []history.Session
	messages []history.Message
}

// NewMemoryStore 建一个空的内存历史存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

// EnsureSession 实现 history.Store。
func (s *MemoryStore) EnsureSession(personaID string) (history.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 从后往前找最近一个未收尾的会话：slice 按插入顺序，尾部最新
	for i := len(s.sessions) - 1; i >= 0; i-- {
		if s.sessions[i].PersonaID == personaID && s.sessions[i].EndedAt == 0 {
			return s.sessions[i], nil
		}
	}

	now := history.NowMillis()
	sess := history.Session{
		ID:        uuid.NewString(),
		PersonaID: personaID,
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.sessions = append(s.sessions, sess)
	return sess, nil
}

// AppendMessage 实现 history.Store。
func (s *MemoryStore) AppendMessage(m history.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = history.NowMillis()
	}
	if m.Status == "" {
		m.Status = history.StatusOK
	}
	s.messages = append(s.messages, m)
	return m.ID, nil
}

// MessagesOf 实现 history.Store。
func (s *MemoryStore) MessagesOf(sessionID string) ([]history.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]history.Message, 0, 8)
	for _, m := range s.messages {
		if m.SessionID == sessionID {
			out = append(out, m)
		}
	}
	return out, nil
}

// RecentMessages 实现 history.Store。
//
// 返回的是**时间正序**的最近 limit 条：取尾部 limit 条但**不反转**——
// "取最近的"是筛选条件，"正序"是给前端直接渲染的顺序，两件事别混。
func (s *MemoryStore) RecentMessages(personaID string, limit int) ([]history.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	mine := make([]history.Message, 0, len(s.messages))
	for _, m := range s.messages {
		if m.PersonaID == personaID {
			mine = append(mine, m)
		}
	}
	if limit > 0 && len(mine) > limit {
		mine = mine[len(mine)-limit:]
	}
	return mine, nil
}

// ListSessions 实现 history.Store。
func (s *MemoryStore) ListSessions(personaID string, limit int) ([]history.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]history.Session, 0, 8)
	for _, sess := range s.sessions {
		if sess.PersonaID == personaID {
			out = append(out, sess)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DeletePersona 实现 history.Store。
func (s *MemoryStore) DeletePersona(personaID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	keptSessions := make([]history.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.PersonaID != personaID {
			keptSessions = append(keptSessions, sess)
		}
	}
	s.sessions = keptSessions

	keptMessages := make([]history.Message, 0, len(s.messages))
	for _, m := range s.messages {
		if m.PersonaID != personaID {
			keptMessages = append(keptMessages, m)
		}
	}
	s.messages = keptMessages
	return nil
}

// Close 实现 io.Closer。内存实现无事可做，与 persona 的内存实现保持一致
// （app.shutdown 用 io.Closer 统一处理两种存储）。
func (s *MemoryStore) Close() error { return nil }
