// 本文件是嵌入的**进程内实现**：直接跑本机 ONNX 模型，不依赖 Ollama 之类的常驻服务。
//
// 为什么换掉 HTTP 那条路（详见 operation.md 的「嵌入运行时替换」）：
//   - Ollama 在本项目里**只**服务嵌入，却要求用户自己装、自己启动、自己保活；
//   - 它常驻内存（用户实测 16GB 的 20%），而嵌入模型本身只需要几百 MB 一次性加载；
//   - 它还带来一串复杂度：keep_alive、原生端点探测、3 秒冷启动超时、失败时的降级分支。
//
// 代价是 **cgo**：ONNX Runtime 的 Go binding 是 cgo 的，所以构建期需要 gcc
// （本机用 w64devkit；运行与分发不需要）。这一笔账在阶段的 TTS 方案里已经认过，
// 两个功能共用同一套基础设施。
//
// 与 OpenAIEmbedder 的关系：**保留两条实现**，由配置选后端。ONNX 是默认；
// 它需要模型文件，缺了就必须在启动时明确拦住（见 NewONNXEmbedder 的报错），
// 绝不允许"静默地没有记忆"。
package llm

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"sync"

	"agent-for-you-love/internal/llm/tokenizer"

	ort "github.com/yalue/onnxruntime_go"
)

// 图的两个输入名（dump 过：只有这两个，xlm-roberta 没有 token_type_ids）。
// 输出只要 dense_vecs 一个 —— 图的另两个头（sparse / ColBERT）**不需要声明也能跑**，
// 实测"只声明 dense"与"三个都声明"的结果逐位相同，而省下的 colbert 张量
// 在批量场景是 (batch × seq × 1024 × 4B) 的量级（几百条短文本时就是几十 MB）。
const (
	onnxInputIDs       = "input_ids"
	onnxAttentionMask  = "attention_mask"
	onnxOutputDenseVec = "dense_vecs"
)

// DefaultMaxTokens 是单条文本送进模型的 token 上限。
//
// 取 8192 是模型的 `max_position_embeddings`（8194）减去两个特殊 token，也就是**不截断**。
// 默认设成"不截断"而不是常见的 512，理由是：截断会**静默丢掉信息**，
// 而我们的片摘要动辄上千字，512 会让它只嵌到一小半。
const DefaultMaxTokens = 8192

// ONNXEmbedConfig 是本机嵌入需要的三样东西 + 两个可调项。
//
// 三个路径都**必须显式给**：它们从哪来（设置项 / exe 同目录 / 用户缓存目录）是 App 侧的事，
// 这一层不替用户猜位置——猜错的代价是"记忆整体停用"。
type ONNXEmbedConfig struct {
	// RuntimePath 是 onnxruntime.dll（Windows）/ .so / .dylib
	RuntimePath string
	// ModelPath 是 .onnx 模型文件
	ModelPath string
	// TokenizerPath 是 tokenizer.json
	TokenizerPath string
	// MaxTokens 见 DefaultMaxTokens；<= 0 时用它
	MaxTokens int
	// IntraOpThreads 是 ONNX Runtime 的算子内线程上限；<= 0 时取 min(NumCPU, 4)
	IntraOpThreads int
}

// ONNXEmbedder 实现 Embedder。
type ONNXEmbedder struct {
	tk        *tokenizer.Tokenizer
	sess      *ort.DynamicAdvancedSession
	dim       int
	maxTokens int

	// mu 串行化 Run。
	//
	// **这不是保险起见**：ONNX Runtime 的 Session::Run 不是并发安全的，而我们有四条路
	// 会同时打进来——recall 的关键路径（每轮）、结算的批量、后台预热、归档规则的索引。
	// 不串行化会出现随机崩溃或结果错乱，而那种故障极难归因到"并发跑同一个 session"。
	mu sync.Mutex

	// truncated 只用于日志节流：截断是**静默丢信息**，必须留痕，但不该每条都刷屏。
	truncatedOnce sync.Once
}

// ---------- 进程级运行时初始化 ----------

