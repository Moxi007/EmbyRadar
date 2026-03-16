package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Cache 缓存上一次发送的消息 ID 和内容，避免重复编辑消耗 API 排队
type Cache struct {
	MessageID int    `json:"message_id"`
	LastText  string `json:"last_text"`
}

// GroupStatusUpdater 单个群组的状态更新器
type GroupStatusUpdater struct {
	bot       *tgbotapi.BotAPI
	emby      *EmbyClient
	group     *GroupConfig
	cache     Cache
	cacheFile string // 每个群组独立的缓存文件路径
}

func main() {
	// 0. 初始化日志文件
	initLogger()

	// 1. 加载配置（新的多群组格式）
	appConfig, err := LoadConfig("config/config.json")
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	// 2. 初始化 Telegram Bot
	bot, err := tgbotapi.NewBotAPI(appConfig.Global.TelegramBotToken)
	if err != nil {
		log.Fatalf("初始化 Telegram Bot 失败: %v", err)
	}
	bot.Debug = false
	log.Printf("授权成功，Bot: %s", bot.Self.UserName)

	// 3. 初始化 AI 聊天模块（检查是否有任意群组启用了 AI）
	hasAIEnabled := false
	for _, g := range appConfig.Groups {
		if g.AIEnabled {
			hasAIEnabled = true
			break
		}
	}
	if hasAIEnabled {
		aiClient := NewAIClient(&appConfig.Global)
		ctxManager := NewContextManager(appConfig.Global.AIMaxContext)

		// 初始化 SQLite 数据库
		dbPath := appConfig.Global.DBPath
		if dbPath == "" {
			dbPath = "config/embyradar.db"
		}
		store, err := NewRequestStore(dbPath)
		if err != nil {
			log.Fatalf("初始化数据库失败: %v", err)
		}

		// 创建求片处理器并注入数据库访问层
		requestHandler := NewRequestHandler(store)

		chatHandler := NewChatHandler(bot, aiClient, ctxManager, appConfig, requestHandler)

		// 创建 Poller 轮询器并启动（复用 chatHandler 的 embyMap）
		poller := NewPoller(store, chatHandler.embyMap, bot, 30*time.Minute)
		poller.Start()

		// 在独立 goroutine 中启动消息监听
		go chatHandler.StartListening()

		// 启动全局统一的 Webhook 服务
		if appConfig.Global.WebhookPort > 0 {
			go StartGlobalWebhook(appConfig, chatHandler)
		}

		log.Printf("[AI] AI 聊天模块已启动 (模型: %s)", appConfig.Global.AIModel)

		// 注册快捷命令菜单
		setBotCommands(bot)

		// 监听退出信号，优雅关闭数据库和 Poller
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		// 4. 启动所有群组的状态监控
		StartAllStatusUpdaters(bot, appConfig)

		sig := <-sigCh
		log.Printf("收到退出信号 %v，正在优雅退出...", sig)
		poller.Stop()
		store.Close()
		log.Printf("数据库和轮询器已关闭，程序退出")
	} else {
		log.Printf("[AI] AI 聊天模块未启用")

		// 4. 启动所有群组的状态监控
		StartAllStatusUpdaters(bot, appConfig)

		// 监听退出信号以便优雅退出
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		sig := <-sigCh
		log.Printf("收到退出信号 %v，程序退出", sig)
	}
}

// updateStatus 获取 Emby 数据并发送或编辑该群组的置顶状态消息
func (gsu *GroupStatusUpdater) updateStatus() {
	// 获取数据
	activeSessions, err := gsu.emby.GetActiveSessions()
	if err != nil {
		log.Printf("[Err] 群组 %d 获取监控数据失败(Sessions): %v", gsu.group.TelegramChatID, err)
		return
	}
	totalUsers, err := gsu.emby.GetTotalUsers()
	if err != nil {
		log.Printf("[Err] 群组 %d 获取监控数据失败(Users): %v", gsu.group.TelegramChatID, err)
		return
	}

	// 格式化文本
	text := fmt.Sprintf(
		"📊 *%s 实时状态*\n\n"+
			"🎥 正在观看：*%d*\n"+
			"👤 用户总数：*%d*\n"+
			"📅 更新时间：*%s*",
		gsu.group.ServerName,
		activeSessions,
		totalUsers,
		time.Now().Format("15:04"),
	)

	// 如果内容没有改变，不要发送编辑请求以避免 rate limit
	if gsu.cache.LastText == text {
		return
	}

	if gsu.cache.MessageID == 0 {
		// 发送新消息并置顶
		msg := tgbotapi.NewMessage(gsu.group.TelegramChatID, text)
		msg.ParseMode = "Markdown"
		sentMsg, err := gsu.bot.Send(msg)
		if err != nil {
			log.Printf("[Err] 群组 %d 发送初始消息失败: %v", gsu.group.TelegramChatID, err)
			return
		}
		gsu.cache.MessageID = sentMsg.MessageID
		gsu.cache.LastText = text
		saveCacheToFile(gsu.cache, gsu.cacheFile)

		// 置顶消息，静默置顶不打扰群员
		pinConfig := tgbotapi.PinChatMessageConfig{
			ChatID:              gsu.group.TelegramChatID,
			MessageID:           sentMsg.MessageID,
			DisableNotification: true,
		}
		if _, err := gsu.bot.Request(pinConfig); err != nil {
			log.Printf("[Warn] 群组 %d 置顶消息失败 (请确保机器人有管理员权限): %v", gsu.group.TelegramChatID, err)
		} else {
			log.Printf("群组 %d 成功发送并置顶状态消息 ID: %d", gsu.group.TelegramChatID, sentMsg.MessageID)
		}

	} else {
		// 编辑已存在的置顶消息
		editMsg := tgbotapi.NewEditMessageText(gsu.group.TelegramChatID, gsu.cache.MessageID, text)
		editMsg.ParseMode = "Markdown"

		_, err := gsu.bot.Send(editMsg)
		if err != nil {
			log.Printf("[Err] 群组 %d 编辑消息状态失败 (ID: %d): %v", gsu.group.TelegramChatID, gsu.cache.MessageID, err)
			// 消息被删除等原因导致找不到该消息时，重置 ID
			gsu.cache.MessageID = 0
			saveCacheToFile(gsu.cache, gsu.cacheFile)
		} else {
			gsu.cache.LastText = text
			saveCacheToFile(gsu.cache, gsu.cacheFile)
		}
	}
}

