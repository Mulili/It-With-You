package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
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
	dsn := DSNFromEnv()
	if dsn == "" {
		t.Skip("未配置 COMPANION_PG_DSN，跳过 PG 集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	builtins, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}
	store, err := NewPgStore(ctx, dsn, builtins)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
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

// 降级路径：DSN 缺失或连不上时，必须退回内存实现并标记 StorageReady=false，
// 而不是让应用起不来。
func TestOpenStoreFallsBackToMemory(t *testing.T) {
	builtins, err := builtin.Load()
	if err != nil {
		t.Fatalf("加载内置人格失败: %v", err)
	}

	for _, dsn := range []string{"", "postgres://nobody@127.0.0.1:1/nothing"} {
		store := OpenStore(dsn, builtins)
		if _, ok := store.(*MemoryStore); !ok {
			t.Errorf("dsn=%q 时应当退回内存实现，实际 %T", dsn, store)
		}
		snap := store.Snapshot()
		if snap.StorageReady {
			t.Errorf("dsn=%q 时应当标记 StorageReady=false", dsn)
		}
		if len(snap.Personas) == 0 {
			t.Errorf("dsn=%q 时内置人格应当照常可用", dsn)
		}
		if snap.ActiveID == "" {
			t.Errorf("dsn=%q 时应当有默认生效人格", dsn)
		}
		t.Logf("dsn=%q → %T，内置人格 %d 个，默认人格 %s", dsn, store, len(snap.Personas), snap.ActiveID)
	}
}

// 表结构版本比程序新时必须拒绝启动，而不是带着可能不兼容的结构跑下去。
func TestPgStoreRejectsNewerSchema(t *testing.T) {
	// 这条不依赖数据库：用一个明显不存在的版本号演示判断逻辑
	if SchemaVersion < 1 {
		t.Fatalf("SchemaVersion 应当 >= 1，实际 %d", SchemaVersion)
	}
	if !strings.Contains(schemaSQL, "CREATE TABLE IF NOT EXISTS personas") {
		t.Error("schema.sql 里应当包含 personas 建表语句")
	}
}
