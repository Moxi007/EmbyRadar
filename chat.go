package main

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ChatHandler 处理群聊消息与 AI 回复
type ChatHandler struct {
	bot            *tgbotapi.BotAPI
	aiClient       *AIClient                 // 全局共享的 AI 客户端
	ctxManager     *ContextManager           // 全局共享，内部按 chatID 隔离
	appConfig      *AppConfig                // 全局应用配置
	globalKB       *KnowledgeBase            // 通用知识库，所有群组共享
	kbMap          map[int64]*KnowledgeBase  // chatID → 独立知识库
	embyMap        map[int64]*EmbyClient     // chatID → 独立 Emby 客户端
	ebMap          map[int64]*EmbyBossClient // chatID → 独立 EmbyBoss 客户端
	tmdbMap        map[int64]*TMDBClient     // chatID → 独立 TMDB 客户端
	requestHandler *RequestHandler           // 全局求片处理器
	memoryStore    *MemoryStore              // 向量记忆存储 (可为 nil)

	// 架构增强组件
	sessionMgr      *SessionManager   // 会话队列管理器（防并发）
	middlewareChain *MiddlewareChain  // Prompt 中间件链
	toolRegistry    *ToolRegistry     // 工具注册中心
	skillsLoader    *SkillsLoader     // 技能加载器 (可为 nil)
	jobsLoader      *JobsLoader       // 任务加载器 (可为 nil)
}

// NewChatHandler 创建聊天处理器，遍历所有群组配置初始化各群组的独立客户端
// requestHandler 由外部创建并传入，确保 store 注入和 Poller 复用 embyMap
func NewChatHandler(bot *tgbotapi.BotAPI, aiClient *AIClient, ctxManager *ContextManager, appConfig *AppConfig, requestHandler *RequestHandler) *ChatHandler {
	ch := &ChatHandler{
		bot:            bot,
		aiClient:       aiClient,
		ctxManager:     ctxManager,
		appConfig:      appConfig,
		kbMap:          make(map[int64]*KnowledgeBase),
		embyMap:        make(map[int64]*EmbyClient),
		ebMap:          make(map[int64]*EmbyBossClient),
		tmdbMap:        make(map[int64]*TMDBClient),
		requestHandler: requestHandler,
	}

	// 初始化通用知识库（所有群组共享，路径固定为 config/knowledge）
	ch.globalKB = NewKnowledgeBase("config/knowledge")
	if err := ch.globalKB.Load(); err != nil {
		log.Printf("[知识库] 通用知识库加载失败: %v", err)
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

		// 初始化 TMDB 客户端（仅当全局配置了 TMDBAPIKey 时）
		if appConfig.Global.TMDBAPIKey != "" {
			ch.tmdbMap[chatID] = NewTMDBClient(appConfig.Global.TMDBAPIKey)
		}

		// 初始化知识库实例并加载
		kb := NewKnowledgeBase(g.AIKnowledgeDir)
		if err := kb.Load(); err != nil {
			log.Printf("[知识库] 群组 %d 加载知识库失败: %v", chatID, err)
		}
		ch.kbMap[chatID] = kb
	}

	// 初始化 Prompt 中间件链
	ch.middlewareChain = DefaultMiddlewareChain()
	ch.middlewareChain.LogMiddlewares()

	// 初始化工具注册中心
	ch.toolRegistry = DefaultToolRegistry()

	// 初始化技能加载器并注册 read_skill 工具
	ch.skillsLoader = NewSkillsLoader("config/skills")
	ch.toolRegistry.Register(&ReadSkillHandler{loader: ch.skillsLoader})

	// 初始化任务加载器并注册 read_job / write_job 工具
	ch.jobsLoader = NewJobsLoader("config/jobs")
	ch.toolRegistry.Register(&ReadJobHandler{loader: ch.jobsLoader})
	ch.toolRegistry.Register(&WriteJobHandler{loader: ch.jobsLoader})

	log.Printf("[工具注册] 已注册 %d 个工具 (含技能和任务工具)", len(ch.toolRegistry.order))

	// 初始化会话队列管理器（用于防并发）
	ch.sessionMgr = NewSessionManager(ch.handleAIResponse)
	log.Printf("[会话队列] 会话管理器已初始化")

	return ch
}

// SetMemoryStore 注入全局向量记忆存储
func (ch *ChatHandler) SetMemoryStore(store *MemoryStore) {
	ch.memoryStore = store
}

