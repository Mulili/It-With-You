// 本文件是阶段2 的 LLM 接入实现：OpenAI 兼容协议的流式对话（DeepSeek 直接复用这套协议）。
//
// 一次调用的完整链路：
//
//	拼 JSON 请求体 → POST /chat/completions（stream:true）→ 服务端按 SSE 逐帧返回 → pump 逐行解析 → Chunk 通道
//
// 选择「net/http + 手写 SSE 解析」而不是引第三方 SDK：本项目只需要读 content 一个字段，
// 自己解既不用跟第三方依赖的版本节奏，也能把"流式到底是什么"看明白。
package llm

import (
	errorcode "agent-for-you-love/internal/pkg/errorCode"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIProvider 是 Provider 接口的 OpenAI 兼容实现，DeepSeek / OpenAI / 本地 vLLM 都能直接复用。
//
// client 用默认 Transport（自带连接池与 keep-alive），刻意不设 Timeout：
// 流式响应天生要长时间占住连接，设总超时会在长回复说到一半时被掐断。
// 超时与取消统一交给 ctx 控制——前端「停止」按钮走的就是这条路径。
type OpenAIProvider struct {
	cfg    Config
	client *http.Client
}

// NewOpenAIProvider 只做组装，不做网络校验。
// 缺 Key 之类的错误留到 ChatStream 里以业务错误码返回，这样构造函数保持无副作用、便于测试。
func NewOpenAIProvider(cfg Config) *OpenAIProvider {
	return &OpenAIProvider{cfg: cfg, client: &http.Client{}}
}

// chatRequest 是请求体。用结构体而不是 map 拼：字段名写错时 struct tag 至少能在审阅中被发现，
// 而 map 的 key 写错只会被服务端静默忽略，最后表现为"模型答非所问"这种很难查的现象。
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// Stream 为 true 时走流式（ChatStream 用）；为 false 时服务端一次性返回完整 JSON（Chat 用）。
	// 流式那条路径下它必须为 true，否则返回体里没有 SSE 帧，pump 一行都解不出来，
	// 表现是"发出去没反应"。
	Stream bool `json:"stream"`
	// ResponseFormat 只在要求 JSON 输出时带上，其余情况为 nil 并被 omitempty 省略
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	// Thinking 是 DeepSeek 的扩展字段（**不是** OpenAI 标准），形如
	// {"thinking":{"type":"enabled"|"disabled"}}，官方默认 enabled。
	//
	// 只在调用方明确要求关思考时才带：对不认识它的服务端，多带一个未知字段会直接 400。
	// 走手写 HTTP 时它就是**顶层字段**（用官方 SDK 才需要塞进 extra_body）。
	Thinking *thinkingSpec `json:"thinking,omitempty"`
}

// thinkingSpec 对应 DeepSeek 的思考模式开关。
type thinkingSpec struct {
	Type string `json:"type"`
}

// responseFormat 对应 OpenAI 兼容协议的 response_format 字段。
// 目前只用 "json_object"：要求服务端保证输出可被解析为 JSON 对象。
type responseFormat struct {
	Type string `json:"type"`
}

