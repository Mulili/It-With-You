// 本文件是阶段4 的嵌入接入：把文本转成向量，供记忆检索用（默认 BGE-M3 @ 本机 Ollama）。
//
// 为什么不塞进 Provider（对话那个接口）：对话与嵌入**大概率是两个服务**——
// 本项目就是 DeepSeek 管聊天、Ollama 管嵌入，base_url 与 key 各不相同。
// 硬塞进同一个接口，等于逼用户把两者配到同一个地址上，是把架构按"最省事"的方式扭曲。
//
// 协议上只实现 OpenAI 兼容的 /embeddings：Ollama、TEI、Xinference、百炼、OpenAI
// 全都提供这个形状，一份实现覆盖所有部署方式，差别只在 base_url 与 model。
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

// OpenAIEmbedder 是 Embedder 的 OpenAI 兼容实现，覆盖 Ollama / TEI / Xinference / 百炼 / OpenAI。
type OpenAIEmbedder struct {
	cfg    EmbedConfig
	client *http.Client
}

// NewOpenAIEmbedder 只做组装，不做网络校验——可用性由调用方用 Verify 探测。
func NewOpenAIEmbedder(cfg EmbedConfig) *OpenAIEmbedder {
	return &OpenAIEmbedder{cfg: cfg, client: &http.Client{Timeout: EmbedTimeout}}
}

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

// Dim 实现 Embedder。
func (e *OpenAIEmbedder) Dim() int { return e.cfg.Dim }

// Embed 实现 Embedder。
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(embedRequest{Model: e.cfg.Model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("序列化嵌入请求失败: %w", err)
	}
	// BaseURL 可能被写成带结尾斜杠的形式，先裁掉再拼（与对话那边同一个理由）
	url := strings.TrimRight(e.cfg.BaseURL, "/") + "/embeddings"
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

	var out embedResponse
	// 一条 1024 维向量序列化后约 10KB，批量十几条也就几百 KB；
	// 32MB 是兜底上限，防的是服务端异常时返回整页垃圾把内存吃掉
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析嵌入响应失败: %w", err)
	}
	// 数量对不上必须报错：按"能取几条算几条"处理会静默丢文本，
	// 下游表现为"有些记忆就是存不进去"，极难查
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("嵌入服务返回 %d 条向量，请求了 %d 条（必须一一对应）", len(out.Data), len(texts))
	}

	vectors := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		// 维度不一致本该在启动时就被 Verify 拦住；这里再兜一道，
		// 防的是"启动后有人换了模型或改了配置"
		if len(d.Embedding) != e.cfg.Dim {
			return nil, fmt.Errorf("嵌入维度不匹配：配置 %d 维，服务返回 %d 维（model=%s）。改维度要同时改表和 COMPANION_EMBED_DIM，并重算所有历史向量",
				e.cfg.Dim, len(d.Embedding), e.cfg.Model)
		}
		vectors[i] = d.Embedding
	}
	return vectors, nil
}

// Verify 发一次最小请求，确认服务可用、且**实际维度与配置一致**。
//
// 为什么值得在启动时花这一次往返：维度不匹配的后果会拖到用户第一次写记忆时才出现，
// 而且报错来自 pgvector（"expected 1024 dimensions"），看着跟嵌入毫无关系。
// 把它提前到启动日志里说清楚，排查成本差一个数量级。
func (e *OpenAIEmbedder) Verify(ctx context.Context) error {
	_, err := e.Embed(ctx, []string{"ping"})
	return err
}
