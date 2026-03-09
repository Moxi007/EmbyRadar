package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// ChatMessage 表示一条对话消息（OpenAI 格式）
type ChatMessage struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"` // 消息内容
}

// ChatCompletionRequest 表示 OpenAI Chat Completion 请求体
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
}

// ChatCompletionResponse 表示 OpenAI Chat Completion 响应体
type ChatCompletionResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// AIClient 封装了 OpenAI 兼容 API 的客户端
type AIClient struct {
	BaseURL     string
	APIKey      string
	Model       string
	MaxTokens   int
	Temperature float64
	HTTPClient  *http.Client
}

// NewAIClient 创建一个新的 AI 客户端实例
func NewAIClient(config *Config) *AIClient {
	return &AIClient{
		BaseURL:     config.AIBaseURL,
		APIKey:      config.AIAPIKey,
		Model:       config.AIModel,
		MaxTokens:   config.AIMaxTokens,
		Temperature: config.AITemperature,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second, // AI 响应可能较慢，设置 60 秒超时
		},
	}
}

// ChatCompletion 调用 Chat Completion API，返回 AI 回复文本
func (ac *AIClient) ChatCompletion(messages []ChatMessage) (string, error) {
	reqBody := ChatCompletionRequest{
		Model:       ac.Model,
		Messages:    messages,
		MaxTokens:   ac.MaxTokens,
		Temperature: ac.Temperature,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("序列化请求体失败: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", ac.BaseURL)
	maxRetries := 3
	var lastErr error

	for i := 0; i <= maxRetries; i++ {
		if i > 0 {
			// 指数退避重试 (1s, 2s, 4s...)
			delay := time.Duration(1<<uint(i-1)) * time.Second
			log.Printf("[AI] API 繁忙(503/429)或网络错误，等待 %v 后进行第 %d 次重试...", delay, i)
			time.Sleep(delay)
		}

		req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
		if err != nil {
			return "", fmt.Errorf("创建 HTTP 请求失败: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", ac.APIKey))

		resp, err := ac.HTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("调用 AI API 失败: %w", err)
			continue // 网络错误也重试
		}

		respBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		
		if err != nil {
			lastErr = fmt.Errorf("读取 AI 响应失败: %w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			// 针对 503 (服务不可用) 或 429 (请求过多) 进行重试
			if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
				lastErr = fmt.Errorf("AI API 返回临时错误 (HTTP %d): %s", resp.StatusCode, string(respBytes))
				continue
			}
			
			// 其他错误 (如 400, 401, 404) 直接报错，不需要重试
			log.Printf("[AI] API 返回非重试类状态码: %d, 响应: %s", resp.StatusCode, string(respBytes))
			return "", fmt.Errorf("AI API 返回错误 (HTTP %d): %s", resp.StatusCode, string(respBytes))
		}

		var chatResp ChatCompletionResponse
		if err := json.Unmarshal(respBytes, &chatResp); err != nil {
			return "", fmt.Errorf("解析 AI 响应失败: %w", err)
		}

		if chatResp.Error != nil {
			return "", fmt.Errorf("AI API 返回错误: %s", chatResp.Error.Message)
		}

		if len(chatResp.Choices) == 0 {
			return "", fmt.Errorf("AI API 未返回任何回复")
		}

		return chatResp.Choices[0].Message.Content, nil
	}

	return "", fmt.Errorf("AI API 重试 %d 次后仍然失败, 最终错误: %w", maxRetries, lastErr)
}
