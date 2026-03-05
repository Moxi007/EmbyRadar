package main

import (
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ChatHandler 处理群聊消息与 AI 回复
type ChatHandler struct {
	bot        *tgbotapi.BotAPI
	aiClient   *AIClient
	ctxManager *ContextManager
	kb         *KnowledgeBase
	config     *Config
}

// NewChatHandler 创建聊天处理器
func NewChatHandler(bot *tgbotapi.BotAPI, aiClient *AIClient, ctxManager *ContextManager, config *Config, kb *KnowledgeBase) *ChatHandler {
	return &ChatHandler{
		bot:        bot,
		aiClient:   aiClient,
		ctxManager: ctxManager,
		kb:         kb,
		config:     config,
	}
}

// StartListening 启动消息监听（长轮询）
func (ch *ChatHandler) StartListening() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := ch.bot.GetUpdatesChan(u)

	log.Printf("[AI] 消息监听已启动，等待群聊消息...")

	for update := range updates {
		if update.Message == nil {
			continue
		}

		// 处理管理员命令
		if ch.handleCommand(update.Message) {
			continue
		}

		// 判断是否需要 AI 回复
		if ch.shouldRespond(update.Message) {
			go ch.handleAIResponse(update.Message)
		}
	}
}

// handleCommand 处理管理员命令，返回 true 表示已处理
func (ch *ChatHandler) handleCommand(msg *tgbotapi.Message) bool {
	if !msg.IsCommand() {
		return false
	}

	switch msg.Command() {
	case "reload_kb":
		// 热重载知识库（仅管理员可用）
		if ch.isAdmin(msg) {
			if err := ch.kb.Reload(); err != nil {
				ch.sendReply(msg, fmt.Sprintf("❌ 重载知识库失败: %v", err))
			} else {
				ch.sendReply(msg, "✅ 知识库已重新加载")
			}
		}
		return true

	case "kb_list":
		// 列出所有知识库条目
		if ch.isAdmin(msg) {
			entries, err := ch.kb.ListEntries()
			if err != nil {
				ch.sendReply(msg, fmt.Sprintf("❌ 获取知识库列表失败: %v", err))
				return true
			}
			if len(entries) == 0 {
				ch.sendReply(msg, "📭 知识库目前为空。")
			} else {
				ch.sendReply(msg, "📚 **当前知识库条目：**\n"+strings.Join(entries, "\n"))
			}
		}
		return true

	case "kb_add":
		// 添加知识库条目，用法: /kb_add <名称> <内容...>
		if ch.isAdmin(msg) {
			args := msg.CommandArguments()
			parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
			if len(parts) < 2 {
				ch.sendReply(msg, "💡 用法: `/kb_add <条目名称> <内容>`\n\n示例:\n`/kb_add rules 群规第一条是不允许发色情内容`")
				return true
			}
			name := parts[0]
			content := parts[1]

			if err := ch.kb.AddEntry(name, content); err != nil {
				ch.sendReply(msg, fmt.Sprintf("❌ 添加知识库条目失败: %v", err))
			} else {
				ch.sendReply(msg, fmt.Sprintf("✅ 成功添加知识库条目: `%s`", name))
			}
		}
		return true

	case "kb_del":
		// 删除知识库条目，用法: /kb_del <名称>
		if ch.isAdmin(msg) {
			name := strings.TrimSpace(msg.CommandArguments())
			if name == "" {
				ch.sendReply(msg, "💡 用法: `/kb_del <条目名称>`\n可以通过 `/kb_list` 查看现有条目。")
				return true
			}

			if err := ch.kb.DeleteEntry(name); err != nil {
				ch.sendReply(msg, fmt.Sprintf("❌ 删除条目失败: %v", err))
			} else {
				ch.sendReply(msg, fmt.Sprintf("✅ 成功删除知识库条目: `%s`", name))
			}
		}
		return true

	case "clear_ctx":
		// 清空当前群的上下文
		ch.ctxManager.ClearContext(msg.Chat.ID)
		ch.sendReply(msg, "✅ 对话上下文已清空")
		return true

	case "ask":
		// /ask 命令触发 AI 回复
		args := msg.CommandArguments()
		if args == "" {
			ch.sendReply(msg, "💡 用法: /ask <你的问题>")
			return true
		}
		go ch.handleAIResponse(msg)
		return true
	}

	return false
}

