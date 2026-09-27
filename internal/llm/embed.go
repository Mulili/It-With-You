// 本文件是阶段4 的嵌入接入：把文本转成向量，供记忆检索用（默认 BGE-M3 @ 本机 Ollama）。
//
// 为什么不塞进 Provider（对话那个接口）：对话与嵌入**大概率是两个服务**——
// 本项目就是 DeepSeek 管聊天、Ollama 管嵌入，base_url 与 key 各不相同。
// 硬塞进同一个接口，等于逼用户把两者配到同一个地址上，是把架构按"最省事"的方式扭曲。
//
// 协议上以 OpenAI 兼容的 /embeddings 为基准：Ollama、TEI、Xinference、百炼、OpenAI
// 全都提供这个形状，一份实现覆盖所有部署方式，差别只在 base_url 与 model。
//
// 唯一的例外是**保活**：只有 Ollama 的原生端点认 keep_alive，而"模型别被卸载"这件事
// 对体验影响不小，所以对 Ollama 单独走一趟原生端点——见 nativeOllama 与 embedKeepAlive。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"agent-for-you-love/internal/config"
)

// DefaultEmbedDim 是默认嵌入维度：BGE-M3 就是 1024。
//
// 维度**必须**与建表时 vector(n) 的 n 一致：改了模型或维度，等于要重建列 + 重算
// 所有历史向量。所以它既是一个配置项，也是启动时要校验的对象。
const DefaultEmbedDim = 1024

// EmbedTimeout 是单次嵌入请求的总超时。
//
// 与对话不同，这里可以设死超时：嵌入没有流式，响应小且快。留 30s 是为了覆盖
// "Ollama 首次把模型加载进内存"那一两秒。
const EmbedTimeout = 30 * time.Second

// embedKeepAlive 是每次嵌入请求附带"模型在显存里留多久"，-1 = 不卸载。
//
// 为什么值得让 380MB 显存常驻：
//   - Ollama 默认**空闲 5 分钟就卸载模型**，而冷启动实测要 2.2 秒（热 0.05 秒）；
//   - 那 2.2 秒落在**关键路径**上（recall 在拼上下文之前），也就是用户按完回车干等着；
//   - 而且本项目的目标是"分发给不喜欢折腾的人"——指望每个用户自己去配
//     `OLLAMA_KEEP_ALIVE` 环境变量是不现实的（连开发者自己都要试两次才发现它生效）。
//
// ⚠️ 这个参数**只有 Ollama 的原生端点 /api/embed 认**。OpenAI 兼容的 /v1/embeddings
// 会**静默忽略**它（实测：传 600 或 "10m" 都无效，`/api/ps` 的 expires_at 仍旧是 5 分钟后）。
// 所以本文件对 Ollama 走原生端点、对其余服务走兼容端点——见 OpenAIEmbedder.nativeOllama。
const embedKeepAlive = -1

// 嵌入相关的环境变量。与对话那组（COMPANION_LLM_*）刻意分开：它们是两个服务。
const (
	envEmbedBaseURL = "COMPANION_EMBED_BASE_URL"
	envEmbedAPIKey  = "COMPANION_EMBED_API_KEY"
	envEmbedModel   = "COMPANION_EMBED_MODEL"
	envEmbedDim     = "COMPANION_EMBED_DIM"
)

// EmbedConfig 是嵌入服务的配置。
type EmbedConfig struct {
	BaseURL string
	// APIKey 对本地 Ollama 用不上（它不校验 key），留空即可：
	// 留空时请求不带 Authorization 头，免得某些服务端对空 Bearer 直接报错。
	APIKey string
	Model  string
	// Dim 是本配置**期望**的维度，启动时会与服务端实际返回的比对
	Dim int
}

// EmbedConfigFromEnv 从环境变量读嵌入配置。
//
// BaseURL / Model 都给了默认值（本机 Ollama + bge-m3）：这是推荐方案，也是最省配置的一条路。
// 于是"没配"和"配了但服务没起"在下游表现为同一件事——探测失败 → 记忆功能停用，
// 不需要额外区分，降级逻辑因此只有一条分支。
//
// ⚠️ 默认模型名必须是**通用名**，不要在这里写社区再分发的名字（例如 qllama/bge-m3:latest）：
// 那类名字只对某个人的机器有意义，写进代码会让别人 clone 后默认指向一个不存在的模型，
// 第一印象就是"这东西跑不起来"。本机实际用哪个名字，写在 .env 的 COMPANION_EMBED_MODEL
// 里覆盖即可（启动日志会打印最终生效的那个名字，便于核对）。
func EmbedConfigFromEnv() EmbedConfig {
	return EmbedConfig{
		BaseURL: config.Get(envEmbedBaseURL, "http://localhost:11434/v1"),
		APIKey:  os.Getenv(envEmbedAPIKey),
		Model:   config.Get(envEmbedModel, "bge-m3"),
		Dim:     config.GetInt(envEmbedDim, DefaultEmbedDim),
	}
}

