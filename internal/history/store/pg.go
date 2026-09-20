package store

import (
	"context"
	"errors"
	"fmt"
	"log"

	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/history"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore 把历史存在 PostgreSQL 里。它是真源——历史不再有内存副本。
//
// 为什么敢不做内存缓存：写入频率很低（每轮两条），读取也很低（每轮一次拼上下文 +
// 打开菜单时一次），而"两份数据要成对维护"的代价却很高（漏一处就永久不同步）。
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore 用已有的连接池组装历史存储。表结构由 internal/db 在 Open 时保证就位。
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Open 返回可用的历史存储：给了连接池就用 PG，否则退回内存实现。
//
// 与 persona.OpenStore 同一姿态：数据库不可用时不该让应用起不来。
// 用户侧由前端的阻断页挡住，所以这里只需保证"代码路径不会因为 nil 崩掉"。
func Open(pool *pgxpool.Pool) history.Store {
	if pool == nil {
		log.Printf("[history] 没有可用的数据库连接，历史只存在内存里（进程重启即丢）")
		return NewMemoryStore()
	}
	return NewPgStore(pool)
}

func (s *PgStore) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), db.QueryTimeout)
}

// sessionColumns 是读会话的列清单，与各处的 Scan 顺序一一对应。
//
// ended_at 用 COALESCE 兜成 0：Go 侧用 0 表示"未收尾"，而库里是 NULL——
// 让 NULL 透到 Go 会逼每个调用点都处理一遍可空性，不如在 SQL 边界上收敛掉。
const sessionColumns = `id, persona_id, title, summary, started_at, COALESCE(ended_at, 0), created_at, updated_at`

// EnsureSession 实现 history.Store。
func (s *PgStore) EnsureSession(personaID string) (history.Session, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// 未收尾的会话最多一条，schema 里的部分索引 sessions_open_idx 就是为这条查询建的
	row := s.pool.QueryRow(ctx, `
		SELECT `+sessionColumns+`
		FROM sessions
		WHERE persona_id = $1 AND ended_at IS NULL
		ORDER BY started_at DESC
		LIMIT 1`, personaID)

	var sess history.Session
	err := row.Scan(&sess.ID, &sess.PersonaID, &sess.Title, &sess.Summary,
		&sess.StartedAt, &sess.EndedAt, &sess.CreatedAt, &sess.UpdatedAt)
	if err == nil {
		return sess, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return history.Session{}, fmt.Errorf("查询当前会话失败: %w", err)
	}

	// 没有未收尾的会话，开一段新的。
	// 并发下理论上可能开出两段（两个 Ask 同时走到这里），但桌面单用户场景下 Ask 是串行的；
	// 真出现了也无害——EnsureSession 只取最近的那条，多出来的会在收尾时被一起处理。
	now := history.NowMillis()
	sess = history.Session{
		ID:        uuid.NewString(),
		PersonaID: personaID,
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, persona_id, title, summary, started_at, ended_at, created_at, updated_at)
		VALUES ($1, $2, '', '', $3, NULL, $4, $5)`,
		sess.ID, sess.PersonaID, sess.StartedAt, sess.CreatedAt, sess.UpdatedAt); err != nil {
		return history.Session{}, fmt.Errorf("新建会话失败: %w", err)
	}
	return sess, nil
}

// AppendMessage 实现 history.Store。
func (s *PgStore) AppendMessage(m history.Message) (string, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = history.NowMillis()
	}
	if m.Status == "" {
		m.Status = history.StatusOK
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO messages (id, session_id, persona_id, role, content, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		m.ID, m.SessionID, m.PersonaID, m.Role, m.Content, m.Status, m.CreatedAt); err != nil {
		return "", fmt.Errorf("写入消息失败: %w", err)
	}

	// 会话的 updated_at 跟着走：将来按"最后活跃时间"排序或清理时会用到。
	// 这条失败不影响消息本身（消息已经进去了），所以只记日志。
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET updated_at = $2 WHERE id = $1`, m.SessionID, m.CreatedAt); err != nil {
		log.Printf("[history] 更新会话时间失败: %v", err)
	}
	return m.ID, nil
}

// MessagesOf 实现 history.Store。
func (s *PgStore) MessagesOf(sessionID string) ([]history.Message, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// 取尾部 limit 条（最近的）再翻回正序：直接写 ORDER BY created_at ASC LIMIT n
	// 会取到会话**开头**的 n 条，正好是反的——这是个很容易写错、且错了也不报错的地方。
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, persona_id, role, content, status, created_at FROM (
			SELECT id, session_id, persona_id, role, content, status, created_at
			FROM messages
			WHERE session_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		) t
		ORDER BY created_at ASC`, sessionID, history.ContextMessagesLimit)
	if err != nil {
		return nil, fmt.Errorf("读取会话消息失败: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// RecentMessages 实现 history.Store。
func (s *PgStore) RecentMessages(personaID string, limit int) ([]history.Message, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, persona_id, role, content, status, created_at FROM (
			SELECT id, session_id, persona_id, role, content, status, created_at
			FROM messages
			WHERE persona_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		) t
		ORDER BY created_at ASC`, personaID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取历史消息失败: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// scanMessages 把结果集读成消息切片。
func scanMessages(rows pgx.Rows) ([]history.Message, error) {
	out := make([]history.Message, 0, 32)
	for rows.Next() {
		var m history.Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.PersonaID,
			&m.Role, &m.Content, &m.Status, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析消息失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历消息失败: %w", err)
	}
	return out, nil
}

// ListSessions 实现 history.Store。
func (s *PgStore) ListSessions(personaID string, limit int) ([]history.Session, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT `+sessionColumns+`
		FROM sessions
		WHERE persona_id = $1
		ORDER BY started_at DESC
		LIMIT $2`, personaID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取会话列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]history.Session, 0, 8)
	for rows.Next() {
		var sess history.Session
		if err := rows.Scan(&sess.ID, &sess.PersonaID, &sess.Title, &sess.Summary,
			&sess.StartedAt, &sess.EndedAt, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析会话失败: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话失败: %w", err)
	}
	return out, nil
}

// DeletePersona 实现 history.Store。
func (s *PgStore) DeletePersona(personaID string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	// 只删会话，messages 由 ON DELETE CASCADE 跟着走（外键在 schema 里定义）
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE persona_id = $1`, personaID); err != nil {
		return fmt.Errorf("删除历史失败: %w", err)
	}
	return nil
}

// Close 实现 io.Closer。
//
// **刻意不关连接池**：池是全应用共用的那一个（人格、会话、记忆都在上面），
// 由 persona store 负责关闭。这里若也关，先被调到的那个就会把池关掉、另一个立刻失效。
// 保留这个方法只是让 app.shutdown 能用统一的方式遍历两种存储。
func (s *PgStore) Close() error { return nil }
