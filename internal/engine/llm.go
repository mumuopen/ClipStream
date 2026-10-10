package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LLMClient OpenAI 兼容大模型客户端（支持任意 OpenAI 兼容端点/本地 ollama）
type LLMClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

func NewLLMClient(baseURL, apiKey, model, proxy string) *LLMClient {
	transport := &http.Transport{}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &LLMClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Transport: transport, Timeout: 180 * time.Second},
	}
}

func (l *LLMClient) Available() bool {
	return l != nil && l.APIKey != "" && l.Model != ""
}

// Chat 单轮对话，返回回复文本
func (l *LLMClient) Chat(ctx context.Context, system, user string) (string, error) {
	base := strings.TrimSuffix(l.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	body, _ := json.Marshal(map[string]any{
		"model":       l.Model,
		"temperature": 0.2,
		"messages": []any{
			map[string]string{"role": "system", "content": system},
			map[string]string{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.APIKey)

	resp, err := l.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("llm 返回 %d: %s", resp.StatusCode, tail(raw, 500))
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("llm 响应解析失败: %w", err)
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("llm 未返回 choices: %s", tail(raw, 300))
	}
	return r.Choices[0].Message.Content, nil
}