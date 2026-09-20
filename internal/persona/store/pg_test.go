package store

import (
	"context"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"
)

// PG 集成测试：需要一个可用的库（.env 里的 COMPANION_PG_DSN）。没配就自动跳过——
// 与 internal/llm 的集成测试同一套约定。
//
//	go test ./internal/persona/store -run TestPg -v -count=1
//
// 测试只碰它自己建的临时人格（名字带时间戳，结束时删掉），不动内置人格与用户已有数据。

func openTestPgStore(t *testing.T) *PgStore {
	t.Helper()
	config.LoadDotEnvUpward()
	dsn := db.DSNFromEnv()
	if dsn == "" {
		t.Skip("未配置 COMPANION_PG_DSN，跳过 PG 集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	builtins, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	store := NewPgStore(pool, builtins)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPgStoreRoundTrip(t *testing.T) {
	s := openTestPgStore(t)

	name := "集成测试人格 " + time.Now().Format("0102-150405.000")
	id, err := s.CreatePersona(name, "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}
	t.Cleanup(func() { _ = s.DeletePersona(id) }) // 不清会留垃圾数据

	if err := s.SetActivePersona(id); err != nil {
		t.Fatalf("切换当前人格失败: %v", err)
	}
	if err := s.SaveSeedText(id, "你是集成测试用的临时人格。"); err != nil {
		t.Fatalf("写主体文本失败: %v", err)
	}

	// 单值槽位：第二次写入应当是覆盖，且沿用同一个规则 ID
	first, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "address_user", Value: "甲"})
	if err != nil {
		t.Fatalf("写规则失败: %v", err)
	}
	second, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "address_user", Value: "乙"})
	if err != nil {
		t.Fatalf("覆盖单值槽位失败: %v", err)
	}
	if first != second {
		t.Errorf("覆盖应沿用原规则 ID：%s → %s", first, second)
	}

	// 多值槽位：两条都留，同值拒绝
	if _, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "taboo", Value: "不要长篇大论"}); err != nil {
		t.Fatalf("写多值槽位失败: %v", err)
	}
	if _, err := s.SaveRule(persona.PersonaRule{PersonaID: id, Slot: "taboo", Value: "不要长篇大论"}); err == nil {
		t.Error("多值槽位重复取值应被拒绝")
	}

	snap := s.Snapshot()
	if !snap.StorageReady {
		t.Error("PG 实现应当标记 StorageReady=true")
	}
	if snap.ActiveID != id {
		t.Errorf("当前人格 = %q，期望 %q", snap.ActiveID, id)
	}
	if len(snap.Rules) != 2 {
		t.Errorf("规则数 = %d，期望 2（address_user + taboo）：%+v", len(snap.Rules), snap.Rules)
	}
	for _, r := range snap.Rules {
		if r.Slot == "address_user" && r.Value != "乙" {
			t.Errorf("单值槽位没被覆盖成「乙」，实际 %q", r.Value)
		}
	}
	if len(snap.RecentChanges) == 0 {
		t.Error("变更记录应当有内容")
	}

	// 权限：内置人格只读
	if err := s.SaveSeedText("builtin:lapwing", "试图改内置"); err == nil {
		t.Error("内置人格应当只读")
	}

	// 导出 → 导入：同名改存副本、规则重新生成 ID、不带 evidence
	f, err := s.ExportFile(id)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	imported, err := s.ImportFile(f)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	t.Cleanup(func() { _ = s.DeletePersona(imported.ID) })
	if imported.Name == name {
		t.Errorf("同名导入应当改存副本，实际仍叫 %q", imported.Name)
	}
	if imported.Origin != persona.OriginImported {
		t.Errorf("导入来源应为 %s，实际 %q", persona.OriginImported, imported.Origin)
	}
	if importedRules, err := s.rulesOf(context.Background(), imported.ID); err != nil {
		t.Fatalf("读取导入后的规则失败: %v", err)
	} else if len(importedRules) != 2 {
		t.Errorf("导入后规则数 = %d，期望 2", len(importedRules))
	}

	// 删除人格：规则与变更记录靠外键级联一起清掉
	if err := s.DeletePersona(id); err != nil {
		t.Fatalf("删除人格失败: %v", err)
	}
	var leftRules, leftChanges int
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM persona_rules WHERE persona_id = $1::uuid`, id).Scan(&leftRules); err != nil {
		t.Fatalf("统计规则失败: %v", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM persona_changes WHERE persona_id = $1::uuid`, id).Scan(&leftChanges); err != nil {
		t.Fatalf("统计变更记录失败: %v", err)
	}
	if leftRules != 0 || leftChanges != 0 {
		t.Errorf("删人格后应级联清掉规则(%d)与变更记录(%d)", leftRules, leftChanges)
	}
}

