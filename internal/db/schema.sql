-- 整个库的表结构（阶段4 v2）
--
-- 时间统一用 bigint（Unix 毫秒）：与应用内的时间口径一致，避免 Go 与 PG 之间来回换算。
--
-- 本文件由 internal/db 在连接成功后执行，语句都是幂等的（IF NOT EXISTS），
-- 所以重复启动、升级都不需要额外处理。
--
-- 需要 pgvector 扩展的表**不在这里**，放在 schema_vector.sql——
-- 缺扩展时只让那部分失败，不连累人格与历史。

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

-- ---------- 阶段4：会话 ----------
--
-- 会话是历史的组织单位：历史落库后按会话分组，而"只发当前会话的消息"天然给出了
-- 上下文边界（替代了原先"只发最近 N 轮"的截断方案）。

CREATE TABLE IF NOT EXISTS sessions (
    id         uuid PRIMARY KEY,
    -- NOT NULL：会话必然属于某个具体人格（你是在跟某一个人聊）。
    -- 与 memories 的两分法（可空）形成对照：**经历私有，事实公共**
    persona_id uuid   NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    -- title / summary 由会话收尾时一次调用生成（懒结算）
    title      text   NOT NULL DEFAULT '',
    summary    text   NOT NULL DEFAULT '',
    started_at bigint NOT NULL,
    -- NULL = 还没收尾（懒结算要扫这个状态）
    ended_at   bigint,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_persona_idx ON sessions (persona_id, started_at DESC);
-- 部分索引：收尾时只扫"还没结束"的会话，比全列索引小得多
CREATE INDEX IF NOT EXISTS sessions_open_idx ON sessions (ended_at) WHERE ended_at IS NULL;

CREATE TABLE IF NOT EXISTS messages (
    id         uuid PRIMARY KEY,
    session_id uuid   NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    persona_id uuid   NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    -- role: system / user / assistant（system 不进这张表，见 app.go 的说明）
    role       text   NOT NULL,
    content    text   NOT NULL,
    -- status: ok / canceled（半截回复也留痕，但可区分）
    status     text   NOT NULL,
    created_at bigint NOT NULL
);

CREATE INDEX IF NOT EXISTS messages_session_idx ON messages (session_id, created_at);

-- ---------- 设置与版本 ----------

-- 杂项设置（当前生效的人格、思考开关等）
CREATE TABLE IF NOT EXISTS app_settings (
    key   text PRIMARY KEY,
    value text NOT NULL
);

-- 表结构版本：程序启动时比对，据此判断"是否需要迁移"或"库比程序新"
CREATE TABLE IF NOT EXISTS schema_version (
    version    integer PRIMARY KEY,
    applied_at bigint  NOT NULL
);
