It-With-You · 增量式开发方案
文档版本：v1.3
创建日期：2026-09-14
修订记录：v1.1（2026-09-15）— 统一嵌入维度口径，修正 `VECTOR(1536)` 与 BGE-M3 的矛盾（见阶段4「关键决策」）
          v1.2（2026-09-16）— 移除「快速开始」小节：该节描述的一键脚本 run.bat 并未随仓库提供
          v1.3（2026-09-16）— 新增「🐞 已知问题」小节：记录阶段2.5 菜单加高暴露的两个 Wails 窗口坐标缺陷（其一已在真机复现）
适用项目：基于 Go + Wails + LLM + RAG 的桌面陪伴型 AI Agent
文档说明：本文档为增量式开发路线图，每个阶段均产出可运行、可验证的版本，逐步叠加功能模块，避免推倒重来。

📐 总体增量路线图
text
阶段0  环境准备
  ↓
阶段1  最小框架：窗口 + 托盘 + 文字气泡
  ↓
阶段2  接入 LLM：文字对话（流式）
  ↓
阶段3  人格系统：System Prompt + 人格配置
  ↓
阶段4  RAG 记忆：PostgreSQL + pgvector 存用户习惯
  ↓
阶段5  语音对话：ASR + TTS + 打断
  ↓
阶段6  虚拟形象：Live2D/VRM + 口型同步
  ↓
阶段7  优化与打包：低负载 + 开机自启 + 发布
🧱 阶段0：环境准备（1～2天）
目标：把所有工具装好，跑通 Hello World。

任务	产出
安装 Go 1.22+	go version 正常
安装 Node.js 18+	node -v 正常
安装 Wails CLI	wails doctor 全绿
安装 PostgreSQL 18 + pgvector	CREATE EXTENSION vector; 成功
安装 Git、VS Code	可正常开发
验收：wails init -n desktop-companion -t vue 创建项目，wails dev 能弹出窗口。

风险：Wails 在 Windows 上依赖 WebView2，Win11 自带，Win10 需手动安装。

🪟 阶段1：最小框架（3～5天）
目标：一个能常驻桌面的透明窗口 + 托盘 + 文字气泡，不接任何 AI。

技术任务
Wails 窗口配置：透明、无边框、置顶、可拖动

系统托盘：显示/隐藏、退出

前端：一个简单的文字气泡组件（先写死内容）

Go 后端：暴露一个 Say(text string) 方法，前端调用后显示气泡

目录结构建议
text
desktop-companion/
├── main.go              # Wails 入口
├── app.go               # 应用逻辑
├── internal/
│   ├── ui/              # 窗口、托盘
│   ├── llm/             # 预留：LLM 接口
│   ├── memory/          # 预留：RAG 接口
│   └── voice/           # 预留：语音接口
├── frontend/
│   ├── src/
│   │   ├── components/
│   │   │   └── Bubble.vue
│   │   └── App.vue
│   └── index.html
└── go.mod
验收标准
双击运行，桌面出现一个透明小窗口

托盘图标可右键，能显示/隐藏窗口

调用 Say("你好")，气泡显示文字

关键决策：窗口用透明 + 点击穿透还是可交互？建议先做可交互，后续再加穿透开关。

💬 阶段2：接入 LLM（3～5天）
目标：能和 Agent 进行文字对话，流式输出。

技术任务
定义 LLMProvider 接口（为后续切换模型预留）

实现 OpenAI 兼容 Provider（DeepSeek / OpenAI 均可）

前端：底部输入框 + 气泡内流式渲染（完整历史留给阶段2.5 的菜单查看）

Go 后端：SSE 流式转发到前端

核心接口设计
go
// internal/llm/provider.go
type Provider interface {
    ChatStream(ctx context.Context, messages []Message) (<-chan Chunk, error)
}

type Message struct {
    Role    string // system / user / assistant
    Content string
}
验收标准
输入文字，Agent 流式回复

关闭窗口后，对话历史不丢失（本阶段用内存暂存；进程重启即丢，持久化留到阶段3/4）

关键决策：默认 Provider 选 DeepSeek（OpenAI 兼容协议、国内直连无需代理，且本机系统代理当前是关闭的）。
API Key 只从环境变量读，不写进代码、不进仓库。

风险：流式输出在 Wails 中需要用 Events 机制，提前看 Wails 的 runtime.EventsEmit。

🪟 阶段2.5：窗口形态与历史视图（2～3天）
目标：把固定的 340×460 小窗变成可拖拽缩放的窗口，并把历史对话放进一个菜单里查看。

