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
	chunks   []history.Chunk
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

// EnsureChunk 实现 history.Store。
func (s *MemoryStore) EnsureChunk(sessionID string, maxRunes int) (history.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.chunks) - 1; i >= 0; i-- {
		c := s.chunks[i]
		if c.SessionID != sessionID || c.EndedAt != 0 {
			continue
		}
		// 没到阈值就接着用它
		if maxRunes <= 0 || s.chunkRunesLocked(c.ID) < maxRunes {
			return c, nil
		}
		// 到了就把它收尾，再往下走开新片。这一步发生在**写入下一条用户消息之前**，
		// 所以片边界落在用户发言处——一个问答对不会被从中间切开。
		now := history.NowMillis()
		s.chunks[i].EndedAt = now
		s.chunks[i].UpdatedAt = now
		break
	}

	now := history.NowMillis()
	c := history.Chunk{
		ID:        uuid.NewString(),
		SessionID: sessionID,
		PersonaID: s.sessionPersonaLocked(sessionID),
		Seq:       s.nextSeqLocked(sessionID),
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.chunks = append(s.chunks, c)
	return c, nil
}

// chunkRunesLocked 返回该片的字符数。调用方需持锁。
func (s *MemoryStore) chunkRunesLocked(chunkID string) int {
	total := 0
	for _, m := range s.messages {
		if m.ChunkID == chunkID {
			total += len([]rune(m.Content))
		}
	}
	return total
}

// nextSeqLocked 返回该会话下一个片序号（从 1 开始）。调用方需持锁。
func (s *MemoryStore) nextSeqLocked(sessionID string) int {
	max := 0
	for _, c := range s.chunks {
		if c.SessionID == sessionID && c.Seq > max {
			max = c.Seq
		}
	}
	return max + 1
}

// sessionPersonaLocked 找出会话所属的人格。调用方需持锁。
func (s *MemoryStore) sessionPersonaLocked(sessionID string) string {
	for _, sess := range s.sessions {
		if sess.ID == sessionID {
			return sess.PersonaID
		}
	}
	return ""
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

// ChunkMessages 实现 history.Store。
func (s *MemoryStore) ChunkMessages(chunkID string, limit int) ([]history.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]history.Message, 0, 8)
	for _, m := range s.messages {
		if m.ChunkID == chunkID {
			out = append(out, m)
		}
	}
	// 条数兜底与 PG 侧口径一致：取尾部（最近的）；limit <= 0 表示全部
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
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

// ListChunks 实现 history.Store。
func (s *MemoryStore) ListChunks(sessionID string) ([]history.Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]history.Chunk, 0, 4)
	for _, c := range s.chunks {
		if c.SessionID == sessionID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// EndSession 实现 history.Store。
//
// 找不到会话时返回 nil 而不是错误：这个操作是幂等的，而"哪一轮算话题结束"是
// **异步**判断出来的——并发下会话可能已经被删掉（人格被删）或已经结束过，
// 把这种情况当错误只会让日志变吵。
func (s *MemoryStore) EndSession(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := history.NowMillis()
	for i := range s.sessions {
		if s.sessions[i].ID == sessionID && s.sessions[i].EndedAt == 0 {
			s.sessions[i].EndedAt = now
			s.sessions[i].UpdatedAt = now
			break
		}
	}

	// 顺手把当前片也收尾。这一步**必须做**：懒结算扫的是"已收尾但没摘要"的片，
	// 会话的最后一片若一直停在"未收尾"，那片就永远拿不到摘要、也永远不会被抽成事实。
	// 循环放在会话那段之外（而不是 break 之前）：会话已经结束过时也要兜一次，
	// 免得出现"会话结束了、片还开着"的孤儿片。
	for i := range s.chunks {
		if s.chunks[i].SessionID == sessionID && s.chunks[i].EndedAt == 0 {
			s.chunks[i].EndedAt = now
			s.chunks[i].UpdatedAt = now
		}
	}
	return nil
}

// PendingChunks 实现 history.Store。
func (s *MemoryStore) PendingChunks(limit int) ([]history.Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]history.Chunk, 0, 4)
	for _, c := range s.chunks {
		// 与 PG 侧口径一致：已收尾、还没摘要、且里面真的有消息
		if c.EndedAt == 0 || c.Summary != "" || !s.hasMessageLocked(c.ID) {
			continue
		}
		out = append(out, c)
	}
	// 按结束时间正序：先聊完的先结算（与 PG 的 ORDER BY ended_at 一致）
	sort.Slice(out, func(i, j int) bool { return out[i].EndedAt < out[j].EndedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// hasMessageLocked 判断该片里有没有消息。调用方需持锁。
func (s *MemoryStore) hasMessageLocked(chunkID string) bool {
	for _, m := range s.messages {
		if m.ChunkID == chunkID {
			return true
		}
	}
	return false
}

// SetChunkSummary 实现 history.Store。
//
// 找不到片时安静收场（与 EndSession 同一姿态）：这一步只是"打标记"，
// 片可能已经随着人格删除被级联清掉了，为此报错只会让日志变吵。
func (s *MemoryStore) SetChunkSummary(chunkID, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.chunks {
		if s.chunks[i].ID == chunkID {
			s.chunks[i].Summary = summary
			s.chunks[i].UpdatedAt = history.NowMillis()
			return nil
		}
	}
	return nil
}

// SetSessionTitleIfEmpty 实现 history.Store。
func (s *MemoryStore) SetSessionTitleIfEmpty(sessionID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.sessions {
		if s.sessions[i].ID == sessionID && s.sessions[i].Title == "" {
			s.sessions[i].Title = title
			s.sessions[i].UpdatedAt = history.NowMillis()
			return nil
		}
	}
	return nil
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

	keptChunks := make([]history.Chunk, 0, len(s.chunks))
	for _, c := range s.chunks {
		if c.PersonaID != personaID {
			keptChunks = append(keptChunks, c)
		}
	}
	s.chunks = keptChunks

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