// Embedder 把文本转成向量。
//
// 两条约定（实现方必须遵守）：
//   - 返回的向量**顺序与入参一一对应**：顺序错了会把 A 的话当成 B 的话存进去/检回来，
//     而这种错误在检索结果上完全看不出来；
//   - 每条向量**长度等于 Dim()**：不等就是配置与服务端不一致，要报错而不是照收。
type Embedder interface {
	// Embed 批量取向量。必须支持一次传多条——一轮对话可能抽出十几条待存记忆，
	// 逐条发请求就是十几次网络往返。
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dim 返回本配置期望的维度。
	Dim() int
}

// OpenAIEmbedder 是 Embedder 的实现，覆盖 Ollama / TEI / Xinference / 百炼 / OpenAI。
type OpenAIEmbedder struct {
	cfg    EmbedConfig
	client *http.Client
	// nativeOllama 表示 base_url 背后是 Ollama，于是可以走它的原生端点。
	//
	// 它由 Verify 探测后写入：构造时不能发网络请求，而"发请求"这件事
	// 本该发生在启动探测那一刻。⚠️ 因此 **Verify 必须在任何 Embed 之前调用一次**
	// （实际调用点是 main 的 probeEmbedder），之后这个字段只读、不再变。
	nativeOllama bool
}

// NewOpenAIEmbedder 只做组装，不做网络校验——可用性由调用方用 Verify 探测。
func NewOpenAIEmbedder(cfg EmbedConfig) *OpenAIEmbedder {
	return &OpenAIEmbedder{cfg: cfg, client: &http.Client{Timeout: EmbedTimeout}}
}

// Dim 实现 Embedder。
func (e *OpenAIEmbedder) Dim() int { return e.cfg.Dim }

// NativeOllama 报告是否探测到了 Ollama 的原生端点，也就是**保活是否生效**。
//
// 给启动日志用：它决定"模型会不会因为空闲 5 分钟被卸载"，而这件事从任何其它日志里
// 都看不出来——只有把模型读回来时那 2.2 秒会提醒你，但那已经晚了（用户正等着回复）。
// ⚠️ 必须在 Verify 之后调用才有意义。
func (e *OpenAIEmbedder) NativeOllama() bool { return e.nativeOllama }

// ---------- 兼容端点（OpenAI 形状）----------

// embedRequest 是 /embeddings 的请求体。
//
// input 用数组而不是单条字符串：OpenAI 与 Ollama 都接受两种写法，用数组能天然支持批量。
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse 是响应体，向量在 data[i].embedding。
// 其余字段（object / model / usage）用不上，反序列化会自动忽略。
type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// ---------- Ollama 原生端点 ----------

// ollamaEmbedRequest 是 /api/embed 的请求体。
//
// 与兼容端点的差别只有 keep_alive——但正是这一个字段决定了模型会不会被卸载，
// 所以对 Ollama 值得单独走这条路。
type ollamaEmbedRequest struct {
	Model     string   `json:"model"`
	Input     []string `json:"input"`
	KeepAlive int      `json:"keep_alive"`
}

// ollamaEmbedResponse 是 /api/embed 的响应体：向量直接在 embeddings 里（二维数组），
// 不像 OpenAI 那样包一层 data[].embedding。
type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// nativeBase 从兼容端点的 base_url 推出 Ollama 原生端点的前缀。
//
// 兼容端点的约定是以 /v1 结尾（OpenAI 风格），而原生端点挂在同一个 host 的 /api/... 上，
// 所以去掉结尾的 /v1 就够了。用户若把 base_url 写成不带 /v1 的形式，这里也不会出错。
func nativeBase(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// probeOllama 判断 base_url 背后是不是 Ollama（决定 Embed 走哪个端点）。
//
// 依据是原生端点 /api/version：兼容端点 /v1/embeddings 是"大家都有的形状"，认不出是谁；
// 而 /api/version 只有 Ollama 提供。探不到（超时、404、别的服务）就**当作不是**——
// 退回兼容端点是最安全的判断，因为它覆盖的服务最多。
func (e *OpenAIEmbedder) probeOllama(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nativeBase(e.cfg.BaseURL)+"/api/version", nil)
	if err != nil {
		return false
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// 送掉一点 body 让连接能被复用；内容本身不看，只认状态码
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode == http.StatusOK
}

// ---------- 主流程 ----------

// Embed 实现 Embedder。
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if e.nativeOllama {
		return e.embedOllama(ctx, texts)
	}
	return e.embedOpenAI(ctx, texts)
}

