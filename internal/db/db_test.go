package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-for-you-love/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 表名写错或漏写属于**静默失效**：CREATE TABLE IF NOT EXISTS 不会报错，
// 只是表不存在，直到某个查询运行时才炸。所以在这里把表名钉住。
func TestSchemaFilesCoverAllTables(t *testing.T) {
	core := []string{
		"personas", "persona_rules", "persona_changes",
		"sessions", "messages",
		"app_settings", "schema_version",
	}
	for _, table := range core {
		if !strings.Contains(schemaSQL, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("schema.sql 缺少建表语句：%s", table)
		}
	}

	vector := []string{"memories", "session_index"}
	for _, table := range vector {
		if !strings.Contains(schemaVectorSQL, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("schema_vector.sql 缺少建表语句：%s", table)
		}
	}
	if !strings.Contains(schemaVectorSQL, "vector(1024)") {
		t.Error("向量列应当是 vector(1024)——维度固化在列定义里，换模型要改这一列并重算向量")
	}

	// 这条是**结构性**断言，不是格式洁癖：核心表里只要出现一个 vector 列，
	// 缺 pgvector 扩展时整段脚本都会失败，连人格都存不了——而人格不需要扩展。
	if strings.Contains(schemaSQL, "vector(") {
		t.Error("schema.sql 不应包含 vector 列：缺扩展时会让核心表一起建不出来")
	}
}

// 加了会话与记忆四张表之后版本必须往上走，否则老库不会被标记为已升级。
func TestSchemaVersionCoversNewTables(t *testing.T) {
	if SchemaVersion < 2 {
		t.Fatalf("加了 sessions/messages/memories/session_index 之后 SchemaVersion 应当至少为 2，实际 %d", SchemaVersion)
	}
}

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	config.LoadDotEnvUpward()
	dsn := DSNFromEnv()
	if dsn == "" {
		t.Skip("未配置 COMPANION_PG_DSN，跳过 PG 集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// 光"建表语句执行成功"不够：IF NOT EXISTS 的情况下，脚本可能什么都没做也不报错。
// 这条直接问库"这几张表在不在"，比检查 SQL 文本实在。
func TestTablesExistAfterOpen(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	want := []string{
		"personas", "persona_rules", "persona_changes",
		"sessions", "messages", "app_settings", "schema_version",
	}

	// 向量表是**可选**的：没装 pgvector 扩展时它们建不出来，而这是允许的状态
	// （记忆功能停用，其余照常）。所以先看扩展在不在，再决定要不要断言它们。
	var hasVector bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&hasVector); err != nil {
		t.Fatalf("检查 vector 扩展失败: %v", err)
	}
	if hasVector {
		want = append(want, "memories", "session_index")
	} else {
		t.Log("未安装 pgvector 扩展，跳过向量表断言")
	}

	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatalf("读取表清单失败: %v", err)
	}
	defer rows.Close()

	have := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("解析表名失败: %v", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历表清单失败: %v", err)
	}

	for _, table := range want {
		if !have[table] {
			t.Errorf("库里缺少表 %s（Open 应当把它建出来）", table)
		}
	}
	t.Logf("库里共 %d 张表，期望的 %d 张都在", len(have), len(want))
}

// 升级路径：库比程序旧时必须把版本号补上。
//
// 之所以"迁移"只是补个版本号，是因为建表语句全部幂等（IF NOT EXISTS），
// Open 里已经重新执行过一遍脚本，新表就位了。这条验证的是"版本标记会跟着走"——
// 否则每次都按老库处理，前端也读不到"已升级"这个事实。
func TestCheckVersionUpgradesOlder(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	var before int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&before); err != nil {
		t.Fatalf("读取版本失败: %v", err)
	}
	if before != SchemaVersion {
		t.Fatalf("前置条件不满足：库里应当是 v%d，实际 v%d", SchemaVersion, before)
	}

	// 把当前版本行挪走，模拟"老库"；跑完 checkVersion 会重新写回来（失败也只是下次启动再补，无害）
	if _, err := pool.Exec(ctx, `DELETE FROM schema_version WHERE version = $1`, SchemaVersion); err != nil {
		t.Fatalf("删除版本行失败: %v", err)
	}

	if err := checkVersion(ctx, pool); err != nil {
		t.Fatalf("升级旧库应当成功: %v", err)
	}

	var after int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&after); err != nil {
		t.Fatalf("读取版本失败: %v", err)
	}
	if after != SchemaVersion {
		t.Errorf("升级后版本应当是 v%d，实际 v%d", SchemaVersion, after)
	}
	t.Logf("版本标记已跟上：v%d → v%d", before, after)
}

// 库比程序新时必须拒绝启动：拿旧程序去操作新结构的库，可能悄悄写坏数据。
func TestCheckVersionRejectsNewer(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	const future = 9999
	if _, err := pool.Exec(ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES ($1, 0)`, future); err != nil {
		t.Fatalf("插入未来版本号失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM schema_version WHERE version = $1`, future)
	})

	if err := checkVersion(ctx, pool); err == nil {
		t.Fatal("库比程序新时 checkVersion 应当报错")
	} else {
		t.Logf("按预期拒绝: %v", err)
	}
}
