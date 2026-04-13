package main

// cognition.go — Cognition 数据类型定义。
// CognitionClient（旧 HTTP 客户端）已删除，所有调用走 CognitionEngine 接口。
// 本文件仅保留跨模块共享的请求/响应类型。

// LLMConfig 大语言模型连接配置
type LLMConfig struct {
	BaseURL     string  `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
}

// CognitionMessage 认知上下文中的一条消息
type CognitionMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// CognitionEvent 认知事件（记录到事件日志）
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

// RespondRequest 对话请求（cognition/respond）
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

// ToolTrace 工具调用记录
type ToolTrace struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

// RespondResponse 对话响应
type RespondResponse struct {
	EpisodeID       string         `json:"episode_id"`
	ReplyText       string         `json:"reply_text"`
	ToolTranscript  []ToolTrace    `json:"tool_transcript,omitempty"`
	MemoryHits      []string       `json:"memory_hits,omitempty"`
	PersonaSnapshot map[string]any `json:"persona_snapshot,omitempty"`
}

// RequestIntentRequest 求片意图提取请求
type RequestIntentRequest struct {
	LLM  LLMConfig `json:"llm"`
	Text string    `json:"text"`
}

// RequestIntentResponse 求片意图提取响应
type RequestIntentResponse struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Year       string `json:"year"`
	IsRemaster bool   `json:"is_remaster"`
	Season     int    `json:"season"`
}

// TextTransformRequest 文本转换请求
type TextTransformRequest struct {
	LLM        LLMConfig `json:"llm"`
	SystemHint string    `json:"system_hint"`
	UserText   string    `json:"user_text"`
}

// TextTransformResponse 文本转换响应
type TextTransformResponse struct {
	Text string `json:"text"`
}

// PlannedAction 计划任务
type PlannedAction struct {
	ID      string             `json:"id"`
	Kind    string             `json:"kind"`
	Command string             `json:"command"`
	Context HostCommandContext `json:"context"`
	Meta    map[string]any     `json:"meta,omitempty"`
}

// ActionResultRequest 任务执行结果
type ActionResultRequest struct {
	Output    string `json:"output"`
	Success   bool   `json:"success"`
	ErrorText string `json:"error_text,omitempty"`
}