// chatResponse 是**非流式**响应的结构，正文在 choices[0].message.content。
// 它与流式的 streamResponse 形状不同（那边在 delta 里），所以各写一个。
//
// 注意：开了 JSON 模式时 content 仍是**一个字符串**（里面装着 JSON 文本），
// 服务端不会把它解包成嵌套对象，解析由调用方负责。
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		// FinishReason 同 streamResponse，暂不使用，留给"是否被截断"的判断
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// streamResponse 是响应流中「一帧」的结构，只声明用得到的字段——JSON 反序列化会自动忽略多余字段。
type streamResponse struct {
	Choices []struct {
		// Delta 是本帧的增量（区别于非流式的 message，后者是整段回答）
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		// FinishReason 目前不参与判断（本轮结束以 [DONE] 哨兵为准）。
		// 保留它是为将来区分"正常说完"与"被 max_tokens 截断"（值为 "length"）留个口子。
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// newRequest 构造一次 /chat/completions 请求（含鉴权与 ctx 绑定）。
//
// 流式与非流式共用：URL 拼接、请求头、ctx 绑定这几件事只写一份，
// 否则两条路径会慢慢走样（典型是加了新头只改了一边）。
func (p *OpenAIProvider) newRequest(c context.Context, messages []Message, stream bool, opts ChatOptions) (*http.Request, error) {
	// 只有在要求 JSON 输出时才带 response_format；不要求时留 nil，被 omitempty 省略
	var rf *responseFormat
	if opts.JSON {
		rf = &responseFormat{Type: "json_object"}
	}
	// 同理：只有明确要求关思考时才带 thinking，默认路径完全不带这个扩展字段
	var th *thinkingSpec
	if opts.DisableThinking {
		th = &thinkingSpec{Type: "disabled"}
	}
	body, err := json.Marshal(chatRequest{
		Model:          p.cfg.Model,
		Messages:       messages,
		Stream:         stream,
		ResponseFormat: rf,
		Thinking:       th,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	// BaseURL 来自环境变量，用户很可能写成带结尾斜杠的形式（https://x.com/），
	// 先裁掉再拼，避免拼出 //chat/completions
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"
	// NewRequestWithContext 把 ctx 绑在这次请求上：此后 cancel 会真正中断 HTTP 往返。
	// 「停止」按钮能立刻停住靠的就是这里，而不是前端单纯地不再渲染。
	req, err := http.NewRequestWithContext(c, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败： %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Key 只在这一行出现，绝不能打进日志。注意它也不该写进代码或仓库，来源见 config.go
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	if stream {
		req.Header.Set("Accept", "text/event-stream") // 明确告诉服务端要流式响应
	}
	return req, nil
}

// send 发一次请求并检查状态码，返回可读的响应体（**调用方负责 Close**）。
func (p *OpenAIProvider) send(c context.Context, messages []Message, stream bool, opts ChatOptions) (*http.Response, error) {
	req, err := p.newRequest(c, messages, stream, opts)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求%s 失败： %w", req.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		// 错误响应体只读前 4KB：正常错误 JSON 很小，但网关异常时可能返回整页 HTML，
		// 无上限地 ReadAll 会白占内存。io.LimitReader 是这类"只取一段"场景的标准做法。
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("服务端返回%s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	return resp, nil
}

// Chat 非流式地要一次完整回复，返回模型输出的正文（字符串）。
//
// 为什么要有非流式：流式是"给人看着舒服"，而抽取类场景（从对话里抽槽位、抽事实）
// 要的是机器可用的一整块结果，半个 JSON 没法解析，只能等完整响应。
//
// 本层不假设返回内容一定是 JSON——只负责把正文取出来，解析与校验由调用方做。
func (p *OpenAIProvider) Chat(c context.Context, messages []Message, opts ChatOptions) (string, error) {
	// 没有 Key 就不用发请求了，直接返回业务错误码
	if p.cfg.APIKey == "" {
		return "", errorcode.ErrCodeUnKownAPIKey
	}
	resp, err := p.send(c, messages, false, opts)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 抽取结果通常只有几百字节，但仍加上限兜底，避免异常情况下读进一整页垃圾
	var cr chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cr); err != nil {
		return "", fmt.Errorf("解析响应失败: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("服务端未返回任何选择项")
	}
	content := cr.Choices[0].Message.Content
	if content == "" {
		return "", fmt.Errorf("服务端返回了空内容")
	}
	return content, nil
}

// ChatStream 发起一次流式对话，立即返回 Chunk 通道（Provider 接口的约定，见 types.go）。
//
// 通道由内部 goroutine 生产。ctx 必须由调用方持有 cancel：
// 取消时底层 HTTP 往返会被中断，pump 收到 ctx.Err() 后以 Chunk{Err} 收尾，
// 而不是让前端自己丢弃后面的包。
func (p *OpenAIProvider) ChatStream(c context.Context, messages []Message, opts ChatOptions) (<-chan Chunk, error) {
	// 没有 Key 就不用发请求了，直接返回业务错误码，前端会把它显示在气泡里
	if p.cfg.APIKey == "" {
		return nil, errorcode.ErrCodeUnKownAPIKey
	}
	// 流式路径只认 DisableThinking：JSON 模式要求一次性给出完整对象，与逐帧流出天然矛盾，
	// 这里显式丢掉，免得"流式 + json_object"这种无意义组合被静默发出去。
	resp, err := p.send(c, messages, true, ChatOptions{DisableThinking: opts.DisableThinking})
	if err != nil {
		return nil, err
	}
	// 无缓冲通道 = 天然背压：生产端每写一帧都要等消费端取走，
	// 于是前端渲染不过来时，网络读取也会跟着慢下来，而不是把数据堆在内存里。
	ch := make(chan Chunk)
	go func() {
		// defer 是后进先出，所以实际执行顺序是：先关响应体，再关通道。
		// 这个顺序不能反——通道一关，调用方的 for range 立刻结束，会认为本轮已干净收尾。
		defer close(ch)
		defer resp.Body.Close()
		p.pump(c, resp.Body, ch)
	}()
	return ch, nil
}

// pump 逐行读取 SSE 响应，把每一帧转成 Chunk 写进通道，是流式解析的核心。
//
// SSE 帧格式（OpenAI 风格）长这样，一行一帧、空行分隔：
//
//	data: {"choices":[{"delta":{"content":"你"},"finish_reason":null}]}
//	: keep-alive                     ← 冒号开头的是注释/心跳，用于保活，不是内容
//	data: [DONE]                     ← 结束哨兵，注意它不是合法 JSON
func (p *OpenAIProvider) pump(c context.Context, r io.Reader, ch chan<- Chunk) {
	scanner := bufio.NewScanner(r)

	// bufio.Scanner 默认单行上限 64KB，超过就直接报 "token too long" 并中断整个流。
	// 一帧增量通常只有几个字，但首帧可能带上很长的 role / 元信息，留 1MB 余量更稳。
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		// 每读一行查一次取消状态。粒度是"行"：若正卡在上一行的 ch <- 上，
		// 要等消费端取走才会走到这里（消费端按约定读到底，所以不会真的卡死）。
		select {
		case <-c.Done():
			// 把取消当成一种"结束原因"交给上层，由上层决定这算错误还是正常收场
			ch <- Chunk{Err: c.Err()}
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())

		// 空行是帧分隔符，冒号开头是心跳/注释，两者都不是内容
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		// 只认 data: 行。event: / id: / retry: 等 SSE 字段本项目用不上，直接跳过。
		// 注意这是对 OpenAI 风格的简化解析：这里假定一帧就是一个完整的 JSON，
		// 没有处理标准 SSE 允许的"多行 data 拼一个事件"的情况。
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			ch <- Chunk{Done: true} // 收尾出口之一：正常结束
			return
		}
		var sr streamResponse
		// 单帧解析失败就跳过这一行，不中断整轮：服务端偶尔会插入非标准行，
		// 为一行无效数据放弃已经收到的整段回复不划算。
		if err := json.Unmarshal([]byte(payload), &sr); err != nil {
			continue
		}
		// 首帧通常只带 role 不带 content，choices 也可能为空，两种情况都跳过
		if len(sr.Choices) == 0 {
			continue
		}
		if delta := sr.Choices[0].Delta.Content; delta != "" {
			ch <- Chunk{Content: delta}
		}
	}

	// 收尾出口之二：读流出错（网络中断、响应体被截断等）
	if err := scanner.Err(); err != nil {
		ch <- Chunk{Err: fmt.Errorf("读取响应流失败: %w", err)}
		return
	}
	// 收尾出口之三：连接正常读完但始终没收到 [DONE]（部分服务端如此），按正常完成处理。
	// 走到这里说明上面的 [DONE] 分支没命中，所以不存在重复发 Done 的问题。
	ch <- Chunk{Done: true}
}
