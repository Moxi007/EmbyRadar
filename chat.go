package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ChatHandler 处理群聊消息与 AI 回复
type ChatHandler struct {
	bot        *tgbotapi.BotAPI
	aiClient   *AIClient                 // 全局共享的 AI 客户端
	ctxManager *ContextManager           // 全局共享，内部按 chatID 隔离
	appConfig  *AppConfig                // 全局应用配置
	kbMap      map[int64]*KnowledgeBase  // chatID → 独立知识库
	embyMap    map[int64]*EmbyClient     // chatID → 独立 Emby 客户端
	ebMap      map[int64]*EmbyBossClient // chatID → 独立 EmbyBoss 客户端
}

// NewChatHandler 创建聊天处理器，遍历所有群组配置初始化各群组的独立客户端
func NewChatHandler(bot *tgbotapi.BotAPI, aiClient *AIClient, ctxManager *ContextManager, appConfig *AppConfig) *ChatHandler {
	ch := &ChatHandler{
		bot:        bot,
		aiClient:   aiClient,
		ctxManager: ctxManager,
		appConfig:  appConfig,
		kbMap:      make(map[int64]*KnowledgeBase),
		embyMap:    make(map[int64]*EmbyClient),
		ebMap:      make(map[int64]*EmbyBossClient),
	}

	// 遍历所有群组配置，为每个群组初始化独立的客户端实例
	for _, g := range appConfig.Groups {
		chatID := g.TelegramChatID

		// 初始化 Emby 客户端（仅当配置了 EmbyURL 时）
		if g.EmbyURL != "" {
			ch.embyMap[chatID] = NewEmbyClient(g.EmbyURL, g.EmbyAPIKey)
		}

		// 初始化 EmbyBoss 客户端（仅当配置了 EmbyBossAPIToken 时）
		if g.EmbyBossAPIToken != "" {
			ch.ebMap[chatID] = NewEmbyBossClient(g.EmbyBossAPIUrl, g.EmbyBossAPIToken)
		}

		// 初始化知识库实例并加载
		kb := NewKnowledgeBase(g.AIKnowledgeDir)
		if err := kb.Load(); err != nil {
			log.Printf("[知识库] 群组 %d 加载知识库失败: %v", chatID, err)
		}
		ch.kbMap[chatID] = kb
	}

	return ch
}

// getCurrencyName 动态获取货币名称，优先从 EmbyBoss API 获取，
// 失败时回退到本地配置值，确保前后端货币名称一致
func (ch *ChatHandler) getCurrencyName(chatID int64) string {
	if eb := ch.ebMap[chatID]; eb != nil {
		if name, err := eb.GetCurrencyName(); err == nil {
			return name
		}
		log.Printf("[ChatHandler] 从 EmbyBoss API 获取货币名称失败，回退到本地配置值")
	}
	// 回退到群组配置中的本地货币名称
	if g := ch.appConfig.GetGroupConfig(chatID); g != nil {
		return g.EmbyBossCurrencyName
	}
	return "鸡蛋"
}

// StartListening 启动消息监听（长轮询）
func (ch *ChatHandler) StartListening() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	// 显式声明需要接收 my_chat_member 更新，用于检测 Bot 被添加到群组的事件
	u.AllowedUpdates = []string{"message", "my_chat_member"}

	updates := ch.bot.GetUpdatesChan(u)

	log.Printf("[AI] 消息监听已启动，等待群聊消息...")

	for update := range updates {
		// 处理 Bot 被添加到群组的事件（入群限制）
		if update.MyChatMember != nil {
			ch.handleMyChatMember(update.MyChatMember)
			continue
		}

		if update.Message == nil {
			continue
		}

		// 群组路由检查：非私聊消息必须来自已配置的群组
		if update.Message.Chat.Type != "private" {
			if !ch.appConfig.IsAuthorizedGroup(update.Message.Chat.ID) {
				continue // 忽略未配置群组的消息
			}
		}

		// 处理管理员命令（无论 AI 是否启用都处理）
		if ch.handleCommand(update.Message) {
			continue
		}

		// AI 开关检查：仅当群组启用了 AI 时才处理 AI 回复
		group := ch.appConfig.GetGroupConfig(update.Message.Chat.ID)
		aiEnabled := true // 私聊默认启用
		if group != nil {
			aiEnabled = group.AIEnabled
		}

		if aiEnabled && ch.shouldRespond(update.Message) {
			go ch.handleAIResponse(update.Message)
		}
	}
}