var (
	runtimeMu     sync.Mutex
	runtimeInited bool
	runtimeErr    error
)

// ensureRuntime 初始化 ONNX Runtime。
//
// 它是**进程级、只能初始化一次**的（binding 的 InitializeEnvironment 管的是全局环境），
// 所以这里用包级状态兜住，而不是放在 ONNXEmbedder 上——否则测试里建第二个实例就会炸。
//
// ⚠️ SetSharedLibraryPath 同样是全局的：**第一个**成功的路径生效。
// 本项目只需要一个嵌入后端，所以这不是问题；真需要换 dll 只能重启进程。
func ensureRuntime(runtimePath string) error {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if runtimeInited {
		return runtimeErr
	}
	if !ort.IsInitialized() {
		ort.SetSharedLibraryPath(runtimePath)
		runtimeErr = ort.InitializeEnvironment()
	}
	runtimeInited = true
	return runtimeErr
}

// ONNXRuntimeVersion 返回已加载的 onnxruntime 版本（启动日志用）。
//
// binding 把 C API 头固定在某个版本上，**必须与 dll 对齐**，而版本不匹配的症状是
// 加载期一句很难懂的报错。所以启动时把实际版本打出来，出问题时一眼能对上。
func ONNXRuntimeVersion() string {
	if !ort.IsInitialized() {
		return ""
	}
	return ort.GetVersion()
}

// ---------- 构造 ----------

// NewONNXEmbedder 组装一个进程内嵌入器。
//
// 它**不做降级**：三个文件缺任何一个、维度不是 1024、都直接返回错误。
// 调用方（main）要把这个错误变成启动拦截——用户明确要求过"不允许出现聊完了没记忆"，
// 而"嵌入不可用"恰恰就会造成那个状态，所以它必须是**挡住**而不是"少做一点"。
func NewONNXEmbedder(cfg ONNXEmbedConfig) (*ONNXEmbedder, error) {
	for _, f := range []struct{ name, path string }{
		{"onnxruntime 动态库", cfg.RuntimePath},
		{"ONNX 模型", cfg.ModelPath},
		{"tokenizer.json", cfg.TokenizerPath},
	} {
		if f.path == "" {
			return nil, fmt.Errorf("嵌入配置缺少%s的路径", f.name)
		}
		if _, err := os.Stat(f.path); err != nil {
			return nil, fmt.Errorf("找不到%s：%s（%v）", f.name, f.path, err)
		}
	}

	if err := ensureRuntime(cfg.RuntimePath); err != nil {
		return nil, fmt.Errorf("初始化 ONNX Runtime 失败（dll 版本必须与 binding 对齐，本 binding 要求 1.29.0）: %w", err)
	}

	tk, err := tokenizer.New(cfg.TokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("加载分词器失败: %w", err)
	}

	// 线程数封顶：桌宠是陪聊的，嵌一条向量时把 16 个核全顶满会被用户察觉（风扇、卡顿）。
	// 4 是权衡——再少会让长文本明显变慢，再多就没有意义了。
	threads := cfg.IntraOpThreads
	if threads <= 0 {
		threads = min(runtime.NumCPU(), 4)
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("创建 ONNX 会话选项失败: %w", err)
	}
	defer opts.Destroy()
	if err := opts.SetIntraOpNumThreads(threads); err != nil {
		return nil, fmt.Errorf("设置 ONNX 线程数失败: %w", err)
	}

	// 用 DynamicAdvancedSession 而不是 AdvancedSession：输入形状取决于**运行时**的
	// 序列长度（图的 batch 与 sequence 都是动态轴），必须在 Run 时才给张量。
	sess, err := ort.NewDynamicAdvancedSession(cfg.ModelPath,
		[]string{onnxInputIDs, onnxAttentionMask},
		[]string{onnxOutputDenseVec}, opts)
	if err != nil {
		return nil, fmt.Errorf("加载 ONNX 模型失败（输入/输出名或形状与图不符）: %w", err)
	}

	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	return &ONNXEmbedder{
		tk:        tk,
		sess:      sess,
		dim:       DefaultEmbedDim,
		maxTokens: maxTokens,
	}, nil
}