// 内置人格的 ID 形如 "builtin:lapwing"，**不是 uuid**。三条读路径都必须把它当"内存里那份"处理，
// 而不是拿去查库——否则 PG 会以 22P02（无效的 uuid 输入语法）拒绝。
//
// 这个 bug 内存实现测不出来（它根本不校验 uuid），只有真连上 PG 才暴露：
// 症状是日志里反复刷 22P02，而界面上只是"变更记录为空"，看不出是错误。
func TestPgStoreBuiltinReadPaths(t *testing.T) {
	s := openTestPgStore(t)

	builtins, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}
	if len(builtins) == 0 {
		t.Fatal("没有可用的内置人格")
	}
	id := builtins[0].Persona.ID

	// Snapshot 读的是"当前人格"，所以要真的切过去才能覆盖那条路径；
	// 切之前记下原值，跑完恢复——测试不该改变用户的实际使用状态
	before := s.Snapshot().ActiveID
	t.Cleanup(func() {
		if before != "" {
			_ = s.SetActivePersona(before)
		}
	})
	if err := s.SetActivePersona(id); err != nil {
		t.Fatalf("切到内置人格失败: %v", err)
	}

	snap := s.Snapshot()
	if snap.ActiveID != id {
		t.Errorf("当前人格应当是 %s，实际 %s", id, snap.ActiveID)
	}
	if len(snap.Rules) == 0 {
		t.Error("内置人格的规则应取注入时带进来的那份，不该为空")
	}
	if len(snap.RecentChanges) != 0 {
		t.Errorf("内置人格不该有变更记录，实际 %d 条", len(snap.RecentChanges))
	}

	rules, err := s.RulesOf(id)
	if err != nil {
		t.Fatalf("读内置人格的规则报错: %v", err)
	}
	if len(rules) == 0 {
		t.Error("内置人格的规则不该为空")
	}

	changes, err := s.ChangesOf(id)
	if err != nil {
		t.Fatalf("读内置人格的变更记录报错: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("内置人格不该有变更记录，实际 %d 条", len(changes))
	}
}

// 阶段0 的最后一项验收：pgvector 必须**真的启用**（扩展文件就位 ≠ 已启用）。
//
// 这条现在就验、而不是留到阶段4：阶段4 的 memories 表要用 vector 列 + HNSW 索引，
// 到那时才发现"扩展没装/版本不对"会白折腾一轮。顺带把三件套一起验掉——
// 向量类型、HNSW 索引、余弦距离算子，缺哪一块都是阶段4 的硬阻塞。
//
// 临时表用完即删（含失败路径），不在用户的库里留垃圾。
func TestPgVectorExtensionReady(t *testing.T) {
	s := openTestPgStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var version string
	if err := s.pool.QueryRow(ctx,
		`SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&version); err != nil {
		t.Fatalf("pgvector 未在库中启用（%v）；执行：psql -U postgres -d companion -c \"CREATE EXTENSION IF NOT EXISTS vector;\"", err)
	}
	t.Logf("pgvector 已启用，版本 %s", version)

	const probe = "_vec_probe_test"
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+probe)
	})

	if _, err := s.pool.Exec(ctx, `CREATE TABLE `+probe+` (id int, embedding vector(1536))`); err != nil {
		t.Fatalf("vector 类型不可用: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE INDEX ON `+probe+` USING hnsw (embedding vector_cosine_ops)`); err != nil {
		t.Fatalf("HNSW 索引不可用: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO `+probe+` VALUES (1, array_fill(0.1, ARRAY[1536])::vector)`); err != nil {
		t.Fatalf("写入向量失败: %v", err)
	}

	// 0.1 与 0.2 是共线的同向向量，余弦距离应当≈0（留浮点余量）
	var dist float64
	if err := s.pool.QueryRow(ctx,
		`SELECT embedding <=> array_fill(0.2, ARRAY[1536])::vector FROM `+probe).Scan(&dist); err != nil {
		t.Fatalf("余弦距离算子不可用: %v", err)
	}
	if dist < 0 || dist > 1e-6 {
		t.Errorf("同向向量的余弦距离应当≈0，实际 %v", dist)
	}
}

// 降级路径：没有可用连接池时必须退回内存实现并标记 StorageReady=false，
// 而不是让应用起不来。
//
// 注意职责已按 internal/db 的引入拆开了：**连接本身**由 db.Open 负责
// （连不上就报错，不自己降级），人格这边只负责"拿到 nil 池时退回内存"。
// 所以这两件事分在两处断言。
func TestOpenStoreFallsBackToMemory(t *testing.T) {
	builtins, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}

	store := OpenStore(nil, builtins)
	if _, ok := store.(*MemoryStore); !ok {
		t.Errorf("没有连接池时应当退回内存实现，实际 %T", store)
	}
	snap := store.Snapshot()
	if snap.StorageReady {
		t.Error("退回内存后应当标记 StorageReady=false")
	}
	if len(snap.Personas) == 0 {
		t.Error("退回内存后内置人格应当照常可用")
	}
	if snap.ActiveID == "" {
		t.Error("应当有默认生效人格")
	}
	t.Logf("→ %T，内置人格 %d 个，默认人格 %s", store, len(snap.Personas), snap.ActiveID)

	// 另一半：连接串为空或不可达时，db.Open 必须报错——main 据此决定降级
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, dsn := range []string{"", "postgres://nobody@127.0.0.1:1/nothing"} {
		pool, err := db.Open(ctx, dsn)
		if err == nil {
			pool.Close()
			t.Errorf("dsn=%q 时 db.Open 应当报错", dsn)
			continue
		}
		t.Logf("dsn=%q → 按预期报错: %v", dsn, err)
	}
}