// getCurrencyName 获取群组配置的货币名称
func (ch *ChatHandler) getCurrencyName(chatID int64) string {
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
	u.AllowedUpdates = []string{"message", "my_chat_member", "callback_query"}

	updates := ch.bot.GetUpdatesChan(u)

	log.Printf("[AI] 消息监听已启动，等待群聊消息...")

	for update := range updates {
		// 处理 Bot 被添加到群组的事件（入群限制）
		if update.MyChatMember != nil {
			ch.handleMyChatMember(update.MyChatMember)
			continue
		}

		// 处理管理员审批回调、用户选择 TMDB 结果回调、AI 求片确认回调
		if update.CallbackQuery != nil {
			if strings.HasPrefix(update.CallbackQuery.Data, "request:") {
				ch.requestHandler.HandleCallbackQuery(ch, update.CallbackQuery)
			} else if strings.HasPrefix(update.CallbackQuery.Data, "reqsel:") {
				go ch.requestHandler.HandleSelectCallback(ch, update.CallbackQuery)
			} else if strings.HasPrefix(update.CallbackQuery.Data, "reqai:") {
				go ch.requestHandler.HandleAIConfirmCallback(ch, update.CallbackQuery)
			}
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

		// 检测求片意图关键词（@ Bot + 求片关键词），优先于命令和 AI 回复处理
		if ch.detectRequestIntent(update.Message) {
			go ch.requestHandler.HandleRequest(ch, update.Message, update.Message.Text)
			continue
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
			ch.sessionMgr.Enqueue(update.Message)
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
	reply = strings.ReplaceAll(reply, "⚠️未知平民", "")
	reply = strings.ReplaceAll(reply, "✅已验证身份", "")
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
			// 私聊时操作通用知识库，群聊时操作群组知识库
			kb := ch.kbMap[msg.Chat.ID]
			if msg.Chat.Type == "private" {
				kb = ch.globalKB
			}
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
			if msg.Chat.Type == "private" {
				kb = ch.globalKB
			}
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
			if msg.Chat.Type == "private" {
				kb = ch.globalKB
			}
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
			if msg.Chat.Type == "private" {
				kb = ch.globalKB
			}
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

		ch.sessionMgr.Enqueue(msg)
		return true

	case "request":
		// /request 命令：提交求片请求
		group := ch.appConfig.GetGroupConfig(msg.Chat.ID)
		if group == nil || ch.appConfig.Global.TMDBAPIKey == "" {
			ch.sendReply(msg, "⚠️ 当前群组未配置 TMDB，无法使用求片功能")
			return true
		}
		if !group.RequestEnabled {
			ch.sendReply(msg, "⚠️ 当前群组未开启求片功能")
			return true
		}
		args := msg.CommandArguments()
		if args == "" {
			ch.sendReply(msg, "💡 用法: /request <影视名称/TMDB链接/豆瓣链接>")
			return true
		}
		go ch.requestHandler.HandleRequest(ch, msg, args)
		return true

	case "request_coin_cost":
		// 查看或设置求片货币费用，仅群聊中有效
		if msg.Chat.Type == "private" {
			return true
		}
		group := ch.appConfig.GetGroupConfig(msg.Chat.ID)
		if group == nil {
			return true
		}
		currencyName := ch.getCurrencyName(msg.Chat.ID)
		args := strings.TrimSpace(msg.CommandArguments())
		if args == "" {
			// 无参数：所有人可查看当前费用
			ch.sendReply(msg, fmt.Sprintf("当前求片费用: %d %s", group.RequestCoinCost, currencyName))
			return true
		}
		// 带参数：仅管理员可修改
		if !ch.isAdmin(msg) {
			return true
		}
		cost, err := strconv.Atoi(args)
		if err != nil {
			ch.sendReply(msg, "请输入有效的数字")
			return true
		}
		if cost < 0 {
			ch.sendReply(msg, fmt.Sprintf("%s费用不能为负数", currencyName))
			return true
		}
		group.RequestCoinCost = cost
		if err := ch.appConfig.SaveConfig("config/config.json"); err != nil {
			log.Printf("[ChatHandler] 保存配置文件失败: %v", err)
			ch.sendReply(msg, fmt.Sprintf("❌ 保存配置失败: %v", err))
			return true
		}
		ch.sendReply(msg, fmt.Sprintf("✅ 求片费用已设置为 %d %s", cost, currencyName))
		return true

	case "img":
		// 图片生成命令，需同时启用 AI 总开关和图片生成开关
		group := ch.appConfig.GetGroupConfig(msg.Chat.ID)
		if group == nil || !group.AIEnabled || !group.AIImageEnabled {
			return true // 功能未启用，静默忽略
		}
		prompt := msg.CommandArguments()
		if prompt == "" {
			ch.sendReply(msg, "💡 用法: /img <图片描述>")
			return true
		}
		go ch.handleImageGeneration(msg, prompt, group)
		return true
	}

	return false
}

// handleImageGeneration 处理图片生成请求（/img 命令和工具调用共用）
func (ch *ChatHandler) handleImageGeneration(msg *tgbotapi.Message, prompt string, group *GroupConfig) error {
	// 发送"正在上传图片"状态，让用户知道请求正在处理
	photoAction := tgbotapi.NewChatAction(msg.Chat.ID, tgbotapi.ChatUploadPhoto)
	ch.bot.Send(photoAction)

	// 调用 AI 图片生成
	imgData, err := ch.aiClient.GenerateImage(prompt, group.AIImageModel, group.AIImageSize)
	if err != nil {
		log.Printf("[AI] 图片生成失败: %v", err)
		// 根据错误类型返回不同的用户提示
		switch {
		case errors.Is(err, ErrContentPolicy):
			ch.sendReply(msg, "⚠️ 描述内容不符合安全策略，请修改后重试")
		case errors.Is(err, ErrRateLimitExhausted):
			ch.sendReply(msg, "⚠️ 图片生成服务繁忙，请稍后重试")
		case errors.Is(err, ErrImageDecode):
			ch.sendReply(msg, "⚠️ 图片数据处理失败，请重试")
		default:
			ch.sendReply(msg, "⚠️ 图片生成失败，请稍后重试")
		}
		return err
	}

	// 通过 Telegram Bot API 发送图片，引用用户的原始命令消息
	photoFile := tgbotapi.FileBytes{Name: "generated.png", Bytes: imgData}
	photoMsg := tgbotapi.NewPhoto(msg.Chat.ID, photoFile)
	photoMsg.ReplyToMessageID = msg.MessageID
	if _, err := ch.bot.Send(photoMsg); err != nil {
		log.Printf("[AI] 发送生成图片失败: %v", err)
		ch.sendReply(msg, "⚠️ 图片发送失败，请重试")
		return err
	}

	return nil
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

// detectRequestIntent 检测消息是否包含求片意图关键词
// 必须同时满足：群组配置了 TMDB 和求片功能、消息 @ 了 Bot、消息包含求片关键词
func (ch *ChatHandler) detectRequestIntent(msg *tgbotapi.Message) bool {
	// 仅处理群聊消息
	if msg.Chat.Type == "private" {
		return false
	}

	// 检查群组是否配置了 TMDB 和求片功能
	group := ch.appConfig.GetGroupConfig(msg.Chat.ID)
	if group == nil || ch.appConfig.Global.TMDBAPIKey == "" || !group.RequestEnabled {
		return false
	}

	// 提取消息文本
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	if text == "" {
		return false
	}

	// 检查消息是否 @ 了 Bot
	mentionedBot := false
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
					mentionedBot = true
					break
				}
			}
		}
	}
	if !mentionedBot {
		return false
	}

	// 检查是否包含求片意图关键词
	requestKeywords := []string{"求片", "想看", "能不能加", "有没有", "可以加"}
	for _, kw := range requestKeywords {
		if strings.Contains(text, kw) {
			return true
		}
	}

	return false
}