// Dim 实现 Embedder。
func (e *ONNXEmbedder) Dim() int { return e.dim }

// Close 释放会话。
//
// 刻意**不**调 DestroyEnvironment：ONNX Runtime 的环境是进程级的，而销毁它的时机
// 必须晚于所有会话；交给进程退出处理更安全（析构顺序错会让退出时崩一下，很难看但无害）。
func (e *ONNXEmbedder) Close() error {
	if e.sess == nil {
		return nil
	}
	return e.sess.Destroy()
}

// Verify 真跑一次最小嵌入，确认整条链路（分词 → 张量 → 图 → 取回）是通的、维度也对。
//
// 为什么值得在启动时花这一次：这条路上任何一处接错，症状都会拖到"用户第一次写记忆"
// 才出现，而且表现是"记不进去"或"召回不准"，与嵌入看起来毫无关系。
func (e *ONNXEmbedder) Verify(ctx context.Context) error {
	v, err := e.Embed(ctx, []string{"ping"})
	if err != nil {
		return err
	}
	if len(v) != 1 || len(v[0]) != e.dim {
		return fmt.Errorf("自检失败：期望 1 条 %d 维向量，实际得到 %d 条、每条 %d 维",
			e.dim, len(v), len(v[0]))
	}
	return nil
}

// ---------- 主流程 ----------

// maxBatchSize 是一次 Run 里最多放几条文本。
const maxBatchSize = 8

// lengthSlack 和 lengthRatio 决定"多长算太长，该另起一组"。
//
// 分组是为了**省算力**，不是为了消除数值差异——这一点是实测纠正过来的，先说清楚：
//
// 一开始的假设是"int8 动态量化的尺度按整个张量统计，所以短句被长文拖着 padding 会失真"。
// 但实测否掉了它：8 条一批（短句 pad 到 1800）时最低余弦 0.9786，
// 按长度分组后（短句只 pad 到 11）最低余弦 0.9792 —— **几乎没变**。
//
// 真正的原因是**这个 int8 模型对输入形状敏感**：不同形状会走不同的 GEMM kernel，
// 累加顺序不同，24 层下来攒出约 2% 的余弦差。这一点改不了，只能如实记着
// （它也是"换模型后必须重校 recallMinScore"的又一条理由）。
//
// 那分组还留着干什么：**算力**。不分组时所有短句都要按最长那条（1800 token）算，
// 实测把整组测试从 4.89s 降到 2.87s。顺带也把 padding 的内存峰值限住了。
//
// 判据取**绝对差与相对差里更宽的那个**：只卡相对值时 3 和 6 就有 2 倍，但那点绝对差无所谓；
// 只卡绝对值时长文之间差 8 个 token 无所谓，可对短句来说 8 个就是好几倍。
//
// ⚠️ 光"排序 + 定长切块"不够：条数少于 maxBatchSize 时一条都不切（第一版就是这么写的）。
const (
	lengthSlack = 8
	lengthRatio = 2
)