// handleMyChatMember 处理 Bot 成员状态变更事件，实现入群限制
func (ch *ChatHandler) handleMyChatMember(update *tgbotapi.ChatMemberUpdated) {
	newStatus := update.NewChatMember.Status
	chatID := update.Chat.ID

	// 检查 Bot 是否被添加到群组（状态变为 member/administrator/restricted）
	if newStatus == "member" || newStatus == "administrator" || newStatus == "restricted" {
		if !ch.appConfig.IsAuthorizedGroup(chatID) {
			// 未授权群组，自动退出
			log.Printf("[安全] Bot 被添加到未授权群组 (chat_id: %d)，操作者: %s (ID: %d)，正在自动退出...",
				chatID, update.From.FirstName, update.From.ID)

			leaveConfig := tgbotapi.LeaveChatConfig{ChatID: chatID}
			if _, err := ch.bot.Request(leaveConfig); err != nil {
				log.Printf("[安全] 退出未授权群组失败 (chat_id: %d): %v", chatID, err)
			} else {
				log.Printf("[安全] 已成功退出未授权群组 (chat_id: %d)", chatID)
			}
		}
	}
}

// WebhookPayload 接收从 EmbyBoss 推送过来的建号通知
type WebhookPayload struct {
	TgID     int64  `json:"tg_id"`
	TgName   string `json:"tg_name"`
	EmbyName string `json:"emby_name"`
}

// StartGroupWebhook 为单个群组启动独立的 Webhook HTTP 服务
// 每个群组使用独立端口和独立的 http.Server（不使用 DefaultServeMux）
func (ch *ChatHandler) StartGroupWebhook(group *GroupConfig) {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/new_user", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var payload WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		log.Printf("[Webhook] 群组 %d 收到新用户注册推送: TG ID=%d, TG Name=%s, Emby Name=%s",
			group.TelegramChatID, payload.TgID, payload.TgName, payload.EmbyName)

		// 异步处理新用户欢迎逻辑，传入具体群组配置
		go ch.NotifyNewEmbyUser(group, payload.TgID, payload.TgName, payload.EmbyName)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	addr := fmt.Sprintf("0.0.0.0:%d", group.WebhookPort)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	log.Printf("[Webhook] 群组 %d 开始监听端口 %d 用于接收内部推送...", group.TelegramChatID, group.WebhookPort)
	if err := server.ListenAndServe(); err != nil {
		log.Printf("[Err] 群组 %d Webhook 服务启动失败 (端口 %d): %v", group.TelegramChatID, group.WebhookPort, err)
	}
}

