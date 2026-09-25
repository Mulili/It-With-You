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

// chunkColumns 是读片的列清单，与各处的 Scan 顺序一一对应。
const chunkColumns = `id, session_id, persona_id, seq, started_at, COALESCE(ended_at, 0), summary, created_at, updated_at`

// scanChunk 把一行读成片。
func scanChunk(row pgx.Row) (history.Chunk, error) {
	var c history.Chunk
	err := row.Scan(&c.ID, &c.SessionID, &c.PersonaID, &c.Seq,
		&c.StartedAt, &c.EndedAt, &c.Summary, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// nullIfEmpty 把空字符串转成 SQL NULL。
//
// 用途只有一个：chunk_id 在老行（v3 之前写入的）里是空的，而给 uuid 列传 ""
// 会让 pgx 直接报错——传 nil 才是"没有值"。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullIfLimit 把"不限"（limit <= 0）转成 SQL NULL——PG 的 LIMIT NULL 就是不限。
func nullIfLimit(limit int) any {
	if limit <= 0 {
		return nil
	}
	return limit
}

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

// EnsureChunk 实现 history.Store。
func (s *PgStore) EnsureChunk(sessionID string, maxRunes, maxMessages int) (history.Chunk, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// 找当前片（未收尾）。每个会话最多一片，部分索引 session_chunks_open_idx 服务这条查询
	row := s.pool.QueryRow(ctx, `
		SELECT `+chunkColumns+`
		FROM session_chunks
		WHERE session_id = $1 AND ended_at IS NULL
		ORDER BY seq DESC
		LIMIT 1`, sessionID)

	cur, err := scanChunk(row)
	switch {
	case err == nil:
		runes, count, err := s.chunkSize(ctx, cur.ID)
		if err != nil {
			return history.Chunk{}, err
		}
		// 字符数与条数**任一到达上限**就切（传 <=0 表示该项不设限）。
		// 为什么要两个上限：见 Store 接口的 EnsureChunk 注释（只按字符切会让短消息落在缝里）
		if (maxRunes <= 0 || runes < maxRunes) && (maxMessages <= 0 || count < maxMessages) {
			return cur, nil
		}
		// 到阈值了：先把当前片收尾再开新片。
		// **这一步发生在写入下一条用户消息之前**，所以片边界落在用户发言处，
		// 一个问答对不会被从中间切开。
		now := history.NowMillis()
		if _, err := s.pool.Exec(ctx,
			`UPDATE session_chunks SET ended_at = $2, updated_at = $2 WHERE id = $1`,
			cur.ID, now); err != nil {
			return history.Chunk{}, fmt.Errorf("收尾当前片失败: %w", err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// 还没有片，下面直接建第一片
	default:
		return history.Chunk{}, fmt.Errorf("查询当前片失败: %w", err)
	}

	var nextSeq int
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM session_chunks WHERE session_id = $1`,
		sessionID).Scan(&nextSeq); err != nil {
		return history.Chunk{}, fmt.Errorf("计算片序号失败: %w", err)
	}
	// persona_id 从会话取：片冗余它是为了让"某人格当前片"不必 JOIN，
	// 但值必须与会话一致，所以在这里现取而不是让调用方传
	var personaID string
	if err := s.pool.QueryRow(ctx,
		`SELECT persona_id FROM sessions WHERE id = $1`, sessionID).Scan(&personaID); err != nil {
		return history.Chunk{}, fmt.Errorf("读取会话所属人格失败: %w", err)
	}

	now := history.NowMillis()
	c := history.Chunk{
		ID:        uuid.NewString(),
		SessionID: sessionID,
		PersonaID: personaID,
		Seq:       nextSeq,
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO session_chunks (id, session_id, persona_id, seq, started_at, ended_at, summary, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULL, '', $6, $7)`,
		c.ID, c.SessionID, c.PersonaID, c.Seq, c.StartedAt, c.CreatedAt, c.UpdatedAt); err != nil {
		return history.Chunk{}, fmt.Errorf("新建片失败: %w", err)
	}
	return c, nil
}

// chunkSize 返回该片的字符数与消息条数。
//
// 两个数一次查出来：它们每次发消息都要一起判（见 EnsureChunk），分两条 SQL 就是白跑一趟。
//
// 在 SQL 里算而不是把消息全取回 Go 再数：片可能有几百条消息，来回传这些数据不值得。
// PG 的 length() 按**字符**计数（不是字节），与 Go 侧的 rune 口径一致。
func (s *PgStore) chunkSize(ctx context.Context, chunkID string) (runes, count int, err error) {
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(length(content)), 0), COUNT(*) FROM messages WHERE chunk_id = $1`,
		chunkID).Scan(&runes, &count); err != nil {
		return 0, 0, fmt.Errorf("统计片大小失败: %w", err)
	}
	return runes, count, nil
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

	// chunk_id 可能是空串（v3 之前写入的老行没有分片）；给 uuid 列传 "" 会报错，转成 NULL
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO messages (id, session_id, chunk_id, persona_id, role, content, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		m.ID, m.SessionID, nullIfEmpty(m.ChunkID), m.PersonaID,
		m.Role, m.Content, m.Status, m.CreatedAt); err != nil {
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

// ChunkMessages 实现 history.Store。
func (s *PgStore) ChunkMessages(chunkID string, limit int) ([]history.Message, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// 取尾部 limit 条（最近的）再翻回正序：直接写 ORDER BY created_at ASC LIMIT n
	// 会取到片**开头**的 n 条，正好是反的——这是个很容易写错、且错了也不报错的地方。
	//
	// limit <= 0 传 NULL 下去：PG 的 LIMIT NULL 就是"不限"，于是两种情况共用一条 SQL。
	// （分成两条写的话，"有没有上限"这个差异会散落在两处 ORDER BY / LIMIT 里，
	// 而它们恰是最容易写反的地方。）
	//
	// chunk_id 用 COALESCE 兜成空串：老行里它是 NULL，扫进 Go 的 string 会失败。
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, COALESCE(chunk_id::text, ''), persona_id, role, content, status, created_at FROM (
			SELECT id, session_id, chunk_id, persona_id, role, content, status, created_at
			FROM messages
			WHERE chunk_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		) t
		ORDER BY created_at ASC`, chunkID, nullIfLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("读取片消息失败: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// SessionMessages 实现 history.Store。
//
// 与 ChunkMessages 同一套写法（取尾部再翻回正序、limit <= 0 传 NULL），只换了筛选列。
func (s *PgStore) SessionMessages(sessionID string, limit int) ([]history.Message, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, COALESCE(chunk_id::text, ''), persona_id, role, content, status, created_at FROM (
			SELECT id, session_id, chunk_id, persona_id, role, content, status, created_at
			FROM messages
			WHERE session_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		) t
		ORDER BY created_at ASC`, sessionID, nullIfLimit(limit))
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
		SELECT id, session_id, COALESCE(chunk_id::text, ''), persona_id, role, content, status, created_at FROM (
			SELECT id, session_id, chunk_id, persona_id, role, content, status, created_at
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
		if err := rows.Scan(&m.ID, &m.SessionID, &m.ChunkID, &m.PersonaID,
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

// ListChunks 实现 history.Store。
func (s *PgStore) ListChunks(sessionID string) ([]history.Chunk, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT `+chunkColumns+`
		FROM session_chunks
		WHERE session_id = $1
		ORDER BY seq ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("读取片列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]history.Chunk, 0, 4)
	for rows.Next() {
		var c history.Chunk
		if err := rows.Scan(&c.ID, &c.SessionID, &c.PersonaID, &c.Seq,
			&c.StartedAt, &c.EndedAt, &c.Summary, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析片失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历片失败: %w", err)
	}
	return out, nil
}

// EndSession 实现 history.Store。
//
// WHERE 里的 ended_at IS NULL 让它天然幂等：已经结束的会话不会被改第二次时间。
// 这也顺带挡住了一个并发场景——同一个话题可能被连续两轮都判成"结束"。
func (s *PgStore) EndSession(sessionID string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	now := history.NowMillis()
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET ended_at = $2, updated_at = $2 WHERE id = $1 AND ended_at IS NULL`,
		sessionID, now); err != nil {
		return fmt.Errorf("结束会话失败: %w", err)
	}

	// 顺手把这个会话的当前片也收尾（理由见 history.Store 的接口注释：
	// 会话的最后一片若停在"未收尾"，它就永远拿不到摘要）。
	// 不加"会话本来就没结束"的条件：会话已结束时这一步是空操作，而万一有片在会话结束后
	// 才被开出来，这里也能兜住——代价只是一次 UPDATE。
	if _, err := s.pool.Exec(ctx,
		`UPDATE session_chunks SET ended_at = $2, updated_at = $2 WHERE session_id = $1 AND ended_at IS NULL`,
		sessionID, now); err != nil {
		return fmt.Errorf("收尾会话的当前片失败: %w", err)
	}
	return nil
}

// PendingChunks 实现 history.Store。
func (s *PgStore) PendingChunks(limit int) ([]history.Chunk, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	// 三个条件缺一不可：已收尾、还没摘要、**里面真的有消息**。
	// 最后一条是为了让空片不再被反复扫到（它们结算不出任何东西）。
	// 按 ended_at 正序：先聊完的先结算，与"回忆"的时间感一致。
	rows, err := s.pool.Query(ctx, `
		SELECT `+chunkColumns+`
		FROM session_chunks c
		WHERE c.ended_at IS NOT NULL AND c.summary = ''
		  AND EXISTS (SELECT 1 FROM messages m WHERE m.chunk_id = c.id)
		ORDER BY c.ended_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("扫描待结算的片失败: %w", err)
	}
	defer rows.Close()

	out := make([]history.Chunk, 0, 4)
	for rows.Next() {
		c, err := scanChunk(rows)
		if err != nil {
			return nil, fmt.Errorf("解析待结算的片失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历待结算的片失败: %w", err)
	}
	return out, nil
}

// SetChunkSummary 实现 history.Store。
func (s *PgStore) SetChunkSummary(chunkID, summary string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	if _, err := s.pool.Exec(ctx,
		`UPDATE session_chunks SET summary = $2, updated_at = $3 WHERE id = $1`,
		chunkID, summary, history.NowMillis()); err != nil {
		return fmt.Errorf("写片摘要失败: %w", err)
	}
	return nil
}

// SetSessionTitleIfEmpty 实现 history.Store。
func (s *PgStore) SetSessionTitleIfEmpty(sessionID, title string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	// WHERE 里的 title = '' 就是"只写第一次"：SQL 一次搞定，不需要先读再判
	//（先读再判会有"两个片同时结算"的竞争窗口）
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET title = $2, updated_at = $3 WHERE id = $1 AND title = ''`,
		sessionID, title, history.NowMillis()); err != nil {
		return fmt.Errorf("写会话标题失败: %w", err)
	}
	return nil
}

// DeletePersona 实现 history.Store。
func (s *PgStore) DeletePersona(personaID string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	// 先删片再删会话：messages 的外键指向片、片指向会话，从叶子往根删读起来更清楚。
	// CASCADE 其实兜得住，但显式写出来意图明确。
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM session_chunks WHERE persona_id = $1`, personaID); err != nil {
		return fmt.Errorf("删除历史分片失败: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM sessions WHERE persona_id = $1`, personaID); err != nil {
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
