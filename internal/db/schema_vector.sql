-- 需要 pgvector 扩展的表（阶段4 的记忆检索）
--
-- 为什么单独成一个文件：一条 `CREATE TABLE ... vector(1024)` 失败会让整个脚本失败。
-- 如果和核心表挤在一起，缺扩展时就会连人格都存不了——而人格根本不需要扩展。
-- 分开之后，缺扩展只让记忆功能停用，对话、人格、历史全部照常（见 internal/db 的 Open）。
--
-- 维度 1024 与 BGE-M3 一致，**固化在列定义里**：
-- 换嵌入模型 = 改这一列 + 重算所有向量，不是改配置就能切。
-- 启动时 llm.Embedder 的 Verify 会拿实际返回的维度与配置比对，不一致直接报在日志里。

-- ---------- 会话索引：摘要做指针 ----------
--
-- 两段式的第一段：向量库里存的是"摘要 + session_id"，命中后回 messages 表取原文。
-- 于是"回忆"看到的是真实对话，不是模型编的。

CREATE TABLE IF NOT EXISTS session_index (
    -- 与 sessions 一一对应，所以直接用 session_id 做主键：天然防重复
    session_id uuid PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    -- 冗余 persona_id 是有意的反范式：检索要按人格过滤，
    -- 若靠 JOIN sessions 过滤，过滤会发生在 HNSW 取完 Top-N 之后，
    -- 可能出现"取了 10 条、过滤后只剩 1 条"
    persona_id uuid   NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    summary    text   NOT NULL,
    embedding  vector(1024) NOT NULL,
    created_at bigint NOT NULL
);

CREATE INDEX IF NOT EXISTS session_index_embedding_idx ON session_index USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS session_index_persona_idx   ON session_index (persona_id, created_at DESC);

-- ---------- 记忆：提炼出的事实 ----------
--
-- 与 session_index 分开而不是共表：两者用法根本不同（一个直接注入、一个只是指针），
-- 混在一起会互相淹没——摘要天然更长、信息更密，检索时容易把精炼的事实挤下去。

CREATE TABLE IF NOT EXISTS memories (
    id         uuid PRIMARY KEY,
    -- NULL = 关于用户的公共事实（任何人格都该知道，用户不必跟三个人格各说一遍"我喜欢猫"）；
    -- 非空 = 这段经历只属于这个人格。查询条件：persona_id IS NULL OR persona_id = $1
    persona_id uuid REFERENCES personas (id) ON DELETE CASCADE,
    -- content 写成**一句可直接注入提示词的话**，不是原始对话片段
    content    text     NOT NULL,
    -- kind: fact / preference / event / promise
    kind       text     NOT NULL,
    -- 1..5，参与排序：没有它「用户离婚了」与「今天吃了面」就是平等候选
    importance smallint NOT NULL DEFAULT 3,
    embedding  vector(1024) NOT NULL,
    -- 抽取这条记忆时的原话，便于回溯（与 persona_changes 的 evidence 同一套路）
    evidence   text     NOT NULL DEFAULT '',
    -- 非空 = 未完结话题，到点可主动问一句（4.6 主动发言用）；
    -- **问过一次后置空**，于是"只回访一次"由数据本身保证，不需要额外的状态字段
    follow_up_at bigint,
    -- 最近一次被检索注入的时间：抑制"反复提同一件事"
    last_recalled_at bigint,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL
);

CREATE INDEX IF NOT EXISTS memories_embedding_idx ON memories USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS memories_persona_idx   ON memories (persona_id, created_at DESC);
-- 部分索引：只索引"待回访"的那几行
CREATE INDEX IF NOT EXISTS memories_follow_up_idx ON memories (follow_up_at) WHERE follow_up_at IS NOT NULL;