// NotifyNewEmbyUser 在发送贴纸并调用 AI 欢迎新主子/平民
func (ch *ChatHandler) NotifyNewEmbyUser(group *GroupConfig, tgID int64, tgName string, embyName string) {
	tgName = cleanMarkdownName(tgName)

	// 发送预设好的贴纸（如果在配置中设置了）
	if group.WelcomeStickerID != "" {
		stickerMsg := tgbotapi.NewSticker(group.TelegramChatID, tgbotapi.FileID(group.WelcomeStickerID))
		if _, err := ch.bot.Send(stickerMsg); err != nil {
			log.Printf("[AI] 发送欢迎贴纸失败: %v", err)
		}
	}

	// 触发 AI 的欢迎语
	displayRole := fmt.Sprintf("[%s](tg://user?id=%d)", tgName, tgID)

	welcomePrompt := ""
	if embyName != "" {
		welcomePrompt = fmt.Sprintf(group.WelcomeEmbyPrompt, displayRole, embyName)
	} else {
		welcomePrompt = fmt.Sprintf(group.WelcomeCodePrompt, displayRole)
	}

	// 构建一次性消息调用 AI，赋予"系统最高权限"以防被 AI 当作伪造消息拦截
	// 欢迎消息不涉及媒体内容，media 传 nil
	messages := ch.buildMessages(group.TelegramChatID, "系统通报", "系统最高权限", welcomePrompt, "", nil)

	replyMsg, err := ch.aiClient.ChatCompletion(messages, nil)
	if err != nil {
		log.Printf("[AI] 欢迎新成员调用 AI 失败: %v", err)
		return
	}
	reply := replyMsg.Content.Text

	// === 代码级脱敏：强制移除可能泄露的内部标签 ===
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ⚠️未知平民]", "")
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ✅已验证身份]", "")
	if idx := strings.Index(reply, "[INTERNAL_AUTH_TAG:"); idx != -1 {
		if endIdx := strings.Index(reply[idx:], "]"); endIdx != -1 {
			reply = reply[:idx] + reply[idx+endIdx+1:]
		}
	}
	reply = strings.TrimSpace(reply)
	// === 脱敏结束 ===

	// 保存上下文让 AI 记住这个新来的
	ch.ctxManager.AddMessage(group.TelegramChatID, ChatMessage{
		Role:    "user",
		Content: MessageContent{Text: fmt.Sprintf("系统通报: %s", welcomePrompt)},
	})
	ch.ctxManager.AddMessage(group.TelegramChatID, ChatMessage{
		Role:    "assistant",
		Content: MessageContent{Text: reply},
	})

	// 发送 AI 回复到对应群组
	replyMsgText := tgbotapi.NewMessage(group.TelegramChatID, reply)
	replyMsgText.ParseMode = "Markdown"
	if _, err := ch.bot.Send(replyMsgText); err != nil {
		replyMsgText.ParseMode = ""
		ch.bot.Send(replyMsgText)
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
			kb := ch.kbMap[msg.Chat.ID]
			if kb == nil {
				ch.sendReply(msg, "❌ 当前群组未配置知识库")
				return true
			}
			if err := kb.Reload(); err != nil {
				ch.sendReply(msg, fmt.Sprintf("❌ 重载知识库失败: %v", err))
			} else {
				ch.sendReply(msg, "✅ 知识库已重新加载")
			}
		}
		return true

	case "kb_list":
		// 列出所有知识库条目
		if ch.isAdmin(msg) {
			kb := ch.kbMap[msg.Chat.ID]
			if kb == nil {
				ch.sendReply(msg, "❌ 当前群组未配置知识库")
				return true
			}
			entries, err := kb.ListEntries()
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
			kb := ch.kbMap[msg.Chat.ID]
			if kb == nil {
				ch.sendReply(msg, "❌ 当前群组未配置知识库")
				return true
			}

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

			formattedMsg, err := ch.aiClient.ChatCompletion([]ChatMessage{
				{Role: "system", Content: MessageContent{Text: "你是一个专业的知识库摘要引擎。"}},
				{Role: "user", Content: MessageContent{Text: formatPrompt}},
			}, nil)

			formattedContent := ""
			if err == nil && formattedMsg != nil {
				formattedContent = formattedMsg.Content.Text
			}

			if err == nil && strings.TrimSpace(formattedContent) != "" {
				content = formattedContent
			} else {
				log.Printf("[AI] 格式化知识库失败，将使用原始文本: %v", err)
			}

			isNew, err := kb.MergeEntry(name, content, ch.aiClient)
			if err != nil {
				if sentMsg.MessageID != 0 {
					editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, sentMsg.MessageID, fmt.Sprintf("❌ 添加知识库条目失败: %v", err))
					ch.bot.Send(editMsg)
				} else {
					ch.sendReply(msg, fmt.Sprintf("❌ 添加知识库条目失败: %v", err))
				}
			} else {
				var successText string
				if isNew {
					successText = fmt.Sprintf("✅ 成功创建新知识库条目: `%s`\n\n**内容预览:**\n%s", name, content)
				} else {
					successText = fmt.Sprintf("✅ 已合并到现有知识库条目: `%s`\n\n**内容预览:**\n%s", name, content)
				}
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
			kb := ch.kbMap[msg.Chat.ID]
			if kb == nil {
				ch.sendReply(msg, "❌ 当前群组未配置知识库")
				return true
			}

			name := strings.TrimSpace(msg.CommandArguments())
			if name == "" {
				ch.sendReply(msg, "💡 用法: `/kb_del <条目名称>`\n可以通过 `/kb_list` 查看现有条目。")
				return true
			}

			if err := kb.DeleteEntry(name); err != nil {
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

	// 统一提取消息文本：优先 msg.Text，其次 msg.Caption（图片/视频说明文字）
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	// 判断消息是否包含媒体内容（图片或视频）
	hasMedia := len(msg.Photo) > 0 || msg.Video != nil

	// 如果既无文本也无媒体，忽略该消息
	if text == "" && !hasMedia {
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

	// 检查是否 @ 了 Bot（同时检查 msg.Entities 和 msg.CaptionEntities）
	allEntities := msg.Entities
	entitySource := msg.Text
	if len(allEntities) == 0 && msg.CaptionEntities != nil {
		allEntities = msg.CaptionEntities
		entitySource = msg.Caption
	}
	if allEntities != nil && entitySource != "" {
		for _, entity := range allEntities {
			if entity.Type == "mention" {
				mention := entitySource[entity.Offset : entity.Offset+entity.Length]
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

	// 从群组配置获取触发关键词（同时检查 text，可能来自 Caption）
	group := ch.appConfig.GetGroupConfig(msg.Chat.ID)
	if group != nil && len(group.AITriggerKeywords) > 0 {
		lowerText := strings.ToLower(text)
		for _, keyword := range group.AITriggerKeywords {
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
	// 根据 chatID 获取群组配置，未匹配时回退到第一个群组（私聊场景等）
	group := ch.appConfig.GetGroupConfig(msg.Chat.ID)
	if group == nil {
		group = ch.appConfig.Groups[0]
	}

	// 提取用户消息文本：优先 msg.Text，其次 msg.Caption（图片/视频说明文字）
	userText := msg.Text
	if userText == "" && msg.Caption != "" {
		userText = msg.Caption
	}

	// 如果是 /ask 命令，提取参数部分
	if msg.IsCommand() && msg.Command() == "ask" {
		userText = msg.CommandArguments()
	}

	// 去掉 @botname 的部分
	userText = ch.cleanMention(userText)

	// 提取媒体内容（图片或视频），返回 nil 表示无可处理的媒体
	var media *MediaContent
	media = extractMedia(ch.bot, msg)

	// 如果既无文本也无媒体，直接返回
	if strings.TrimSpace(userText) == "" && media == nil {
		return
	}

	// 有媒体但无任何文字时，使用默认提示词
	if strings.TrimSpace(userText) == "" && media != nil {
		if media.IsVideo {
			userText = "请分析这个视频"
		} else {
			userText = "请分析这张图片"
		}
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

		userText = fmt.Sprintf("（引用了 %s 的话：\u201c%s\u201d）\n我的回复/问题是：%s", replySender, replyText, userText)
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

		// 如果 SenderChat 的 ID 和当前群组一致，说明是本群的"匿名管理员"
		if senderID == msg.Chat.ID {
			memberTitle := "匿名管理员"
			if msg.AuthorSignature != "" {
				memberTitle = msg.AuthorSignature
			}

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

	// 检查群组配置中是否预设了该 ID 的特定身份（覆盖之前的判定）
	var verifiedRole string
	if senderID != 0 && group.AIRoles != nil {
		if roleName, exists := group.AIRoles[senderID]; exists {
			displayRole = fmt.Sprintf("[%s](tg://user?id=%d)", roleName, senderID)
			verifiedRole = roleName
		}
	}

	// === 动态按需查询 EmbyBoss API ===
	var embyBossData string
	keyWords := []string{"积分", "多少钱", "花币", "余额", "账号", "我的号", "封禁", "解封", "到期", "过期", "状态", "鸡蛋", "播放", "时长", "查一下", "看一下", "看看"}
	needsQuery := false
	lowerUserText := strings.ToLower(userText)
	for _, kw := range keyWords {
		if strings.Contains(lowerUserText, kw) {
			needsQuery = true
			break
		}
	}

	if needsQuery && ch.ebMap[chatID] != nil {
		// 优先判断：如果是回复了某人的消息并且在问"他/她"的信息，查被回复者
		// 否则查发消息的人自己
		queryID := senderID
		queryLabel := "自己"

		if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
			askAboutOther := false
			otherKeywords := []string{"他的", "她的", "他", "她", "这个人", "此人", "对方", "看一下", "看看", "查一下", "查查"}
			for _, ok := range otherKeywords {
				if strings.Contains(lowerUserText, ok) {
					askAboutOther = true
					break
				}
			}
			myKeywords := []string{"我的", "我有", "我还"}
			askAboutSelf := false
			for _, mk := range myKeywords {
				if strings.Contains(lowerUserText, mk) {
					askAboutSelf = true
					break
				}
			}

			if !askAboutSelf || askAboutOther {
				queryID = msg.ReplyToMessage.From.ID
				replyName := msg.ReplyToMessage.From.FirstName
				if msg.ReplyToMessage.From.LastName != "" {
					replyName += " " + msg.ReplyToMessage.From.LastName
				}
				queryLabel = replyName
			}
		}

		if queryID != 0 {
			log.Printf("[AI] 检测到询问用户(%d: %s)的资产/状态，正在通过 API 请求 EmbyBoss...", queryID, queryLabel)
			// 动态获取货币名称，优先使用 API 返回值，失败时回退到本地配置
			currencyName := ch.getCurrencyName(chatID)
			userInfoResp, err := ch.ebMap[chatID].GetUserInfo(queryID)
			if err == nil && userInfoResp != nil {
				if queryID == senderID {
					embyBossData = userInfoResp.FormatForAI(currencyName)
				} else {
					embyBossData = fmt.Sprintf("【内部系统数据查询结果 - 查询对象: %s (TG ID: %d)】：该用户的系统绑定账号名为「%s」，目前的可用资产余额为 %d %s。其账号当前所处状态判定为「%s」，此账号的过期时间戳记录为 %s。请在回答时根据这些准确数据为其解答疑问，不可凭空捏造数据。注意：货币单位必须严格使用「%s」，禁止自行替换为其他名称。",
						queryLabel, queryID, userInfoResp.Data.Name, userInfoResp.Data.Iv, currencyName,
						func() string {
							if userInfoResp.Data.Lv == "c" {
								return "封禁状态(被禁止登录)"
							}
							return "正常"
						}(),
						userInfoResp.Data.Ex,
						currencyName)
				}
				log.Printf("[AI] 成功获取用户 %d (%s) 最新数据注入上下文", queryID, queryLabel)
			} else {
				log.Printf("[AI] 获取用户信息失败或不存在: %v", err)
				embyBossData = fmt.Sprintf("系统（EmbyBoss）中未查到 %s (TG ID: %d) 的任何绑定数据。可能该用户尚未在系统内注册/绑定TG，或该TG ID无对应Emby账号记录。", queryLabel, queryID)
			}
		}
	}

	// 构建消息列表，传入媒体内容（无媒体时 media 为 nil）
	messages := ch.buildMessages(chatID, displayRole, verifiedRole, userText, embyBossData, media)

	// 准备工具配置
	var tools []Tool
	if group.AISearchEnabled {
		modelName := strings.ToLower(ch.appConfig.Global.AIModel)
		if strings.Contains(modelName, "gemini") {
			// Gemini 系列模型注入原生 Google Search Grounding 参数
			tools = append(tools, Tool{
				Type:         "google_search",
				GoogleSearch: map[string]any{},
			})
			log.Printf("[AI] 检测到 Gemini 模型，注入原生 Google Search Grounding 参数...")
		} else {
			// 普通模型使用自定义的 DuckDuckGo 爬虫 Function Calling
			currentDateStr := time.Now().Format("2006年01月")
			tools = append(tools, Tool{
				Type: "function",
				Function: &ToolFunction{
					Name:        "search_web",
					Description: fmt.Sprintf("必须使用此工具来获取最新的资讯和新闻。当前时间是 %s，你的搜索关键词中必须主动携带 '%s' 或者具体日期作为检索词，否则你会搜到过时的旧新闻！", currentDateStr, currentDateStr),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"query": map[string]any{
								"type":        "string",
								"description": fmt.Sprintf("进行搜索引擎查询的关键词。务必包含时间如 '%s' 以保证时效性。", currentDateStr),
							},
						},
						"required": []string{"query"},
					},
				},
			})
			log.Printf("[AI] 启用本地 DuckDuckGo search_web 工具...")
		}
	}

	// 循环处理 AI 的响应（支持多次连续工具调用）
	var reply string
	for i := 0; i < 5; i++ { // 最多允许连续调用5次工具
		aiMsg, err := ch.aiClient.ChatCompletion(messages, tools)
		if err != nil {
			log.Printf("[AI] 调用 AI 失败: %v", err)
			ch.sendReply(msg, "⚠️ AI 暂时无法回复，请稍后再试")
			return
		}

		// 将 AI 的响应加入消息列表
		messages = append(messages, *aiMsg)

		// 检查是否有工具调用
		if len(aiMsg.ToolCalls) > 0 {
			for _, tc := range aiMsg.ToolCalls {
				if tc.Function.Name == "search_web" {
					var args map[string]any
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						log.Printf("[AI] 解析参数失败: %v", err)
						continue
					}

					query, ok := args["query"].(string)
					if !ok {
						log.Printf("[AI] 参数类型错误，query 不是 string")
						continue
					}

					log.Printf("[AI] 【触发网络搜索】关键词: %s", query)
					searchResult := SearchWeb(query)

					// 将工具执行的结果作为 role="tool" 加回 messages
					messages = append(messages, ChatMessage{
						Role:       "tool",
						ToolCallID: tc.ID,
						Name:       tc.Function.Name,
						Content:    MessageContent{Text: searchResult},
					})
				}
			}
			// 继续进行下一次请求，让 AI 总结搜索结果
			continue
		}

		// 没有工具调用，正常返回文本
		reply = aiMsg.Content.Text
		break
	}

	if reply == "" {
		reply = "（思考了很久，不知道该说什么）"
	}

	// === 代码级脱敏：强制移除可能泄露的内部标签 ===
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ⚠️未知平民]", "")
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ✅已验证身份]", "")
	if idx := strings.Index(reply, "[INTERNAL_AUTH_TAG:"); idx != -1 {
		if endIdx := strings.Index(reply[idx:], "]"); endIdx != -1 {
			reply = reply[:idx] + reply[idx+endIdx+1:]
		}
	}

	reply = strings.TrimSpace(reply)
	// === 脱敏结束 ===

	// 保存上下文（用户消息 + AI 回复）
	// 多模态消息降级为纯文本描述，避免历史上下文中包含 Base64 编码数据
	contextUserText := userText
	if media != nil {
		if media.IsVideo {
			contextUserText = "[用户发送了一个视频] " + userText
		} else {
			contextUserText = "[用户发送了一张图片] " + userText
		}
	}
	ch.ctxManager.AddMessage(chatID, ChatMessage{
		Role:    "user",
		Content: MessageContent{Text: fmt.Sprintf("%s: %s", displayRole, contextUserText)},
	})
	ch.ctxManager.AddMessage(chatID, ChatMessage{
		Role:    "assistant",
		Content: MessageContent{Text: reply},
	})

	// 发送回复
	ch.sendReply(msg, reply)
}

// buildMessages 构建发送给 AI 的完整消息列表。
// media 参数用于传递多模态内容（图片/视频），为 nil 时保持纯文本模式。
func (ch *ChatHandler) buildMessages(chatID int64, userName, verifiedRole, userText string, embyBossData string, media *MediaContent) []ChatMessage {
	// 根据 chatID 获取群组配置，未匹配时回退到第一个群组
	group := ch.appConfig.GetGroupConfig(chatID)
	if group == nil {
		group = ch.appConfig.Groups[0]
	}

	var messages []ChatMessage

	// 1. 系统提示词（人设 + 知识库）
	systemPrompt := group.AISystemPrompt
	if systemPrompt == "" {
		systemPrompt = "你是一个群聊助手，请保持回复简洁友好。"
	}

	// 注入当前准确时间服务器时间
	currentTime := time.Now().Format("2006年01月02日 15:04:05")
	systemPrompt += fmt.Sprintf("\n\n[系统硬参]：当前服务器精确时间为：%s。当用户询问时间、日期、星期几或者判断今天、明天、昨天时，请务必以此时间为基准进行回答。", currentTime)

	// 动态注入最高级强制指令（针对具体的特权用户）
	if verifiedRole != "" {
		systemPrompt += fmt.Sprintf("\n\n[系统最高强制指令]：当前与你对话的是你的绝对主子【%s】，其身份已通过物理级硬编码验证。你必须极度谦卑、绝对服从，绝不可对其身份产生任何质疑或指责！", verifiedRole)
	}

	// 注入知识库内容（从群组级知识库获取）
	if kb := ch.kbMap[chatID]; kb != nil {
		kbContent := kb.GetContent()
		if kbContent != "" {
			systemPrompt += "\n\n" + kbContent
		}
	}

	// 注入实时的 Emby 服务器客观数据（从群组级 Emby 客户端获取）
	if embyClient := ch.embyMap[chatID]; embyClient != nil {
		users, errU := embyClient.GetTotalUsers()
		sessions, errS := embyClient.GetActiveSessions()
		if errU == nil && errS == nil {
			if group.AIEmbyStatsFormat != "" {
				systemPrompt += fmt.Sprintf(group.AIEmbyStatsFormat, users, sessions)
			}
		}
	}

	messages = append(messages, ChatMessage{
		Role:    "system",
		Content: MessageContent{Text: systemPrompt},
	})

	// 2. 历史上下文
	history := ch.ctxManager.GetMessages(chatID)
	if history != nil {
		messages = append(messages, history...)
	}

	// 3. 当前用户消息，并在末尾追加最终的防伪标签
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

	// 根据是否有媒体内容决定消息格式：
	// 有媒体时使用 OpenAI Vision 格式的 content 数组，否则保持纯文本
	if media != nil {
		dataURL := fmt.Sprintf("data:%s;base64,%s", media.MIMEType, media.Base64Data)
		messages = append(messages, ChatMessage{
			Role: "user",
			Content: MessageContent{
				Parts: []ContentPart{
					{Type: "text", Text: finalUserText},
					{Type: "image_url", ImageURL: &ImageURL{URL: dataURL}},
				},
			},
		})
	} else {
		messages = append(messages, ChatMessage{
			Role:    "user",
			Content: MessageContent{Text: finalUserText},
		})
	}

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
	// 如果全局配置指定了专属管理员名单，优先检查白名单
	if len(ch.appConfig.Global.BotAdmins) > 0 {
		for _, adminID := range ch.appConfig.Global.BotAdmins {
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

// isAuthorizedUser 检查用户是否在白名单中（Bot 管理员或配置的专属 AI 角色）
func (ch *ChatHandler) isAuthorizedUser(userID int64) bool {
	// 检查是否在全局 BotAdmins 名单中
	if len(ch.appConfig.Global.BotAdmins) > 0 {
		for _, adminID := range ch.appConfig.Global.BotAdmins {
			if userID == adminID {
				return true
			}
		}
	}

	// 遍历所有群组的 AIRoles，检查用户是否在任一群组中有角色
	for _, g := range ch.appConfig.Groups {
		if g.AIRoles != nil {
			if _, exists := g.AIRoles[userID]; exists {
				return true
			}
		}
	}

	return false
}