// StartAllStatusUpdaters 为所有群组启动独立的状态更新 goroutine
// 仅当群组配置了 EmbyURL 时才启动状态更新
func StartAllStatusUpdaters(bot *tgbotapi.BotAPI, appConfig *AppConfig) {
	for _, g := range appConfig.Groups {
		// 未配置 Emby 地址的群组跳过状态监控
		if g.EmbyURL == "" {
			continue
		}

		emby := NewEmbyClient(g.EmbyURL, g.EmbyAPIKey)

		// 如果未配置 server_name，自动从 Emby 获取
		if g.ServerName == "" {
			if name, err := emby.GetServerName(); err == nil {
				g.ServerName = name
				log.Printf("群组 %d 自动获取服务名称: %s", g.TelegramChatID, name)
			} else {
				g.ServerName = "EMBY"
				log.Printf("[Warn] 群组 %d 获取服务名称失败，使用默认名称: %v", g.TelegramChatID, err)
			}
		}

		cacheFile := fmt.Sprintf("config/message_id_%d.json", g.TelegramChatID)
		gsu := &GroupStatusUpdater{
			bot:       bot,
			emby:      emby,
			group:     g,
			cache:     loadCacheFromFile(cacheFile),
			cacheFile: cacheFile,
		}

		// 立即执行一次
		gsu.updateStatus()

		// 启动独立的定时器 goroutine，各群组互不影响
		go func(updater *GroupStatusUpdater) {
			ticker := time.NewTicker(time.Duration(updater.group.UpdateInterval) * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				updater.updateStatus()
			}
		}(gsu)

		log.Printf("群组 %d 状态监控已启动 (间隔: %ds)", g.TelegramChatID, g.UpdateInterval)
	}
}

// loadCacheFromFile 从指定文件加载缓存的消息 ID 和文本状态
func loadCacheFromFile(filename string) Cache {
	var cache Cache
	data, err := os.ReadFile(filename)
	if err == nil {
		json.Unmarshal(data, &cache)
	}
	return cache
}

// saveCacheToFile 保存消息 ID 和文本状态到指定缓存文件
func saveCacheToFile(cache Cache, filename string) {
	data, err := json.Marshal(cache)
	if err == nil {
		os.WriteFile(filename, data, 0644)
	}
}

// initLogger 初始化日志，同时输出到终端和日志文件
func initLogger() {
	logDir := "logs"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Fatalf("创建日志目录失败: %v", err)
	}

	logFile, err := os.OpenFile(
		filepath.Join(logDir, "embyradar.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		log.Fatalf("打开日志文件失败: %v", err)
	}

	// 同时输出到终端和文件
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

// setBotCommands 设置机器人命令菜单，使聊天界面出现快捷菜单。
// 普通用户看到基础命令，管理员额外看到管理类命令。
func setBotCommands(bot *tgbotapi.BotAPI) {
	// 普通用户可见的命令（默认作用域）
	commands := []tgbotapi.BotCommand{
		{Command: "ask", Description: "提问 (用法: /ask 你的问题)"},
		{Command: "request", Description: "求片 (用法: /request 影视名称)"},
		{Command: "img", Description: "AI 画图 (用法: /img 图片描述)"},
	}
	cfg := tgbotapi.NewSetMyCommands(commands...)
	if _, err := bot.Request(cfg); err != nil {
		log.Printf("[Warn] 设置 Bot 命令菜单失败: %v", err)
	} else {
		log.Printf("[Bot] 普通用户命令菜单已注册")
	}

	// 群聊管理员可见的完整命令列表
	adminCommands := []tgbotapi.BotCommand{
		{Command: "ask", Description: "提问 (用法: /ask 你的问题)"},
		{Command: "request", Description: "求片 (用法: /request 影视名称)"},
		{Command: "img", Description: "AI 画图 (用法: /img 图片描述)"},
		{Command: "request_coin_cost", Description: "查看/设置求片费用 (用法: /request_coin_cost [数值])"},
		{Command: "clear_ctx", Description: "清空当前聊天的上下文记忆"},
		{Command: "kb_list", Description: "查看当前知识库条目"},
		{Command: "kb_add", Description: "添加知识库 (用法: /kb_add 词条 内容)"},
		{Command: "kb_del", Description: "删除知识库 (用法: /kb_del 词条)"},
		{Command: "reload_kb", Description: "重新加载知识库 (从文件)"},
	}
	adminScope := tgbotapi.NewSetMyCommandsWithScope(
		tgbotapi.NewBotCommandScopeAllChatAdministrators(),
		adminCommands...,
	)
	if _, err := bot.Request(adminScope); err != nil {
		log.Printf("[Warn] 设置管理员命令菜单失败: %v", err)
	} else {
		log.Printf("[Bot] 管理员命令菜单已注册")
	}
}
