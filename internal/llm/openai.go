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

type OpenAIProvider struct {
	cfg    Config
	client *http.Client
}

func NewOpenAIProvider(cfg Config) *OpenAIProvider {
	return &OpenAIProvider{cfg: cfg, client: &http.Client{}}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type streamResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func (p *OpenAIProvider) ChatStream(c context.Context, messages []Message) (<-chan Chunk, error) {
	if p.cfg.APIKey == "" {
		return nil, errorcode.ErrCodeUnKownAPIKey
	}
	body, err := json.Marshal(chatRequest{
		Model:    p.cfg.Model,
		Messages: messages,
		Stream:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(c, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败： %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求%s 失败： %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("服务端返回%s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	ch := make(chan Chunk)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		p.pump(c, resp.Body, ch)
	}()
	return ch, nil
}

func (p *OpenAIProvider) pump(c context.Context, r io.Reader, ch chan<- Chunk) {
	scanner := bufio.NewScanner(r)

	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		select {
		case <-c.Done():
			ch <- Chunk{Err: c.Err()}
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			ch <- Chunk{Done: true}
			return
		}
		var sr streamResponse
		if err := json.Unmarshal([]byte(payload), &sr); err != nil {
			continue
		}
		if len(sr.Choices) == 0 {
			continue
		}
		if delta := sr.Choices[0].Delta.Content; delta != "" {
			ch <- Chunk{Content: delta}
		}
	}

	if err := scanner.Err(); err != nil {
		ch <- Chunk{Err: fmt.Errorf("读取响应流失败: %w", err)}
		return
	}
	ch <- Chunk{Done: true}
}
