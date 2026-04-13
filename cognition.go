package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type CognitionClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewCognitionClient(baseURL string) *CognitionClient {
	return &CognitionClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

type LLMConfig struct {
	BaseURL     string  `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
}

type CognitionMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type CognitionEvent struct {
	Type      string         `json:"type"`
	ChatID    int64          `json:"chat_id"`
	UserID    int64          `json:"user_id,omitempty"`
	UserName  string         `json:"user_name,omitempty"`
	MessageID int            `json:"message_id,omitempty"`
	Text      string         `json:"text,omitempty"`
	Timestamp string         `json:"timestamp"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type RespondRequest struct {
	LLM                LLMConfig          `json:"llm"`
	EngineBaseURL      string             `json:"engine_base_url"`
	StaticPrompt       string             `json:"static_prompt"`
	PersonaSeed        string             `json:"persona_seed"`
	QuietHours         []int              `json:"quiet_hours"`
	ProactiveCooldownM int                `json:"proactive_cooldown_minutes"`
	RelationshipDecayD int                `json:"relationship_decay_days"`
	ChatID             int64              `json:"chat_id"`
	UserID             int64              `json:"user_id,omitempty"`
	UserName           string             `json:"user_name,omitempty"`
	VerifiedRole       string             `json:"verified_role,omitempty"`
	IsPrivate          bool               `json:"is_private"`
	AllowSensitive     bool               `json:"allow_sensitive"`
	MessageID          int                `json:"message_id,omitempty"`
	ReplyToMessageID   int                `json:"reply_to_message_id,omitempty"`
	Text               string             `json:"text"`
	RecentContext      []CognitionMessage `json:"recent_context,omitempty"`
	SkillSummaries     []string           `json:"skill_summaries,omitempty"`
	JobSummaries       []string           `json:"job_summaries,omitempty"`
	StickerSummaries   []string           `json:"sticker_summaries,omitempty"`
	KnowledgeSummary   string             `json:"knowledge_summary,omitempty"`
}

type ToolTrace struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

type RespondResponse struct {
	EpisodeID       string         `json:"episode_id"`
	ReplyText       string         `json:"reply_text"`
	ToolTranscript  []ToolTrace    `json:"tool_transcript,omitempty"`
	MemoryHits      []string       `json:"memory_hits,omitempty"`
	PersonaSnapshot map[string]any `json:"persona_snapshot,omitempty"`
}

type RequestIntentRequest struct {
	LLM  LLMConfig `json:"llm"`
	Text string    `json:"text"`
}

type RequestIntentResponse struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Year       string `json:"year"`
	IsRemaster bool   `json:"is_remaster"`
	Season     int    `json:"season"`
}

type TextTransformRequest struct {
	LLM        LLMConfig `json:"llm"`
	SystemHint string    `json:"system_hint"`
	UserText   string    `json:"user_text"`
}

type TextTransformResponse struct {
	Text string `json:"text"`
}

type PlannedAction struct {
	ID      string             `json:"id"`
	Kind    string             `json:"kind"`
	Command string             `json:"command"`
	Context HostCommandContext `json:"context"`
	Meta    map[string]any     `json:"meta,omitempty"`
}

type ActionResultRequest struct {
	Output    string `json:"output"`
	Success   bool   `json:"success"`
	ErrorText string `json:"error_text,omitempty"`
}

func (cc *CognitionClient) HealthCheck() error {
	req, err := http.NewRequest(http.MethodGet, cc.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := cc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求 cognition health 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cognition health 非 200: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (cc *CognitionClient) SendEvent(event *CognitionEvent) error {
	_, err := cc.postJSON("/cognition/events", event, nil)
	return err
}

func (cc *CognitionClient) Respond(reqBody *RespondRequest) (*RespondResponse, error) {
	var resp RespondResponse
	if _, err := cc.postJSON("/cognition/respond", reqBody, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (cc *CognitionClient) ParseRequestIntent(llm LLMConfig, text string) (*RequestIntentResponse, error) {
	var resp RequestIntentResponse
	if _, err := cc.postJSON("/cognition/request-intent", &RequestIntentRequest{LLM: llm, Text: text}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (cc *CognitionClient) TransformText(path string, reqBody *TextTransformRequest) (string, error) {
	var resp TextTransformResponse
	if _, err := cc.postJSON(path, reqBody, &resp); err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}

func (cc *CognitionClient) ClaimAction() (*PlannedAction, error) {
	var resp PlannedAction
	status, err := cc.postJSON("/cognition/actions/claim", map[string]any{}, &resp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	if resp.ID == "" {
		return nil, nil
	}
	return &resp, nil
}

func (cc *CognitionClient) SubmitActionResult(actionID string, result *ActionResultRequest) error {
	_, err := cc.postJSON("/cognition/actions/"+actionID+"/result", result, nil)
	return err
}

func (cc *CognitionClient) postJSON(path string, reqBody any, out any) (int, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return 0, fmt.Errorf("序列化 cognition 请求失败: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, cc.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, fmt.Errorf("创建 cognition 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cc.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("调用 cognition 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("读取 cognition 响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("cognition 返回错误 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, fmt.Errorf("解析 cognition 响应失败: %w", err)
		}
	}
	return resp.StatusCode, nil
}