// shouldRespond 判断是否需要对该消息进行 AI 回复
func (ch *ChatHandler) shouldRespond(msg *tgbotapi.Message) bool {
	// 忽略命令消息（已在 handleCommand 中处理）
	if msg.IsCommand() {
		return false
	}

	// 忽略空消息
	if msg.Text == "" {
		return false
	}

	// 检查是否 @ 了 Bot
	if msg.Entities != nil {
		for _, entity := range msg.Entities {
			if entity.Type == "mention" {
				mention := msg.Text[entity.Offset : entity.Offset+entity.Length]
				if strings.EqualFold(mention, "@"+ch.bot.Self.UserName) {
					return true
				}
			}
		}
	}

	// 检查是否回复了 Bot 的消息
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		if msg.ReplyToMessage.From.ID == ch.bot.Self.ID {
			return true
		}
	}

	// 检查关键词触发
	if len(ch.config.AITriggerKeywords) > 0 {
		lowerText := strings.ToLower(msg.Text)
		for _, keyword := range ch.config.AITriggerKeywords {
			if strings.Contains(lowerText, strings.ToLower(keyword)) {
				return true
			}
		}
	}

	return false
}

// handleAIResponse 处理 AI 回复逻辑
func (ch *ChatHandler) handleAIResponse(msg *tgbotapi.Message) {
	// 提取用户消息文本
	userText := msg.Text

	// 如果是 /ask 命令，提取参数部分
	if msg.IsCommand() && msg.Command() == "ask" {
		userText = msg.CommandArguments()
	}

	// 去掉 @botname 的部分
	userText = ch.cleanMention(userText)
	if strings.TrimSpace(userText) == "" {
		return
	}

	chatID := msg.Chat.ID

	// 发送 "正在输入..." 状态
	typingAction := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	ch.bot.Send(typingAction)

	// 获取用户显示名称和头衔
	var userName string
	var displayRole string

	// 判断是否为频道代发（关联频道发的消息）
	if msg.SenderChat != nil {
		userName = msg.SenderChat.Title
		// 如果是指定频道，或刚好对应频道的 ID，可以在传给 AI 时带上特殊标识
		displayRole = fmt.Sprintf("%s(频道ID:%d)", userName, msg.SenderChat.ID)
	} else if msg.From != nil {
		userName = msg.From.FirstName
		if msg.From.LastName != "" {
			userName += " " + msg.From.LastName
		}

		// 尝试获取用户在此群的专属头衔（CustomTitle）
		memberTitle := ""
		if msg.Chat.Type == "supergroup" || msg.Chat.Type == "group" {
			memberConfig := tgbotapi.GetChatMemberConfig{
				ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
					ChatID: chatID,
					UserID: msg.From.ID,
				},
			}
			if member, err := ch.bot.GetChatMember(memberConfig); err == nil {
				if member.CustomTitle != "" {
					memberTitle = member.CustomTitle
				}
			}
		}

		// 合并头衔和名字（如果有头衔的话）
		displayRole = userName
		if memberTitle != "" {
			displayRole = fmt.Sprintf("%s(头衔:%s)", userName, memberTitle)
		}
	} else {
		userName = "未知用户"
		displayRole = userName
	}

	// 构建消息列表
	messages := ch.buildMessages(chatID, displayRole, userText)

	// 调用 AI API
	reply, err := ch.aiClient.ChatCompletion(messages)
	if err != nil {
		log.Printf("[AI] 调用 AI 失败: %v", err)
		ch.sendReply(msg, "⚠️ AI 暂时无法回复，请稍后再试")
		return
	}

	// 保存上下文（用户消息 + AI 回复）
	ch.ctxManager.AddMessage(chatID, ChatMessage{
		Role:    "user",
		Content: fmt.Sprintf("[%s]: %s", displayRole, userText),
	})
	ch.ctxManager.AddMessage(chatID, ChatMessage{
		Role:    "assistant",
		Content: reply,
	})

	// 发送回复
	ch.sendReply(msg, reply)
}

