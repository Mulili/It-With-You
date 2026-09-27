-- 需要 pgvector 扩展的表（阶段4 的记忆检索）
--
-- 为什么单独成一个文件：一条 `CREATE TABLE ... vector(1024)` 失败会让整个脚本失败。
-- 如果和核心表挤在一起，缺扩展时就会连人格都存不了——而人格根本不需要扩展。
-- 分开之后，缺扩展只让记忆功能停用，对话、人格、历史全部照常（见 internal/db 的 Open）。
--
-- 维度 1024 与 BGE-M3 一致，**固化在列定义里**：
-- 换嵌入模型 = 改这一列 + 重算所有向量，不是改配置就能切。
-- 启动时 llm.Embedder 的 Verify 会拿实际返回的维度与配置比对，不一致直接报在日志里。

-- ---------- 片索引：摘要做指针（v3 起按**片**建，不再按会话）----------
--
-- 两段式的第一段：向量库里存的是"片摘要 + chunk_id"，命中后回 messages 取该片的原文。
--
-- 粒度为什么是"片"而不是"会话"：一次会话可能聊好几个小时（尤其边玩边聊），
-- 整段会话压成一条索引，摘要必然糊成"我们聊了很多东西"，检索时什么都匹配不上。
-- 按片（约 2 万字符量级）建，每条索引对应的主题才足够具体。

CREATE TABLE IF NOT EXISTS chunk_index (
    -- 与 session_chunks 一一对应，所以直接用 chunk_id 做主键：天然防重复
    chunk_id   uuid PRIMARY KEY REFERENCES session_chunks (id) ON DELETE CASCADE,
    -- 冗余 session_id / persona_id 是有意的反范式：
    -- 前者用于"命中片后回溯它属于哪次对话"（片作为组归属于会话），
    -- 后者用于按人格过滤——若靠 JOIN 过滤，过滤会发生在 HNSW 取完 Top-N 之后，
    -- 可能出现"取了 10 条、过滤后只剩 1 条"
    session_id uuid   NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    persona_id uuid   NOT NULL REFERENCES personas (id) ON DELETE CASCADE,
    summary    text   NOT NULL,
    embedding  vector(1024) NOT NULL,
    -- 最近一次被注入时所在的会话，语义与 memories.last_recalled_session 完全一样：
    -- 同一段对话里提过一次就不再提。
    --
    -- 这一列在往事侧尤其要紧：**它和"排除当前会话"是两件事**。
    -- 后者只挡住"本会话自己的片"，而一条几天前的往事属于**别的**会话，
    -- 于是每轮检索都会命中它、每轮都注入一遍——用户的感觉就是"她每一句都回扣同一件事"。
    last_recalled_session uuid,
    -- 在 last_recalled_session 那段对话里，它已经被注入过几次。
    --
    -- 为什么是计数而不是一个布尔"提过没有"：提过之后还有**梯度**——
    -- 第一次她可以自然提起；第二次该提醒她"这件事你提过了，除非他主动问起"；
    -- 再之后就不再返回（避免反复刷屏）。零值表示"这段对话里没提过"。
    recall_count smallint NOT NULL DEFAULT 0,
    created_at bigint NOT NULL
);

CREATE INDEX IF NOT EXISTS chunk_index_embedding_idx ON chunk_index USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS chunk_index_persona_idx   ON chunk_index (persona_id, created_at DESC);
CREATE INDEX IF NOT EXISTS chunk_index_session_idx   ON chunk_index (session_id);

-- ---------- 记忆：提炼出的事实 ----------
--
-- 与 chunk_index 分开而不是共表：两者用法根本不同（一个直接注入、一个只是指针），
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
    -- 最近一次被检索注入的时间。
    -- ⚠️ 抑制"反复提同一件事"靠的不是它，而是下面那列 session——"隔了多久算久"是个要拍的数，
    -- 而"同一段对话里提过没有"是确定的事实。它留着是因为"什么时候提过"本身有价值
    -- （将来做遗忘曲线 / 相关性衰减要用）
    last_recalled_at bigint,
    -- 最近一次被注入时所在的**会话**。
    --
    -- 真正要防的不是"隔了多久"，而是**同一段对话里反复提**：用户会明确感觉到"她怎么老提这个"。
    -- 用会话做判据就不必拍一个冷却时长——同一段对话里提过就不再提；
    -- 换个新会话仍会提一次，那本来就是期望行为（"上次我们聊过…"）。
    last_recalled_session uuid,
    -- 语义与 memories.recall_count 完全一样，见那里的说明
    recall_count smallint NOT NULL DEFAULT 0,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL
);

-- 幂等建表管不到"给**已有**的表加列"，所以这里补两条 ALTER。
-- 新库上它们没有副作用；老库靠它们把列补上——在本项目里，"迁移"的全部形态就是这个
-- （见 internal/db 的 checkVersion：升级 = 重跑一遍建表脚本 + 补版本号）。
ALTER TABLE memories    ADD COLUMN IF NOT EXISTS last_recalled_session uuid;
ALTER TABLE chunk_index ADD COLUMN IF NOT EXISTS last_recalled_session uuid;
ALTER TABLE memories    ADD COLUMN IF NOT EXISTS recall_count smallint NOT NULL DEFAULT 0;
ALTER TABLE chunk_index ADD COLUMN IF NOT EXISTS recall_count smallint NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS memories_embedding_idx ON memories USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS memories_persona_idx   ON memories (persona_id, created_at DESC);
-- 部分索引：只索引"待回访"的那几行
CREATE INDEX IF NOT EXISTS memories_follow_up_idx ON memories (follow_up_at) WHERE follow_up_at IS NOT NULL;
