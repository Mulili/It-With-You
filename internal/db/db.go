// Package db 只管一件事：连上 PostgreSQL，并保证表结构就位。
//
// 为什么单独成包：库里既有阶段3 的人格表，也有阶段4 的会话与记忆表，
// 而"连接池 + 表结构"只有一份。放进任一个 store 包里，都会让另一个反向依赖它。
//
// 降级由调用方决定：本包只返回错误，不替上层决定"连不上该怎么办"——
// 人格退回内存实现照样可用，记忆则是整体停用，两种取舍不一样。
package db

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion 是当前代码期望的表结构版本（对应 schema.sql）。
//
// 版本号的含义是"程序认为库已经是什么结构"，而不是"执行过哪些迁移脚本"——
// 因为建表语句全部幂等，升级就等于重新执行一遍再把版本号补上。
//
//	v1 → v2：加 sessions / messages（阶段4 会话）与 memories / session_index（记忆检索）
//	v2 → v3：加 session_chunks（会话内分片）、messages 加 chunk_id；
//	        会话级的 session_index 改为片级的 chunk_index
//	v3 → v4：加 persona_rule_candidates（隐式演化的候选区）
const SchemaVersion = 4

// envDSN 是连接串所在的环境变量名。
const envDSN = "COMPANION_PG_DSN"

// ConnectTimeout 是启动时连库的超时：桌面应用不该为了等数据库卡住启动。
const ConnectTimeout = 3 * time.Second

// QueryTimeout 是单次查询的超时。Store 的方法签名不带 ctx（要直接暴露给前端），
// 所以统一加一层保护，避免数据库卡住时把界面拖死。
const QueryTimeout = 5 * time.Second

// DSNFromEnv 读取连接串；为空表示没配置。
func DSNFromEnv() string { return strings.TrimSpace(os.Getenv(envDSN)) }

//go:embed schema.sql
var schemaSQL string

// schemaVectorSQL 是需要 pgvector 扩展的那部分表。
//
// 单独成文件，是为了让"没装扩展"只影响记忆功能：如果它和核心表挤在同一个脚本里，
// 一条 CREATE TABLE ... vector(1024) 失败就会让整个脚本失败，
// 连人格都存不了——而人格根本不需要扩展。
//
//go:embed schema_vector.sql
var schemaVectorSQL string

// Open 连库、建表、校验版本，返回可用的连接池。
//
// 连不上就返回错误，**不在这里降级**：降级是上层的决定，且不同用途的降级方式不同。
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf("未配置 %s", envDSN)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("创建连接池失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("初始化核心表失败: %w", err)
	}
	// 向量表失败只警告：记忆功能停用，人格与历史照常
	if _, err := pool.Exec(ctx, schemaVectorSQL); err != nil {
		log.Printf("[db] 向量表未就绪，记忆功能将停用：%v", err)
		log.Printf("[db] 若尚未安装扩展，先执行：psql -U postgres -d <库名> -c \"CREATE EXTENSION IF NOT EXISTS vector;\"")
	}
	if err := checkVersion(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// checkVersion 比对表结构版本：0 = 首次初始化；库比程序新 = 拒绝启动。
//
// 升级（库比程序旧）之所以只需补个版本号，是因为建表语句全部幂等：
// "迁移"就等于重新执行一遍 schema.sql（Open 里已经做过了）。
func checkVersion(ctx context.Context, pool *pgxpool.Pool) error {
	var version int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("读取表结构版本失败: %w", err)
	}
	switch {
	case version == SchemaVersion:
		return nil
	case version > SchemaVersion:
		return fmt.Errorf("数据库表结构是 v%d，比程序期望的 v%d 新，请升级程序", version, SchemaVersion)
	}

	// schema_version 按版本号一行，所以升级是 INSERT 而不是 UPDATE——历史版本留痕
	if _, err := pool.Exec(ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES ($1, $2)`,
		SchemaVersion, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("写入表结构版本失败: %w", err)
	}
	if version == 0 {
		log.Printf("[db] 表结构初始化为 v%d", SchemaVersion)
	} else {
		log.Printf("[db] 表结构 v%d → v%d（幂等建表已补齐新表）", version, SchemaVersion)
	}
	return nil
}
