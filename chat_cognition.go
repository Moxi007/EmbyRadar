package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

func (ch *ChatHandler) emitCognitionEvent(eventType string, chatID, userID int64, userName string, messageID int, text string, metadata map[string]any) {
	if ch.cognition == nil {
		return
	}
	event := &CognitionEvent{
		Type:      eventType,
		ChatID:    chatID,
		UserID:    userID,
		UserName:  userName,
		MessageID: messageID,
		Text:      text,
		Timestamp: time.Now().Format(time.RFC3339),
		Metadata:  metadata,
	}
	if err := ch.cognition.SendEvent(event); err != nil {
		log.Printf("[Cognition] 发送事件失败: %v", err)
	}
}

func (ch *ChatHandler) llmConfig() LLMConfig {
	return LLMConfig{
		BaseURL:     ch.appConfig.Global.AIBaseURL,
		APIKey:      ch.appConfig.Global.AIAPIKey,
		Model:       ch.appConfig.Global.AIModel,
		MaxTokens:   ch.appConfig.Global.AIMaxTokens,
		Temperature: ch.appConfig.Global.AITemperature,
	}
}

func (ch *ChatHandler) buildRecentContext(chatID int64) []CognitionMessage {
	history := ch.ctxManager.GetMessages(chatID)
	if len(history) == 0 {
		return nil
	}
	out := make([]CognitionMessage, 0, len(history))
	for _, item := range history {
		text := strings.TrimSpace(item.Content.Text)
		if text == "" && len(item.Content.Parts) > 0 {
			text = "[多模态消息]"
		}
		if text == "" {
			continue
		}
		out = append(out, CognitionMessage{
			Role: item.Role,
			Text: text,
		})
	}
	return out
}

func (ch *ChatHandler) summarizeKnowledge(chatID int64) string {
	var parts []string
	if ch.globalKB != nil {
		if content := strings.TrimSpace(ch.globalKB.GetContent()); content != "" {
			parts = append(parts, content)
		}
	}
	if kb := ch.kbMap[chatID]; kb != nil && kb != ch.globalKB {
		if content := strings.TrimSpace(kb.GetContent()); content != "" {
			parts = append(parts, content)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	content := strings.Join(parts, "\n\n")
	if len(content) > 4000 {
		content = content[:4000] + "\n...[知识摘要截断]"
	}
	return content
}

func (ch *ChatHandler) summarizeSkills() []string {
	if ch.skillsLoader == nil {
		return nil
	}
	skills := ch.skillsLoader.ListSkills()
	if len(skills) == 0 {
		return nil
	}
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		out = append(out, fmt.Sprintf("%s: %s", skill.Name, skill.Description))
	}
	return out
}

func (ch *ChatHandler) summarizeJobs() []string {
	if ch.jobsLoader == nil {
		return nil
	}
	jobs := ch.jobsLoader.ListActiveJobs()
	if len(jobs) == 0 {
		return nil
	}
	out := make([]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, fmt.Sprintf("%s [%s]: %s", job.Name, job.Status, job.Description))
	}
	return out
}

func (ch *ChatHandler) summarizeStickers(chatID int64) []string {
	group := ch.appConfig.GetGroupConfig(chatID)
	if group == nil {
		return nil
	}
	var out []string
	if strings.TrimSpace(group.WelcomeStickerID) != "" {
		out = append(out, "welcome: 默认欢迎贴纸")
	}
	for alias := range group.AIStickers {
		if strings.TrimSpace(alias) == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s: 可发送贴纸", alias))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (ch *ChatHandler) buildRespondRequest(msg *MessageEnvelope, text string) *RespondRequest {
	group := ch.appConfig.GetGroupConfig(msg.ChatID)
	staticPrompt := "你是一个群聊助手，请保持回复简洁友好。"
	if group != nil && strings.TrimSpace(group.AISystemPrompt) != "" {
		staticPrompt = group.AISystemPrompt
	}
	return &RespondRequest{
		LLM:                ch.llmConfig(),
		EngineBaseURL:      defaultEngineBaseURL(),
		StaticPrompt:       staticPrompt,
		PersonaSeed:        ch.appConfig.Global.PersonaSeed,
		QuietHours:         ch.appConfig.Global.QuietHours,
		ProactiveCooldownM: ch.appConfig.Global.ProactiveCooldownMinutes,
		RelationshipDecayD: ch.appConfig.Global.RelationshipDecayDays,
		ChatID:             msg.ChatID,
		UserID:             msg.SenderID,
		UserName:           msg.DisplayName,
		VerifiedRole:       msg.VerifiedRole,
		IsPrivate:          msg.IsPrivate,
		AllowSensitive:     msg.AllowSensitive,
		MessageID:          msg.MessageID,
		ReplyToMessageID:   msg.ReplyToMessageID,
		Text:               text,
		RecentContext:      ch.buildRecentContext(msg.ChatID),
		SkillSummaries:     ch.summarizeSkills(),
		JobSummaries:       ch.summarizeJobs(),
		StickerSummaries:   ch.summarizeStickers(msg.ChatID),
		KnowledgeSummary:   ch.summarizeKnowledge(msg.ChatID),
	}
}

func defaultEngineBaseURL() string {
	return "http://" + defaultEngineListenAddr
}

type MessageEnvelope struct {
	ChatID           int64
	SenderID         int64
	MessageID        int
	ReplyToMessageID int
	DisplayName      string
	VerifiedRole     string
	IsPrivate        bool
	AllowSensitive   bool
	UserName         string
}