func (e *OpenAIEmbedder) embedOpenAI(ctx context.Context, texts []string) ([][]float32, error) {
	// BaseURL 可能被写成带结尾斜杠的形式，先裁掉再拼（与对话那边同一个理由）
	data, err := e.postJSON(ctx, strings.TrimRight(e.cfg.BaseURL, "/")+"/embeddings",
		embedRequest{Model: e.cfg.Model, Input: texts})
	if err != nil {
		return nil, err
	}

	var out embedResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析嵌入响应失败: %w", err)
	}
	vectors := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vectors[i] = d.Embedding
	}
	return e.validateVectors(vectors, len(texts))
}

func (e *OpenAIEmbedder) embedOllama(ctx context.Context, texts []string) ([][]float32, error) {
	data, err := e.postJSON(ctx, nativeBase(e.cfg.BaseURL)+"/api/embed",
		ollamaEmbedRequest{Model: e.cfg.Model, Input: texts, KeepAlive: embedKeepAlive})
	if err != nil {
		return nil, err
	}

	var out ollamaEmbedResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析嵌入响应失败: %w", err)
	}
	return e.validateVectors(out.Embeddings, len(texts))
}

// postJSON 发一次 JSON POST，返回限流读取后的响应体。
//
// 两个端点只有 URL、请求体、响应体形状不同，传输这一段完全一样。抽出来是因为
// "状态码检查 + 只读 4KB 错误体 + 32MB 响应上限"这几处一旦分叉，
// 症状就是"某一个端点偶尔把整页 HTML 当向量解析"。
func (e *OpenAIEmbedder) postJSON(ctx context.Context, url string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化嵌入请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造嵌入请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求嵌入服务失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 错误体只读前 4KB：正常错误 JSON 很小，网关异常时可能返回整页 HTML
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("嵌入服务返回 %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	// 一条 1024 维向量序列化后约 10KB，批量十几条也就几百 KB；
	// 32MB 是兜底上限，防的是服务端异常时返回整页垃圾把内存吃掉
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// validateVectors 检查条数一一对应、以及每条维度与配置一致。
//
// 两处都必须报错而不是照收：
//   - 条数对不上 = 静默丢文本，下游表现为"有些记忆就是存不进去"，极难查；
//   - 维度不对 = 要等存进 pgvector 时才炸，而报错来自数据库（expected 1024 dimensions），
//     看着与嵌入毫无关系。
func (e *OpenAIEmbedder) validateVectors(vectors [][]float32, want int) ([][]float32, error) {
	if len(vectors) != want {
		return nil, fmt.Errorf("嵌入服务返回 %d 条向量，请求了 %d 条（必须一一对应）", len(vectors), want)
	}
	for _, v := range vectors {
		if len(v) != e.cfg.Dim {
			return nil, fmt.Errorf("嵌入维度不匹配：配置 %d 维，服务返回 %d 维（model=%s）。改维度要同时改表和 COMPANION_EMBED_DIM，并重算所有历史向量",
				e.cfg.Dim, len(v), e.cfg.Model)
		}
	}
	return vectors, nil
}

// Verify 发一次最小请求，确认服务可用、**实际维度与配置一致**，并顺手探测端点类型。
//
// 为什么值得在启动时花这一次往返：维度不匹配的后果会拖到用户第一次写记忆时才出现，
// 而且报错来自 pgvector（"expected 1024 dimensions"），看着跟嵌入毫无关系。
// 把它提前到启动日志里说清楚，排查成本差一个数量级。
//
// 它还有第二个职责：**决定 Embed 走哪个端点**（见 nativeOllama）。放在这里而不是构造函数里，
// 是因为构造不该发网络请求，而这里本来就要发一次。
func (e *OpenAIEmbedder) Verify(ctx context.Context) error {
	// 探测结果决定后面走原生还是兼容端点，所以必须在第一次 Embed 之前定下来
	e.nativeOllama = e.probeOllama(ctx)
	_, err := e.Embed(ctx, []string{"ping"})
	return err
}