为什么单独成阶段：阶段2 的未知量集中在 LLM 链路（SSE 解析、Wails 事件流、请求取消），UI 改造是已知的体力活。
混在一起做，出错时分不清是链路错还是布局错。先跑通链路，再动形态。

技术任务
打开窗口缩放：main.go 的 DisableResize 由 true 改成 false（原理见「关键决策」）

补 MinWidth / MinHeight（建议 280×360），防止缩到布局塌陷

前端：拖拽区加菜单键（☰），点开浮层列出历史对话

把「测试 Say / 隐藏 / 退出」这类低频按钮收进菜单，给底部输入框腾空间

验收标准
鼠标移到窗口边缘（约 6px）时光标变成 ↖↘ 一类形状，按住可拖拽改变大小

缩到 MinWidth / MinHeight 时布局不塌，输入框与桌宠仍可见

点菜单键能看到历史对话；隐藏窗口再显示后记录仍在（承接阶段2 验收项2）

关键决策：拖拽缩放为什么只需要改一个配置项
Wails 在无边框窗口下自带边缘缩放：DOM ready 时若 Frameless && !DisableResize，会自动置
window.wails.flags.enableResize = true；前端运行时检测鼠标是否落在窗口边缘 6px 内，命中就把光标改成
*-resize，mousedown 时发 resize:se-resize 一类消息，Go 侧转成 WM_NCLBUTTONDOWN 走系统缩放。
这条链路本来就在，是阶段1 为了先稳住把 DisableResize 设成了 true。

菜单用前端自绘浮层，不用 options.App.Menu：原生菜单需要一条不透明的菜单栏，与透明桌宠的观感冲突，
还会占掉本就紧张的垂直空间。

注意：无边框窗口没有可见边框，可缩放的 6px 是透明的，用户只能靠光标变化发现，建议右下角再画一个视觉抓手。

风险：缩放与透明合成叠加时的实际表现尚未真机验证（边缘重绘、残影）。若出现异常，先临时关掉
WindowIsTranslucent 做对比，隔离变量后再决定是否降级为「前端抓手 + runtime.WindowSetSize」自绘缩放。

已知问题：菜单临时加高已在真机上实测出一个缺陷（展开时窗口下边缘下沉），根因是 Wails 尺寸 API 与位置 API
的量纲不一致，详见下方「🐞 已知问题」。当前决定：先记录不修，等阶段6 需要钉住桌宠屏幕位置时一并处理。

🎭 阶段3：人格系统（2～3天）
目标：用户可配置 Agent 人格，Agent 按人格说话。

技术任务
人格配置结构体：名称、性格、说话风格、示例对话

人格存 SQLite（本地轻量）或 PostgreSQL

拼装 System Prompt：人格配置 + 对话历史

前端：人格设置页面（简单表单）

人格配置示例
go
type Persona struct {
    Name        string   `json:"name"`
    Traits      []string `json:"traits"`       // 性格标签
    Style       string   `json:"style"`        // 说话风格
    Examples    []Dialog `json:"examples"`     // 示例对话
    AvatarPath  string   `json:"avatar_path"`  // 虚拟形象路径（预留）
}
System Prompt 模板
text
你是 {Name}，性格 {Traits}。
说话风格：{Style}。
参考以下对话示例：
{Examples}
请始终保持这个人格，不要跳出角色。
验收标准
用户能创建/切换人格

切换人格后，Agent 说话风格明显不同

关键点：人格不要塞进 RAG。RAG 留给长期记忆。

🧠 阶段4：RAG 记忆（5～7天）
目标：Agent 能记住用户习惯/爱好，并在对话中自然引用。

技术任务
PostgreSQL + pgvector 建表

嵌入模型接入（OpenAI Embedding 或 BGE-M3）—— 维度口径见下方「关键决策」

对话结束后，用 LLM 抽取“值得记住的事实”

存入向量库，下次对话时检索并注入上下文

