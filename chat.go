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
	embyClient *EmbyClient
	ebClient   *EmbyBossClient
	kb         *KnowledgeBase
	config     *Config
}

// NewChatHandler 创建聊天处理器
func NewChatHandler(bot *tgbotapi.BotAPI, aiClient *AIClient, ctxManager *ContextManager, config *Config, kb *KnowledgeBase, embyClient *EmbyClient, ebClient *EmbyBossClient) *ChatHandler {
	return &ChatHandler{
		bot:        bot,
		aiClient:   aiClient,
		ctxManager: ctxManager,
		embyClient: embyClient,
		ebClient:   ebClient,
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
		// 添加知识库条目
		if ch.isAdmin(msg) {
			args := strings.TrimSpace(msg.CommandArguments())
			var name, content string

			if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
				// 如果是回复了某条消息，使用被回复的消息作为内容
				name = args
				content = msg.ReplyToMessage.Text
			} else {
				// 否则采用常规的用空格分隔的形式
				parts := strings.SplitN(args, " ", 2)
				if len(parts) >= 2 {
					name = parts[0]
					content = parts[1]
				}
			}

			if name == "" || content == "" {
				ch.sendReply(msg, "💡 用法:\n1. 直接发送: `/kb_add <条目名称> <内容>`\n2. 选取一条文本消息进行**回复(Reply)**，并发送: `/kb_add <条目名称>`")
				return true
			}

			// 发送正在处理提示
			initMsg := tgbotapi.NewMessage(msg.Chat.ID, "⏳ 正在由 AI 格式化并提炼知识库内容...")
			initMsg.ReplyToMessageID = msg.MessageID
			sentMsg, _ := ch.bot.Send(initMsg)

			// 调用 AI 进行内容格式化
			formatPrompt := "你现在是一个知识库整理助手。请将用户提供的以下内容进行提炼和格式化，使其最适合作为机器人的知识库（Wiki）供日后检索使用。" +
				"要求：\n1. 剔除对话中的闲聊成分，只保留核心事实或步骤。\n2. 如果适合，请尽量使用结构化的 Q&A (问答)格式或条理清晰的列表格式。\n3. 直接输出整理后的内容，不要包含任何前言或解释词汇。\n\n需要整理的内容如下：\n" + content

			formattedContent, err := ch.aiClient.ChatCompletion([]ChatMessage{
				{Role: "system", Content: "你是一个专业的知识库摘要引擎。"},
				{Role: "user", Content: formatPrompt},
			})

			if err == nil && strings.TrimSpace(formattedContent) != "" {
				content = formattedContent
			} else {
				log.Printf("[AI] 格式化知识库失败，将使用原始文本: %v", err)
			}

			if err := ch.kb.AddEntry(name, content); err != nil {
				if sentMsg.MessageID != 0 {
					editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, sentMsg.MessageID, fmt.Sprintf("❌ 添加知识库条目失败: %v", err))
					ch.bot.Send(editMsg)
				} else {
					ch.sendReply(msg, fmt.Sprintf("❌ 添加知识库条目失败: %v", err))
				}
			} else {
				successText := fmt.Sprintf("✅ 成功添加知识库条目: `%s`\n\n**内容预览:**\n%s", name, content)
				if len(successText) > 4000 {
					successText = successText[:3900] + "...\n(内容过长已折叠)"
				}
				if sentMsg.MessageID != 0 {
					editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, sentMsg.MessageID, successText)
					editMsg.ParseMode = "Markdown"
					if _, err := ch.bot.Send(editMsg); err != nil {
						editMsg.ParseMode = "" // 降级为纯文本
						ch.bot.Send(editMsg)
					}
				} else {
					ch.sendReply(msg, successText)
				}
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
		
		// 权限收束：私聊且未授权的人不可用 AI
		if msg.Chat.Type == "private" && !ch.isAuthorizedUser(msg.From.ID) {
			ch.sendReply(msg, "⚠️ 闲人免进：主管很忙，咱家只给主子私下汇报。请退下！")
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

	// 防刷与权限机制：私聊模式下必须是授权主子/管理员，如果在白名单内，直接回复
	if msg.Chat.Type == "private" {
		if !ch.isAuthorizedUser(msg.From.ID) {
			ch.sendReply(msg, "⚠️ 闲人免进：主管很忙，咱家只给主子私下汇报。请退下！")
			return false
		}
		return true // 私聊消息且有权限，直接回复，不再需要 @
	}

	// 以下为群聊逻辑...

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

// cleanMarkdownName 移除名字中的中括号或括号，防止破坏 Markdown 的 [text](url) 语法
func cleanMarkdownName(name string) string {
	name = strings.ReplaceAll(name, "[", "「")
	name = strings.ReplaceAll(name, "]", "」")
	return name
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

	// === 获取被引用的消息上下文 (ReplyContext) ===
	if msg.ReplyToMessage != nil {
		replyText := msg.ReplyToMessage.Text
		if replyText == "" {
			replyText = "[非文本或无法读取的消息]"
		}
		
		replySender := "某人"
		if msg.ReplyToMessage.From != nil {
			replySender = msg.ReplyToMessage.From.FirstName
			if msg.ReplyToMessage.From.LastName != "" {
				replySender += " " + msg.ReplyToMessage.From.LastName
			}
		}

		userText = fmt.Sprintf("（引用了 %s 的话：“%s”）\n我的回复/问题是：%s", replySender, replyText, userText)
	}
	// === 引用处理结束 ===

	chatID := msg.Chat.ID

	// 发送 "正在输入..." 状态
	typingAction := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	ch.bot.Send(typingAction)

	// 获取用户显示名称和头衔
	var userName string
	var displayRole string

	// 判断是否为频道代发或群内匿名管理员
	var senderID int64
	if msg.SenderChat != nil {
		senderID = msg.SenderChat.ID
		userName = cleanMarkdownName(msg.SenderChat.Title)

		// 如果 SenderChat 的 ID 和当前群组一致，说明是本群的“匿名管理员”
		if senderID == msg.Chat.ID {
			// 提取有可能存在的匿名头衔
			memberTitle := "匿名管理员"
			if msg.AuthorSignature != "" {
				memberTitle = msg.AuthorSignature
			}

			// 尝试组装跳转回本群的链接
			var groupLink string
			if msg.SenderChat.UserName != "" {
				groupLink = fmt.Sprintf("https://t.me/%s", msg.SenderChat.UserName)
			} else {
				chanIDStr := fmt.Sprintf("%d", senderID)
				if strings.HasPrefix(chanIDStr, "-100") {
					chanIDStr = chanIDStr[4:]
					groupLink = fmt.Sprintf("https://t.me/c/%s/1", chanIDStr)
				}
			}

			if groupLink != "" {
				displayRole = fmt.Sprintf("[%s(头衔:%s)](%s)", userName, memberTitle, groupLink)
			} else {
				displayRole = fmt.Sprintf("%s(头衔:%s)", userName, memberTitle)
			}
		} else {
			// 真正的外部关联频道发声
			if msg.SenderChat.UserName != "" {
				displayRole = fmt.Sprintf("[%s](https://t.me/%s)", userName, msg.SenderChat.UserName)
			} else {
				// 私密频道通过特殊链接跳转 (移除 -100 前缀)
				chanIDStr := fmt.Sprintf("%d", senderID)
				if strings.HasPrefix(chanIDStr, "-100") {
					chanIDStr = chanIDStr[4:]
					displayRole = fmt.Sprintf("[%s](https://t.me/c/%s/1)", userName, chanIDStr)
				} else {
					displayRole = userName
				}
			}
		}
	} else if msg.From != nil {
		senderID = msg.From.ID
		userName = msg.From.FirstName
		if msg.From.LastName != "" {
			userName += " " + msg.From.LastName
		}
		userName = cleanMarkdownName(userName)

		// 尝试获取用户在此群的专属头衔（CustomTitle）
		memberTitle := ""
		if msg.Chat.Type == "supergroup" || msg.Chat.Type == "group" {
			memberConfig := tgbotapi.GetChatMemberConfig{
				ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
					ChatID: chatID,
					UserID: senderID,
				},
			}
			if member, err := ch.bot.GetChatMember(memberConfig); err == nil {
				if member.CustomTitle != "" {
					memberTitle = cleanMarkdownName(memberTitle)
					memberTitle = member.CustomTitle
				}
			}
		}

		// 合并头衔和名字（如果有头衔的话）
		displayRole = fmt.Sprintf("[%s](tg://user?id=%d)", userName, senderID)
		if memberTitle != "" {
			displayRole = fmt.Sprintf("[%s%s](tg://user?id=%d)", memberTitle, userName, senderID)
		}
	} else {
		userName = "未知用户"
		displayRole = "未知用户"
	}

	// 检查 config.json 是否预设了该 ID 的特定身份 (覆盖之前的判定)
	var verifiedRole string
	if senderID != 0 && ch.config.AIRoles != nil {
		if roleName, exists := ch.config.AIRoles[senderID]; exists {
			displayRole = fmt.Sprintf("[%s](tg://user?id=%d)", roleName, senderID)
			verifiedRole = roleName
		}
	}

	// === 动态按需查询 EmbyBoss API ===
	var embyBossData string
	keyWords := []string{"积分", "多少钱", "花币", "余额", "账号", "我的号", "封禁", "解封", "到期", "过期", "状态"}
	needsQuery := false
	lowerUserText := strings.ToLower(userText)
	for _, kw := range keyWords {
		if strings.Contains(lowerUserText, kw) {
			needsQuery = true
			break
		}
	}

	if needsQuery && ch.ebClient != nil && senderID != 0 {
		log.Printf("[AI] 检测到用户(%d)询问个人资产，正在通过 API 请求 EmbyBoss...", senderID)
		userInfoResp, err := ch.ebClient.GetUserInfo(senderID)
		if err == nil && userInfoResp != nil {
			embyBossData = userInfoResp.FormatForAI()
			log.Printf("[AI] 成功获取用户 %d 最新数据注入上下文", senderID)
		} else {
			log.Printf("[AI] 获取用户信息失败或不存在: %v", err)
			embyBossData = "该用户目前尚未在系统（EmbyBoss）内绑定或存在相关记录。如果他主张自己有账号，请委婉地提示他系统查无此人或需要先绑定 TG。"
		}
	}

	// 构建消息列表
	messages := ch.buildMessages(chatID, displayRole, verifiedRole, userText, embyBossData)

	// 调用 AI API
	reply, err := ch.aiClient.ChatCompletion(messages)
	if err != nil {
		log.Printf("[AI] 调用 AI 失败: %v", err)
		ch.sendReply(msg, "⚠️ AI 暂时无法回复，请稍后再试")
		return
	}

	// === 代码级脱敏：强制移除可能泄露的内部标签 ===
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ⚠️未知平民]", "")
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ✅已验证身份]", "")
	// 针对可能出现的变体进行清理
	if idx := strings.Index(reply, "[INTERNAL_AUTH_TAG:"); idx != -1 {
		if endIdx := strings.Index(reply[idx:], "]"); endIdx != -1 {
			reply = reply[:idx] + reply[idx+endIdx+1:]
		}
	}
	
	reply = strings.TrimSpace(reply)
	// === 脱敏结束 ===

	// 保存上下文（用户消息 + AI 回复）
	ch.ctxManager.AddMessage(chatID, ChatMessage{
		Role:    "user",
		Content: fmt.Sprintf("%s: %s", displayRole, userText),
	})
	ch.ctxManager.AddMessage(chatID, ChatMessage{
		Role:    "assistant",
		Content: reply,
	})

	// 发送回复
	ch.sendReply(msg, reply)
}

