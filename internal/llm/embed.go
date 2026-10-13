// 本文件是阶段4 的嵌入接入：把文本转成向量，供记忆检索用。
//
// 为什么不塞进 Provider（对话那个接口）：对话与嵌入**大概率是两个服务**——
// 本项目就是 DeepSeek 管聊天、嵌入另有来源，base_url 与 key 各不相同。
// 硬塞进同一个接口，等于逼用户把两者配到同一个地址上，是把架构按"最省事"的方式扭曲。
//
// 两条实现，由配置选后端（默认是进程内 ONNX，见 embed_onnx.go）：
//   - ONNXEmbedder：进程内跑本机 ONNX 模型，**不依赖任何常驻服务**（推荐）；
//   - OpenAIEmbedder：走 OpenAI 兼容的 /embeddings，覆盖 TEI / Xinference / 百炼 / OpenAI
//     这类远程或自建服务，差别只在 base_url 与 model。
//
// 两条都遵守 Embedder 的约定；**哪条不可用都是启动拦截的理由**，不是"降级成没有记忆"
// （用户明确要求过：不允许出现聊完了没记忆）。
//
// ⚠️ 这里**已经没有 Ollama 的原生端点与保活了**：`keep_alive`、`/api/version` 探测、
// "空闲 5 分钟被卸载"那一整套，只在"嵌入靠外部 Ollama"时才存在。换成进程内实现之后
// 没有冷启动，于是整块删掉——见 operation.md 的「嵌入运行时替换」。
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

// EmbedTimeout 是单次嵌入请求的总超时（只对 HTTP 那条实现有意义）。
//
// 与对话不同，这里可以设死超时：嵌入没有流式，响应小且快。留 30s 是为了覆盖
// 对端首次加载模型那一两秒。
const EmbedTimeout = 30 * time.Second

// 嵌入相关的环境变量。与对话那组（COMPANION_LLM_*）刻意分开：它们是两个服务。
const (
	// envEmbedBackend 选实现：onnx（默认，进程内）或 http（兼容端点）。
	envEmbedBackend = "COMPANION_EMBED_BACKEND"
	envEmbedBaseURL = "COMPANION_EMBED_BASE_URL"
	envEmbedAPIKey  = "COMPANION_EMBED_API_KEY"
	envEmbedModel   = "COMPANION_EMBED_MODEL"
	envEmbedDim     = "COMPANION_EMBED_DIM"
	// envEmbedModelDir / envEmbedRuntime 覆盖 ONNX 三件套的位置。
	// 不填就按 exe 同目录 → 用户缓存目录找（见 LocateONNXFiles）。
	envEmbedModelDir = "COMPANION_EMBED_MODEL_DIR"
	envEmbedRuntime  = "COMPANION_ONNXRUNTIME_DLL"
)

// EmbedBackendONNX / EmbedBackendHTTP 是 envEmbedBackend 的两个取值。
const (
	EmbedBackendONNX = "onnx"
	EmbedBackendHTTP = "http"
)

// EmbedBackendFromEnv 返回选定的后端，默认进程内 ONNX。
//
// 认不出来的值也回落到 ONNX（而不是报错）：错一个字符串就让应用起不来，
// 代价远大于"他其实想选 http"。
func EmbedBackendFromEnv() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envEmbedBackend)))
	switch v {
	case EmbedBackendHTTP, "openai", "remote":
		return EmbedBackendHTTP
	default:
		return EmbedBackendONNX
	}
}

// EmbedStatus 是嵌入后端对外的**状态**：给设置页显示、给启动拦截判断。
//
// 它必须能回答"到底哪儿不对"：嵌入不可用等于记忆整体失效，而用户能做的只有
// "把缺的东西补上"——只说一句"嵌入不可用"帮不上任何忙。
type EmbedStatus struct {
	// Backend 是生效的后端："onnx" 或 "http"
	Backend string `json:"backend"`
	// Ready 为 false 时上层**必须拦住启动**（见 App 里的拦截），embedder 也会是 nil
	Ready bool `json:"ready"`
	Dim   int  `json:"dim"`
	// Detail 就绪时是"模型路径 / 端点地址"，失败时是**失败原因**（含试过的位置）
	Detail string `json:"detail"`
}

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

// OpenAIEmbedder 是 Embedder 的实现，覆盖任何提供 OpenAI 兼容 /embeddings 的服务。
type OpenAIEmbedder struct {
	cfg    EmbedConfig
	client *http.Client
}

// NewOpenAIEmbedder 只做组装，不做网络校验——可用性由调用方用 Verify 探测。
func NewOpenAIEmbedder(cfg EmbedConfig) *OpenAIEmbedder {
	return &OpenAIEmbedder{cfg: cfg, client: &http.Client{Timeout: EmbedTimeout}}
}

// Dim 实现 Embedder。
func (e *OpenAIEmbedder) Dim() int { return e.cfg.Dim }

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

// ---------- 主流程 ----------

// Embed 实现 Embedder。
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
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
	return validateVectors(vectors, len(texts), e.cfg.Dim, "嵌入服务")
}

// postJSON 发一次 JSON POST，返回限流读取后的响应体。
//
// 抽出来的理由：嵌入请求只有这一个出口，而"状态码检查 + 只读 4KB 错误体 + 32MB 响应上限"
// 这几处一旦在别处再抄一遍，很容易漏掉某一项——症状是"偶尔把整页 HTML 当向量解析"。
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

// validateVectors 检查条数一一对应、以及每条维度与 dim 一致。
//
// 两条实现（本机 ONNX 与 HTTP 服务）**共用这一份**：它是最容易出现静默错误的一处
// （见下面注释），分成两份实现就等于允许两边慢慢漂移。
//
// 两处都必须报错而不是照收：
//   - 条数对不上 = 静默丢文本，下游表现为"有些记忆就是存不进去"，极难查；
//   - 维度不对 = 要等存进 pgvector 时才炸，而报错来自数据库（expected 1024 dimensions），
//     看着与嵌入毫无关系。
func validateVectors(vectors [][]float32, want, dim int, source string) ([][]float32, error) {
	if len(vectors) != want {
		return nil, fmt.Errorf("%s 返回 %d 条向量，请求了 %d 条（必须一一对应）", source, len(vectors), want)
	}
	for _, v := range vectors {
		if len(v) != dim {
			return nil, fmt.Errorf("嵌入维度不匹配：期望 %d 维，%s 返回 %d 维。改维度要同时改表和 DefaultEmbedDim，并重算所有历史向量",
				dim, source, len(v))
		}
	}
	return vectors, nil
}

// Verify 发一次最小请求，确认服务可用、**实际维度与配置一致**。
//
// 为什么值得在启动时花这一次往返：维度不匹配的后果会拖到用户第一次写记忆时才出现，
// 而且报错来自 pgvector（"expected 1024 dimensions"），看着跟嵌入毫无关系。
// 把它提前到启动日志里说清楚，排查成本差一个数量级。
//
// ⚠️ 它还是"这条后端能不能用"的唯一判据：失败就意味着上层要**拦住启动**
//（见 EmbedStatus），而不是降级成"没有记忆"。
func (e *OpenAIEmbedder) Verify(ctx context.Context) error {
	_, err := e.Embed(ctx, []string{"ping"})
	return err
}