// buildMessages 构建发送给 AI 的完整消息列表
func (ch *ChatHandler) buildMessages(chatID int64, userName, userText string) []ChatMessage {
	var messages []ChatMessage

	// 1. 系统提示词（人设 + 知识库）
	systemPrompt := ch.config.AISystemPrompt
	if systemPrompt == "" {
		systemPrompt = "你是一个群聊助手，请保持回复简洁友好。"
	}

	// 注入知识库内容
	kbContent := ch.kb.GetContent()
	if kbContent != "" {
		systemPrompt += "\n\n" + kbContent
	}

	messages = append(messages, ChatMessage{
		Role:    "system",
		Content: systemPrompt,
	})

	// 2. 历史上下文
	history := ch.ctxManager.GetMessages(chatID)
	if history != nil {
		messages = append(messages, history...)
	}

	// 3. 当前用户消息
	messages = append(messages, ChatMessage{
		Role:    "user",
		Content: fmt.Sprintf("[%s]: %s", userName, userText),
	})

	return messages
}

// cleanMention 从消息文本中移除 @botname
func (ch *ChatHandler) cleanMention(text string) string {
	botMention := "@" + ch.bot.Self.UserName
	text = strings.ReplaceAll(text, botMention, "")
	text = strings.ReplaceAll(text, strings.ToLower(botMention), "")
	return strings.TrimSpace(text)
}

// sendReply 回复消息
func (ch *ChatHandler) sendReply(msg *tgbotapi.Message, text string) {
	// Telegram 单条消息最大长度 4096 字符
	const maxLen = 4000

	if len(text) <= maxLen {
		reply := tgbotapi.NewMessage(msg.Chat.ID, text)
		reply.ReplyToMessageID = msg.MessageID
		reply.ParseMode = "Markdown"
		if _, err := ch.bot.Send(reply); err != nil {
			// Markdown 解析失败时降级为纯文本
			reply.ParseMode = ""
			ch.bot.Send(reply)
		}
		return
	}

	// 超长消息分段发送
	for len(text) > 0 {
		end := maxLen
		if end > len(text) {
			end = len(text)
		}

		// 尽量在换行处断开
		if end < len(text) {
			if idx := strings.LastIndex(text[:end], "\n"); idx > maxLen/2 {
				end = idx + 1
			}
		}

		chunk := text[:end]
		text = text[end:]

		reply := tgbotapi.NewMessage(msg.Chat.ID, chunk)
		reply.ReplyToMessageID = msg.MessageID
		if _, err := ch.bot.Send(reply); err != nil {
			log.Printf("[AI] 发送分段消息失败: %v", err)
		}
	}
}

// isAdmin 检查用户是否为群管理员或 Bot 管理员
func (ch *ChatHandler) isAdmin(msg *tgbotapi.Message) bool {
	// 私聊默认允许
	if msg.Chat.Type == "private" {
		return true
	}

	// 检查是否为群管理员
	memberConfig := tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: msg.Chat.ID,
			UserID: msg.From.ID,
		},
	}

	member, err := ch.bot.GetChatMember(memberConfig)
	if err != nil {
		log.Printf("[AI] 获取用户权限失败: %v", err)
		return false
	}

	return member.Status == "administrator" || member.Status == "creator"
}