数据库表设计
sql
-- ⚠️ 维度必须先定嵌入模型：1536 = OpenAI text-embedding-3-small / ada-002；1024 = BGE-M3
--    VECTOR(n) 的 n 在建表时固化，HNSW 索引同样依赖它，换模型 = 改表 + 重建索引 + 重算全部历史向量
CREATE TABLE memories (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    content TEXT NOT NULL,
    embedding VECTOR(:embedding_dim),   -- 由嵌入模型决定，不要写死，见「关键决策」
    category TEXT,        -- fact / preference / event
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX ON memories USING hnsw (embedding vector_cosine_ops);
记忆抽取 Prompt 示例
text
从以下对话中提取值得长期记住的用户信息（爱好、习惯、事实）。
只输出 JSON 数组，每项包含 content 和 category。
没有则输出 []。

对话：
{conversation}
检索逻辑
go
// 1. 用户提问 → 生成 embedding
// 2. pgvector 检索 top 5 相关记忆
// 3. 拼接到 System Prompt 或作为上下文注入
关键决策（阶段4 动手前必须锁定）
嵌入模型二选一，维度随之确定，二者不可混用：

OpenAI text-embedding-3-small / ada-002 → 1536 维

BGE-M3（本地 / 自托管）→ 1024 维

VECTOR(n) 的类型定义在建表时固化，HNSW 索引也与其绑定；后期更换嵌入模型需要改表结构 + 重建索引 + 重算全部历史记忆向量，属于高成本迁移。

建议：把 embedding_dim 抽成配置项，建表语句用参数拼接（如上 :embedding_dim），避免把维度写死在 SQL 里。

注意：已建好的 vector 列无法直接修改维度，迁移时需新建列/新表再回填。

验收标准
用户说“我喜欢喝美式”，下次对话 Agent 能提起

检索延迟 < 200ms

风险：记忆抽取质量依赖 LLM；建议加人工审核开关（可选）。

🎙️ 阶段5：语音对话（5～7天）
目标：能语音唤醒、语音输入、语音回复、可打断。

技术任务
音频采集：malgo 或 portaudio

VAD：Silero VAD（本地轻量）

ASR：Whisper.cpp（本地）或 OpenAI Whisper API（云）

TTS：Edge TTS（免费）或 GPT-SoVITS（本地）

打断逻辑：检测到用户说话 → 停止 TTS

数据流
text
麦克风 → VAD → ASR → LLM → TTS → 扬声器
                ↑
            用户说话时打断
验收标准
说“你好”，Agent 语音回复

说话时能打断 Agent

关键决策：ASR/TTS 优先云 API（低延迟、低负载），本地作为隐私选项。

🎨 阶段6：虚拟形象（7～10天）
目标：Agent 有 Live2D/VRM 形象，能口型同步、表情变化。

技术任务
前端集成 Live2D Cubism SDK 或 Three.js + VRM

口型同步：根据 TTS 音频驱动嘴型

表情：根据 LLM 输出情绪标签切换表情

窗口透明、置顶、点击穿透

实现路径
先用现成 Live2D 模型跑通渲染

接 TTS 音频做口型同步

LLM 输出加情绪标签（如 [happy]），前端切换表情

验收标准
Agent 形象显示在桌面

说话时嘴型动，情绪变化时表情变

风险：Live2D SDK 集成到 WebView 有一定工作量，建议先做静态图 + 口型，再升级 Live2D。

⚡ 阶段7：优化与发布（5～7天）
目标：低负载、开机自启、可打包分发。

技术任务
内存优化：复用对象、减少 GC

CPU 优化：语音模型按需加载

开机自启：注册表 / 启动文件夹

打包：wails build 生成 exe

自动更新：预留接口

验收标准
空闲时内存 < 200MB，CPU < 5%

双击 exe 可运行，开机自启

安装包可分发给用户

📊 各阶段优先级与依赖关系
阶段	优先级	依赖	预计工期
0 环境	P0	无	1～2天
1 最小框架	P0	阶段0	3～5天
2 LLM 对话	P0	阶段1	3～5天
2.5 窗口形态与历史视图	P1	阶段2	2～3天
3 人格系统	P1	阶段2	2～3天
4 RAG 记忆	P1	阶段3	5～7天
5 语音	P2	阶段4	5～7天
6 虚拟形象	P2	阶段5	7～10天
7 优化发布	P3	阶段6	5～7天
MVP（最小可行产品）：阶段0 + 1 + 2 + 3，约 2周，即可拥有一个能文字对话、有人格的桌面 Agent。

阶段2.5 是体验增强、不在关键路径上：跳过它，阶段3 及之后照常推进。

🐞 已知问题

以下两项均已定位到根因，记录在此以便将来复现与处理。两者同源：Wails v2 的尺寸 API 以**逻辑像素**为语义
（`Size()` 除以缩放比、`SetSize()` 乘回去），而位置 API **全程用物理像素**、不做任何换算。
同一段代码里把「高度增量」和「位移增量」相加减，只有在 100% 缩放下才成立。

问题1：菜单展开时窗口下边缘下沉（已真机复现）

现象：点开 ☰ 临时加高窗口时，「保持下边缘不动」的设计意图只做到一部分——上方只长出应增长度的约 2/3，
剩下的落到下方，表现为窗口下边缘外扩。

实测环境：2560×1600、显示器缩放 150%，窗口逻辑尺寸 340×460，菜单增量 260。

| 状态 | 窗口位置 | 窗口尺寸 | 下边缘 |
|---|---|---|---|
| 菜单关闭 | (711, 366) | 340×460 | 826 |
| 菜单展开 | (711, 193) | 340×720 | 913 |
| 差值 | y −173 | +260 | +87（下沉） |

上表为 DPI 不感知进程读到的虚拟化坐标，等同于 CSS / 逻辑像素；乘 1.5 得物理像素，即下边缘实际下沉 130 物理像素。

根因：高度增量 260 逻辑像素被换算成 390 物理像素，而位置上移按传入的 260 物理像素执行，差的 130 物理像素全部落在下边缘上。

影响：平时看不出来——透明窗口没有可见边框、缺少参照物，且窗口离工作区下沿还有 241 个逻辑像素的余量。
一旦把窗口拖到贴屏幕底部（余量小于 87 逻辑像素），面板底部就会跑到屏幕外；而历史列表滚动到底时
最后一条会停在容器底边上，那条底边在屏幕外——**最后一条既看不到也滚不出来**。桌宠贴底恰恰是最典型的摆法。

问题2：位置 API 的坐标系偏置导致累积漂移（本机未复现，触发条件不满足）

现象：`WindowGetPosition` 返回绝对屏幕坐标，而 `WindowSetPosition` 会把入参**再叠加一次**窗口所在
显示器的工作区原点（Wails 内部 `SetPos` 用的是 `RcWork.Left/Top + x/y`）。于是「读位置 → 加减差值 → 写回」
这个模式每写一次就注入一次偏置，且不会自愈：

- 展开：请求 y−260，实际落到 y−260+W
- 收起：以带偏置的位置为基准再加回 260，实际落到 y+2W

即每开合一次就向偏置方向漂移 2W，反复开合会一路走远。

触发条件：所在显示器的工作区原点不为 (0,0)——任务栏放在屏幕左侧或顶部，或窗口位于副屏
（副屏的工作区原点可能是 1920 等，第一次展开就会跳到屏幕之外）。

本机实测：工作区原点为 (0,0)，连续开合 11 次后窗口精确回到 (711, 366) / 340×460，零漂移——偏置源不存在，问题处于休眠状态。

当前决定：阶段2.5 的验收标准不涉及窗口底边位置，这两项暂不修改，仅记录。
等阶段6（虚拟形象要把桌宠钉在屏幕固定位置）时一并处理。届时可选的三种改法：

1. 保留「向上扩展」，把位置增量按缩放比换算（用 `runtime.ScreenGetAll` 的 `Size` 与 `PhysicalSize` 之比推缩放比）。
   能修掉问题1，问题2 仍在。
2. 绕开位置 API，改为「向下扩展」（只调 `WindowSetSize`）。两个问题一起消失，代价是底边不再锚定，且贴底时会提前出屏。
3. 自己 syscall `GetMonitorInfo` + `GetDpiForWindow` 做完整补偿。两个问题都能修，代价是引入平台相关代码。

复现与验证方法：用 DPI 不感知的进程对窗口做 `GetWindowRect` 采样，即可看到上面这组数字（读数是虚拟化值，
乘缩放比得物理值）。判断缩放比必须用 `GetDpiForWindow(该窗口)`——`GetDpiForSystem` / `GetDpiForMonitor`
对不感知的进程会返回 96，据此判断会得出「100% 缩放」的错误结论。

升级 Wails 后需重测：两项都源于 Wails 的实现语义，上游若修正，本节结论与规避方案应同步调整。

⚠️ 全局风险与应对
风险	应对
Wails 窗口透明/穿透在 Windows 上不稳定	提前做技术验证，必要时用 Win32 API
Live2D 集成复杂	先用静态图 + 口型，后续升级
语音延迟高	优先云 API，本地模型作为可选
记忆检索不准	混合检索 + 重排，持续调优
低负载目标难达成	Go 核心 + 云端 AI，重任务放子进程
💎 建议启动顺序
先做阶段1 + 2，跑通“窗口 + LLM 对话”，这是最小闭环。

阶段2 跑通后可以顺手做阶段2.5（窗口缩放 + 菜单看历史）：纯体验增强，不做也不阻塞后面。

再做阶段3，加入人格，体感立刻提升。

阶段4 是分水岭，决定项目是否有“长期陪伴”价值。

阶段5、6 按兴趣和资源决定，语音和形象是加分项，不是必需。
