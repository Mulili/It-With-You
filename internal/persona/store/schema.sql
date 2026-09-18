-- 人格系统的表结构（阶段3 v1）
--
-- 时间统一用 bigint（Unix 毫秒）：与应用内 Persona/PersonaRule 的时间口径一致，
-- 避免 Go 与 PG 之间来回换算（阶段4 若要做时间范围查询，毫秒也够用）。
--
-- 本文件由 PgStore 在连接成功后执行，语句都是幂等的（IF NOT EXISTS），
-- 所以重复启动、升级都不需要额外处理。

CREATE TABLE IF NOT EXISTS personas (
    id          uuid PRIMARY KEY,
    name        text   NOT NULL,
    seed_text   text   NOT NULL DEFAULT '',
    -- origin: user / imported（内置人格不落库，随 exe 分发）
    origin      text   NOT NULL,
    avatar_path text   NOT NULL DEFAULT '',
    created_at  bigint NOT NULL,
    updated_at  bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS persona_rules (
    id         uuid PRIMARY KEY,
    -- 人格被删时，它的规则一起走
    persona_id uuid    NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    -- slot 必须是代码里的规范槽位；这里不做 CHECK，因为槽位清单会演进，
    -- 用数据库约束去跟代码里的清单对齐只会制造迁移负担，校验在写入路径上做
    slot       text    NOT NULL,
    value      text    NOT NULL,
    -- source: explicit / inferred / manual
    source     text    NOT NULL,
    -- evidence 是触发这条规则的原始话（敏感内容，导出时不带）
    evidence   text    NOT NULL DEFAULT '',
    -- tier: core / recent / archived；kind: stable / volatile
    tier       text    NOT NULL,
    kind       text    NOT NULL,
    priority   integer NOT NULL DEFAULT 0,
    enabled    boolean NOT NULL DEFAULT true,
    created_at bigint  NOT NULL,
    updated_at bigint  NOT NULL
);

CREATE INDEX IF NOT EXISTS persona_rules_persona_idx ON persona_rules (persona_id, slot);

CREATE TABLE IF NOT EXISTS persona_changes (
    id         uuid PRIMARY KEY,
    persona_id uuid    NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    -- rule_id 故意不加外键：规则可能已被删除，而变更日志要留痕（审计价值就在这儿）
    rule_id    uuid,
    -- action: create / update / delete / enable / disable
    action     text    NOT NULL,
    field      text    NOT NULL DEFAULT '',
    old_value  text    NOT NULL DEFAULT '',
    new_value  text    NOT NULL DEFAULT '',
    source     text    NOT NULL,
    evidence   text    NOT NULL DEFAULT '',
    created_at bigint  NOT NULL
);

CREATE INDEX IF NOT EXISTS persona_changes_persona_idx ON persona_changes (persona_id, created_at DESC);

-- 杂项设置（当前生效的人格、阶段4 的嵌入模型选择等）
CREATE TABLE IF NOT EXISTS app_settings (
    key   text PRIMARY KEY,
    value text NOT NULL
);

-- 表结构版本：程序启动时比对，据此判断"是否需要迁移"或"库比程序新"
CREATE TABLE IF NOT EXISTS schema_version (
    version    integer PRIMARY KEY,
    applied_at bigint  NOT NULL
);
