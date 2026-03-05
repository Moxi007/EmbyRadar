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
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", ac.APIKey))

	resp, err := ac.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 AI API 失败: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取 AI 响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("[AI] API 返回非 200 状态码: %d, 响应: %s", resp.StatusCode, string(respBytes))
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
