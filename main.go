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

const messageCacheFile = "config/message_id.json"

// Cache 缓存上一次发送的消息 ID 和内容，避免重复编辑消耗 API 排队
type Cache struct {
	MessageID int    `json:"message_id"`
	LastText  string `json:"last_text"`
}

func main() {
	// 0. 初始化日志文件
	initLogger()

	// 1. 加载配置
	config, err := LoadConfig("config/config.json")
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	// 2. 初始化 Emby 客户端
	emby := NewEmbyClient(config.EmbyURL, config.EmbyAPIKey)

	// 如果未配置 server_name，则自动从 Emby 获取服务器名称
	if config.ServerName == "" {
		if name, err := emby.GetServerName(); err == nil {
			config.ServerName = name
			log.Printf("自动获取服务名称: %s", name)
		} else {
			config.ServerName = "EMBY"
			log.Printf("[Warn] 获取服务名称失败，使用默认名称: %v", err)
		}
	}

	// 3. 初始化 Telegram Bot
	bot, err := tgbotapi.NewBotAPI(config.TelegramBotToken)
	if err != nil {
		log.Fatalf("初始化 Telegram Bot 失败: %v", err)
	}
	bot.Debug = false
	log.Printf("授权成功，Bot: %s", bot.Self.UserName)

	// 4. 初始化 AI 聊天模块（如果启用）
	if config.AIEnabled {
		aiClient := NewAIClient(config)
		kb := NewKnowledgeBase(config.AIKnowledgeDir)
		if err := kb.Load(); err != nil {
			log.Printf("[Warn] 加载知识库失败: %v", err)
		}
		ctxManager := NewContextManager(config.AIMaxContext)
		ebClient := NewEmbyBossClient(config.EmbyBossAPIUrl, config.EmbyBossAPIToken)
		chatHandler := NewChatHandler(bot, aiClient, ctxManager, config, kb, emby, ebClient)

		// 在独立 goroutine 中启动消息监听
		go chatHandler.StartListening()

		// 在独立 goroutine 中启动 Webhook 监听
		go chatHandler.StartWebhookServer()

		log.Printf("[AI] AI 聊天模块已启动 (模型: %s)", config.AIModel)

		// 注册快捷命令菜单
		setBotCommands(bot)
	} else {
		log.Printf("[AI] AI 聊天模块未启用")
	}

	// 5. 读取上一次置顶消息的缓存 ID
	cache := loadCache()

	// 6. 设置定时器
	ticker := time.NewTicker(time.Duration(config.UpdateInterval) * time.Second)
	defer ticker.Stop()

	// 监听退出信号以便优雅退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 立即执行一次
	updateStatus(bot, emby, config, &cache)

	for {
		select {
		case <-ticker.C:
			updateStatus(bot, emby, config, &cache)
		case sig := <-sigCh:
			log.Printf("收到退出信号 %v，程序退出", sig)
			return
		}
	}
}

// updateStatus 获取数据并发送或编辑消息
func updateStatus(bot *tgbotapi.BotAPI, emby *EmbyClient, config *Config, cache *Cache) {
	// 获取数据
	activeSessions, err := emby.GetActiveSessions()
	if err != nil {
		log.Printf("[Err] 获取监控数据失败(Sessions): %v", err)
		return
	}
	totalUsers, err := emby.GetTotalUsers()
	if err != nil {
		log.Printf("[Err] 获取监控数据失败(Users): %v", err)
		return
	}

	// 格式化文本
	text := fmt.Sprintf(
		"📊 *%s 实时状态*\n\n"+
			"🎥 正在观看：*%d*\n"+
			"👤 用户总数：*%d*\n"+
			"📅 更新时间：*%s*",
		config.ServerName,
		activeSessions,
		totalUsers,
		time.Now().Format("15:04"),
	)

	// 如果内容没有改变，不要发送编辑请求以避免 rate limit
	if cache.LastText == text {
		return
	}

	if cache.MessageID == 0 {
		// 发送新消息并置顶
		msg := tgbotapi.NewMessage(config.TelegramChatID, text)
		msg.ParseMode = "Markdown"
		sentMsg, err := bot.Send(msg)
		if err != nil {
			log.Printf("[Err] 发送初始消息失败: %v", err)
			return
		}
		cache.MessageID = sentMsg.MessageID
		cache.LastText = text
		saveCache(*cache)

		// 置顶消息
		pinConfig := tgbotapi.PinChatMessageConfig{
			ChatID:              config.TelegramChatID,
			MessageID:           sentMsg.MessageID,
			DisableNotification: true, // 静默置顶，不打扰群员
		}
		if _, err := bot.Request(pinConfig); err != nil {
			log.Printf("[Warn] 置顶消息失败 (请确保机器人有管理员权限): %v", err)
		} else {
			log.Printf("成功发送并置顶状态消息 ID: %d", sentMsg.MessageID)
		}

	} else {
		// 编辑已存在的置顶消息
		editMsg := tgbotapi.NewEditMessageText(config.TelegramChatID, cache.MessageID, text)
		editMsg.ParseMode = "Markdown"

		_, err := bot.Send(editMsg)
		if err != nil {
			log.Printf("[Err] 编辑消息状态失败 (ID: %d): %v", cache.MessageID, err)
			// 如果因为消息被删除等原因导致找不到该消息，重置 ID
			cache.MessageID = 0
			saveCache(*cache)
		} else {
			// 修改成功
			cache.LastText = text
			saveCache(*cache)
		}
	}
}

// loadCache 从本地文件加载缓存的 Message ID
func loadCache() Cache {
	var cache Cache
	data, err := os.ReadFile(messageCacheFile)
	if err == nil {
		json.Unmarshal(data, &cache)
	}
	return cache
}

// saveCache 保存 Message ID 和文本状态到本地文件
func saveCache(cache Cache) {
	data, err := json.Marshal(cache)
	if err == nil {
		os.WriteFile(messageCacheFile, data, 0644)
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

// setBotCommands 设置机器人命令菜单，使私聊界面出现快捷菜单
func setBotCommands(bot *tgbotapi.BotAPI) {
	commands := []tgbotapi.BotCommand{
		{Command: "ask", Description: "提问 (用法: /ask 你的问题)"},
		{Command: "clear_ctx", Description: "清空当前聊天的上下文记忆"},
		{Command: "kb_list", Description: "[管理] 查看当前知识库条目"},
		{Command: "kb_add", Description: "[管理] 添加知识库 (用法: /kb_add 词条 内容)"},
		{Command: "kb_del", Description: "[管理] 删除知识库 (用法: /kb_del 词条)"},
		{Command: "reload_kb", Description: "[管理] 重新加载知识库 (从文件)"},
	}
	cfg := tgbotapi.NewSetMyCommands(commands...)
	if _, err := bot.Request(cfg); err != nil {
		log.Printf("[Warn] 设置 Bot 命令菜单失败: %v", err)
	} else {
		log.Printf("[Bot] 命令菜单已成功注册")
	}
}