// Embed 实现 Embedder。
//
// 批内 padding 是按**本组最长**做的（不是固定 512）：图是动态轴，按实际长度喂能省下
// 大量无用计算——短文本占绝大多数，固定 512 会白烧十几倍；而且按上面的理由，
// padding 越少，int8 量化尺度被拉偏得越少。
func (e *ONNXEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// 推理一旦开始就打断不了（CPU 算子不像 HTTP 请求可以取消），所以只能在入口拦一次。
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil { // 等锁期间可能已经超时
		return nil, err
	}

	rows := make([][]int64, len(texts))
	order := make([]int, len(texts))
	for i, text := range texts {
		rows[i] = e.encode(text)
		order[i] = i
	}
	// 按 token 数升序 → 切成"长度相近"的若干组。
	// 排序只影响"谁和谁同批"，结果按原下标放回，所以**入参顺序与返回顺序仍然一一对应**。
	sort.SliceStable(order, func(a, b int) bool { return len(rows[order[a]]) < len(rows[order[b]]) })

	out := make([][]float32, len(texts))
	for start := 0; start < len(order); {
		// 组内以**最短**那条为基准：升序排过，所以就是 start 这条
		base := len(rows[order[start]])
		end := start + 1
		for end < len(order) && end-start < maxBatchSize {
			l := len(rows[order[end]])
			if l > base+lengthSlack && l > base*lengthRatio {
				break
			}
			end++
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		group := order[start:end]
		vecs, err := e.runBatch(rows, group)
		if err != nil {
			return nil, err
		}
		for k, idx := range group {
			out[idx] = vecs[k]
		}
		start = end
	}
	return validateVectors(out, len(texts), e.dim, "本机 ONNX 模型")
}

// runBatch 把指定的这几条（idx 给出下标）一次跑完。
//
// 张量的行顺序**必须**与 idx 一致，返回的向量也按同样的行序——中间隔了一层排序，
// 行序错位会让"把 A 的话当成 B 的话"存进去，而那种错在检索结果上完全看不出来。
func (e *ONNXEmbedder) runBatch(rows [][]int64, idx []int) ([][]float32, error) {
	batch := int64(len(idx))
	var maxLen int64
	for _, i := range idx {
		if int64(len(rows[i])) > maxLen {
			maxLen = int64(len(rows[i]))
		}
	}
	if maxLen == 0 {
		return nil, fmt.Errorf("分词结果为空（%d 条文本）", len(idx))
	}

	// 展平 + 右补 <pad>（id 1、mask 0）。
	// 补位必须配 mask=0。注意：**光有 mask 还不够** —— 上面说的 int8 量化串扰就是因为
	// 补位会把张量撑长，而量化区间是按整个张量算的。
	flatIDs := make([]int64, batch*maxLen)
	flatMask := make([]int64, batch*maxLen)
	for r, i := range idx {
		base := int64(r) * maxLen
		copy(flatIDs[base:], rows[i])
		for j := range rows[i] {
			flatMask[base+int64(j)] = 1
		}
	}

	shape := ort.NewShape(batch, maxLen)
	inIDs, err := ort.NewTensor(shape, flatIDs)
	if err != nil {
		return nil, fmt.Errorf("构造 input_ids 张量失败: %w", err)
	}
	defer inIDs.Destroy()
	inMask, err := ort.NewTensor(shape, flatMask)
	if err != nil {
		return nil, fmt.Errorf("构造 attention_mask 张量失败: %w", err)
	}
	defer inMask.Destroy()

	out, err := ort.NewEmptyTensor[float32](ort.NewShape(batch, int64(e.dim)))
	if err != nil {
		return nil, fmt.Errorf("构造输出张量失败: %w", err)
	}
	defer out.Destroy()

	if err := e.sess.Run([]ort.Value{inIDs, inMask}, []ort.Value{out}); err != nil {
		return nil, fmt.Errorf("ONNX 推理失败（形状或输入名与图不符）: %w", err)
	}

	raw := out.GetData()
	vectors := make([][]float32, batch)
	for i := int64(0); i < batch; i++ {
		v := make([]float32, e.dim)
		copy(v, raw[i*int64(e.dim):(i+1)*int64(e.dim)])
		vectors[i] = v
	}
	return vectors, nil
}

// encode 分词并按 maxTokens 截断。
//
// 截断要**保住结尾的 </s>**：模型是按"整句"训练的，尾巴上那个 token 丢了会让向量偏。
// 而且截断这件事必须留痕——它是最典型的"静默丢信息"，
// 症状是"长文本的记忆召回不准"，从界面上完全看不出来。
func (e *ONNXEmbedder) encode(text string) []int64 {
	ids := e.tk.Encode(text)
	if len(ids) > e.maxTokens {
		e.truncatedOnce.Do(func() {
			log.Printf("[embed] 有文本超过 %d 个 token，已截断（只记一次）：本条 %d 个 token，约 %d 字",
				e.maxTokens, len(ids), len([]rune(text)))
		})
		ids = append(ids[:e.maxTokens-1:e.maxTokens-1], ids[len(ids)-1])
	}
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out
}