// cleanMarkdownName 移除名字中可能破坏 Telegram Markdown 语法的特殊字符
// 替换中括号为中文引号，转义下划线、星号、反引号等
func cleanMarkdownName(name string) string {
	name = strings.ReplaceAll(name, "[", "「")
	name = strings.ReplaceAll(name, "]", "」")
	name = strings.ReplaceAll(name, "_", "\\_")
	name = strings.ReplaceAll(name, "*", "\\*")
	name = strings.ReplaceAll(name, "`", "\\`")
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
	// 优先从当前消息提取；若当前消息无媒体，则尝试从被引用的消息中提取
	var media *MediaContent
	var mediaErr string
	media, mediaErr = extractMedia(ch.bot, msg)
	if media == nil && msg.ReplyToMessage != nil {
		media, mediaErr = extractMedia(ch.bot, msg.ReplyToMessage)
	}

	// 媒体提取失败时通知用户具体原因，避免静默忽略
	if mediaErr != "" && media == nil {
		// 如果有文本内容，仍然继续处理（仅丢失媒体部分）
		if strings.TrimSpace(userText) == "" {
			ch.sendReply(msg, mediaErr)
			return
		}
		// 有文本时在日志中记录，继续以纯文本模式处理
		log.Printf("[媒体] 提取失败但有文本，降级为纯文本: %s", mediaErr)
	}

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
			replyText = msg.ReplyToMessage.Caption
		}
		if replyText == "" {
			// 被引用消息既无文本也无说明文字，根据是否有媒体给出不同描述
			if media != nil {
				replyText = "[媒体消息]"
			} else {
				replyText = "[非文本或无法读取的消息]"
			}
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

	// 取消之前硬编码的关键字前置拦截逻辑，不再自动为每一句聊天请求用户资产。
	// 改为提供专属的 AI 工具 (get_user_info) 让 AI 在需要时自行发问。
	var embyBossData string

	// 构建消息列表，传入媒体内容（无媒体时 media 为 nil）
	messages := ch.buildMessages(chatID, displayRole, verifiedRole, userText, embyBossData, media)

	// 构建工具执行上下文
	isSensitiveAllowed := ch.canQuerySensitiveInfo(msg)
	isGroupChat := msg.Chat != nil && (msg.Chat.Type == "group" || msg.Chat.Type == "supergroup")
	toolCtx := &ToolContext{
		ChatID:                chatID,
		SenderID:              senderID,
		Msg:                   msg,
		Group:                 group,
		AppConfig:             ch.appConfig,
		IsPrivate:             !isGroupChat,
		EmbyClient:            ch.embyMap[chatID],
		EBClient:              ch.ebMap[chatID],
		TMDBClient:            ch.tmdbMap[chatID],
		AIClient:              ch.aiClient,
		ChatHandler:           ch,
		IsSensitiveAllowed:    isSensitiveAllowed,
		AllowSensitiveDetails: isSensitiveAllowed && !isGroupChat,
	}

	// 通过工具注册中心获取当前上下文可用的工具定义
	tools := ch.toolRegistry.GetEnabledTools(toolCtx)

	// Gemini 特殊处理：注入原生 Google Search Grounding（不经过 ToolRegistry）
	if group.AISearchEnabled {
		modelName := strings.ToLower(ch.appConfig.Global.AIModel)
		if strings.Contains(modelName, "gemini") {
			tools = append(tools, Tool{
				Type:         "google_search",
				GoogleSearch: map[string]any{},
			})
			log.Printf("[AI] 检测到 Gemini 模型，注入原生 Google Search Grounding 参数...")
		} else {
			log.Printf("[AI] 启用本地 DuckDuckGo search_web 工具...")
		}
	}
	if ch.tmdbMap[chatID] != nil {
		log.Printf("[AI] 启用 TMDB search_tmdb 工具...")
	}
	if group.AIImageEnabled && group.AIImageModel != "" {
		log.Printf("[AI] 启用图片生成 generate_image 工具...")
	}
	if ch.embyMap[chatID] != nil {
		if isSensitiveAllowed {
			log.Printf("[AI] 启用 Emby 管家工具集 (search, latest, playback_stats, user_info)...")
		} else {
			log.Printf("[AI] 启用 Emby 管家工具集 (search, latest, playback_stats[脱敏], user_info)")
		}
	}

	// 循环处理 AI 的响应（支持多次连续工具调用）
	var reply string
	for i := 0; i < 5; i++ {
		aiMsg, err := ch.aiClient.ChatCompletion(messages, tools)
		if err != nil {
			log.Printf("[AI] 调用 AI 失败: %v", err)
			ch.sendReply(msg, "⚠️ AI 暂时无法回复，请稍后再试")
			return
		}

		messages = append(messages, *aiMsg)

		if len(aiMsg.ToolCalls) > 0 {
			for _, tc := range aiMsg.ToolCalls {
				toolResult := ch.toolRegistry.Execute(tc.Function.Name, tc.Function.Arguments, toolCtx)
				messages = append(messages, ChatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Content:    MessageContent{Text: toolResult},
				})
			}
			continue
		}

		reply = aiMsg.Content.Text
		break
	}

	if reply == "" {
		reply = "（思考了很久，不知道该说什么）"
	}

	// === 代码级脱敏：强制移除可能泄露的内部标签 ===
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ⚠️未知平民]", "")
	reply = strings.ReplaceAll(reply, "[INTERNAL_AUTH_TAG: ✅已验证身份]", "")
	reply = strings.ReplaceAll(reply, "⚠️未知平民", "")
	reply = strings.ReplaceAll(reply, "✅已验证身份", "")
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

	// ====== [方案三：长期记忆异步归档] ======
	if ch.memoryStore != nil {
		go func(cid int64, cUserText, cReply string) {
			// 将这一轮完整的用户问题与 AI 回答应答拼合为一条完整的语义记忆
			memText := fmt.Sprintf("用户说: %s\nAI回答: %s", cUserText, cReply)
			metadata := map[string]any{
				"timestamp": time.Now().Format(time.RFC3339),
				"user_name": userName,
			}
			if err := ch.memoryStore.Store(cid, memText, metadata); err != nil {
				log.Printf("[记忆] 长期记忆写入 Qdrant 失败: %v", err)
			}
		}(chatID, contextUserText, reply)
	}

	// 发送回复：检测 AI 回复中是否包含求片确认标记
	requestConfirmRe := regexp.MustCompile(`\[REQUEST_CONFIRM:(.+?)\]`)
	if matches := requestConfirmRe.FindStringSubmatch(reply); len(matches) == 2 {
		movieName := strings.TrimSpace(matches[1])
		// 从回复中移除标记
		reply = strings.TrimSpace(requestConfirmRe.ReplaceAllString(reply, ""))

		// 构建确认求片的 Inline Keyboard 按钮
		// 回调数据格式：reqai:{chatID}:{userID}:{movieName}
		// movieName 可能较长，Telegram 回调数据限制 64 字节，需要截断
		callbackMovieName := movieName
		if len(callbackMovieName) > 30 {
			callbackMovieName = string([]rune(callbackMovieName)[:30])
		}
		cbData := fmt.Sprintf("%s:%d:%d:%s", aiConfirmCallbackPrefix, chatID, senderID, callbackMovieName)
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🎬 确认求片", cbData),
			),
		)

		replyMsg := tgbotapi.NewMessage(chatID, reply)
		replyMsg.ReplyToMessageID = msg.MessageID
		replyMsg.ParseMode = "Markdown"
		replyMsg.ReplyMarkup = keyboard
		if _, err := ch.bot.Send(replyMsg); err != nil {
			log.Printf("[AI] 发送求片确认消息失败: %v，降级为纯文本", err)
			// Markdown 解析失败时降级为纯文本
			replyMsg.ParseMode = ""
			ch.bot.Send(replyMsg)
		}
	} else {
		ch.sendReply(msg, reply)
	}
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

	// 1. 使用 Middleware 链构建系统提示词
	basePrompt := group.AISystemPrompt
	if basePrompt == "" {
		basePrompt = "你是一个群聊助手，请保持回复简洁友好。"
	}

	promptCtx := &PromptContext{
		ChatID:       chatID,
		UserText:     userText,
		SenderID:     0,
		Group:        group,
		IsPrivate:    false,
		VerifiedRole: verifiedRole,
		GlobalKB:     ch.globalKB,
		GroupKB:      ch.kbMap[chatID],
		MemoryStore:  ch.memoryStore,
		EmbyClient:   ch.embyMap[chatID],
		AppConfig:    ch.appConfig,
		SkillsLoader: ch.skillsLoader,
		JobsLoader:   ch.jobsLoader,
	}
	systemPrompt := ch.middlewareChain.BuildSystemPrompt(basePrompt, promptCtx)

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

// canQuerySensitiveInfo 检查用户是否有权限查询设备/IP等敏感信息
// 仅允许：Bot 管理员 或 群组管理员
func (ch *ChatHandler) canQuerySensitiveInfo(msg *tgbotapi.Message) bool {
	if msg == nil || msg.From == nil {
		return false
	}

	// 全局 BotAdmins 直接放行
	for _, adminID := range ch.appConfig.Global.BotAdmins {
		if msg.From.ID == adminID {
			return true
		}
	}

	// 仅群聊允许群管理员放行，私聊不放行
	if msg.Chat == nil || (msg.Chat.Type != "group" && msg.Chat.Type != "supergroup") {
		return false
	}

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
