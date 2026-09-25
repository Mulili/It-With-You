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

-- 候选区（阶段4）：自动抽出来的「值得留存的行为规则」，等用户采纳才成为真规则。
--
-- 为什么不直接写进 persona_rules：规则改的是**行为方式**，一条错的会持续污染每一轮；
-- 而记忆抽错了只影响"她记错一件事"。风险等级不同，所以一个要过审、一个直接生效
-- （见 operation.md：显式为主、隐式落候选区、变更可见可回滚）。
--
-- 去重按 (persona_id, slot, value)：结算是每段会话都跑一次，同一件事会被反复抽到，
-- 不去重的话候选区很快被同一条刷屏。也正因为它幂等，结算重放时可以直接重写。
CREATE TABLE IF NOT EXISTS persona_rule_candidates (
    id         uuid PRIMARY KEY,
    persona_id uuid   NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    slot       text   NOT NULL,
    value      text   NOT NULL,
    -- 触发它的原话：与规则的 evidence 一样属敏感内容，导出时不含
    evidence   text   NOT NULL DEFAULT '',
    created_at bigint NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS persona_rule_candidates_uniq_idx
    ON persona_rule_candidates (persona_id, slot, value);

CREATE INDEX IF NOT EXISTS persona_rule_candidates_persona_idx
    ON persona_rule_candidates (persona_id, created_at DESC);

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

-- 会话内的片：长会话按体量切开，避免"边打游戏边聊一整天"导致上下文被截断。
--
-- 为什么需要它：会话边界由"话题聊完"给出，但连续闲聊可能一整天不结束。只靠会话做边界，
-- 超长会话要么被静默截断（中间内容既进不了上下文、也进不了长期记忆），要么顶爆上下文。
-- 所以加一层**不依赖语义**的兜底：按体量切片，且片边界永远落在用户发言之前
--（保证一个问答对不被从中间切开）。
CREATE TABLE IF NOT EXISTS session_chunks (
    id         uuid PRIMARY KEY,
    session_id uuid    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    -- 冗余 persona_id：与 messages 同理，让"某人格当前片"这类查询不必 JOIN
    persona_id uuid    NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    -- seq 是会话内的片序号，从 1 开始
    seq        integer NOT NULL,
    started_at bigint  NOT NULL,
    -- NULL = 还是当前片（每个会话最多一片）
    ended_at   bigint,
    -- 摘要由片收尾时一次生成（抽取式，见 operation.md），未生成为空
    summary    text    NOT NULL DEFAULT '',
    created_at bigint  NOT NULL,
    updated_at bigint  NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS session_chunks_seq_idx ON session_chunks (session_id, seq);
-- 部分索引：找"当前片"只扫未收尾的几行
CREATE INDEX IF NOT EXISTS session_chunks_open_idx ON session_chunks (persona_id) WHERE ended_at IS NULL;
-- 部分索引：收尾懒结算要扫"已关闭但还没摘要"的片
CREATE INDEX IF NOT EXISTS session_chunks_pending_idx ON session_chunks (ended_at)
    WHERE ended_at IS NOT NULL AND summary = '';

CREATE TABLE IF NOT EXISTS messages (
    id         uuid PRIMARY KEY,
    session_id uuid   NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    -- chunk_id 指向所属的片。**可空**是为了兼容 v3 之前写入的老行（那时还没有分片）；
    -- 新写入的一定非空。这里不写 NOT NULL，否则老库加不上这条约束
    chunk_id   uuid REFERENCES session_chunks (id) ON DELETE CASCADE,
    persona_id uuid   NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    -- role: system / user / assistant（system 不进这张表，见 app.go 的说明）
    role       text   NOT NULL,
    content    text   NOT NULL,
    -- status: ok / canceled（半截回复也留痕，但可区分）
    status     text   NOT NULL,
    created_at bigint NOT NULL
);

-- v3 增量迁移：给老库的 messages 补上 chunk_id。
--
-- **必须紧跟在建表语句之后、索引之前**：CREATE TABLE IF NOT EXISTS 对已存在的表是直接跳过，
-- 所以老库的 messages 不会凭空多出这一列；而下面的 messages_chunk_idx 引用了它——
-- 顺序反了就会报「字段 chunk_id 不存在」，且错误发生在启动建表阶段，看着跟索引毫无关系。
--
-- 老行补不上 chunk_id（那时还没有分片概念），所以这一列允许为空：
-- 它们仍能按 session_id 读出来，只是不参与"按片拼上下文"。
ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS chunk_id uuid REFERENCES session_chunks (id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS messages_session_idx ON messages (session_id, created_at);
-- 拼上下文是按片取的，这条索引直接服务它
CREATE INDEX IF NOT EXISTS messages_chunk_idx ON messages (chunk_id, created_at);

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

-- ---------- 增量迁移（幂等）----------
--
-- 为什么需要这一段：CREATE TABLE IF NOT EXISTS 只对**新库**有效——表已存在时它会直接跳过，
-- 所以"给老表加一列"必须用 ALTER。
--
-- 约定：**加列的 ALTER 写在对应表定义的正下方**（就近放置，避免"建表—建索引—补列"
-- 的顺序被跨文件段落打乱）；这里只放跨表的结构调整。
--
-- 注意：本段只处理"结构"，不搬数据；需要搬的会单独写明。

-- v3：会话级的 session_index 改成片级的 chunk_index（粒度变了，所以顺带换名）。
-- 它在 v2 里只建好了、从未写入过数据（写入逻辑在第 3 步才做），可以直接删。
-- 这条是幂等的：表不存在时无事发生，也不会误删 chunk_index。
DROP TABLE IF EXISTS session_index;