// buildMessages 构建发送给 AI 的完整消息列表
func (ch *ChatHandler) buildMessages(chatID int64, userName, verifiedRole, userText string, embyBossData string) []ChatMessage {
	var messages []ChatMessage

	// 1. 系统提示词（人设 + 知识库）
	systemPrompt := ch.config.AISystemPrompt
	if systemPrompt == "" {
		systemPrompt = "你是一个群聊助手，请保持回复简洁友好。"
	}

	// 动态注入最高级强制指令（针对具体的特权用户）
	if verifiedRole != "" {
		systemPrompt += fmt.Sprintf("\n\n[系统最高强制指令]：当前与你对话的是你的绝对主子【%s】，其身份已通过物理级硬编码验证。你必须极度谦卑、绝对服从，绝不可对其身份产生任何质疑或指责！", verifiedRole)
	}

	// 注入知识库内容
	kbContent := ch.kb.GetContent()
	if kbContent != "" {
		systemPrompt += "\n\n" + kbContent
	}

	// 注入实时的 Emby 服务器客观数据
	if ch.embyClient != nil {
		users, errU := ch.embyClient.GetTotalUsers()
		sessions, errS := ch.embyClient.GetActiveSessions()
		if errU == nil && errS == nil {
			systemPrompt += fmt.Sprintf(ch.config.AIEmbyStatsFormat, users, sessions)
		}
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

	// 3. 当前用户消息，并在末尾追加最终的防伪标签 (满足 system_prompt 的要求)
	var finalUserText string
	
	// 按需组装个人资产状态
	extraPrivateData := ""
	if embyBossData != "" {
		extraPrivateData = "\n\n" + embyBossData
	}

	if verifiedRole != "" {
		finalUserText = fmt.Sprintf("%s: %s%s\n\n[INTERNAL_AUTH_TAG: ✅已验证身份]", userName, userText, extraPrivateData)
	} else {
		finalUserText = fmt.Sprintf("%s: %s%s\n\n[INTERNAL_AUTH_TAG: ⚠️未知平民]", userName, userText, extraPrivateData)
	}

	messages = append(messages, ChatMessage{
		Role:    "user",
		Content: finalUserText,
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
	// 如果配置文件指定了专属管理员名单，优先检查白名单
	if len(ch.config.BotAdmins) > 0 {
		for _, adminID := range ch.config.BotAdmins {
			if msg.From.ID == adminID {
				return true
			}
		}
		// 配置了名单但自己不在名单内，直接拒绝
		return false
	}

	// 如果未指定特定 ID 名单，回退为允许所有群管理员操作
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

// isAuthorizedUser 检查用户是否在白名单中（Bot管理员或配置的专属AI角色）
func (ch *ChatHandler) isAuthorizedUser(userID int64) bool {
	// 检查是否在 BotAdmins 名单中
	if len(ch.config.BotAdmins) > 0 {
		for _, adminID := range ch.config.BotAdmins {
			if userID == adminID {
				return true
			}
		}
	}

	// 检查是否在预设身份 (AI_Roles) 中，比如皇帝/皇后
	if ch.config.AIRoles != nil {
		if _, exists := ch.config.AIRoles[userID]; exists {
			return true
		}
	}

	return false
}
