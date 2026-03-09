package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 存储应用的所有配置
type Config struct {
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramChatID   int64  `json:"telegram_chat_id"`
	EmbyURL          string `json:"emby_url"`
	EmbyAPIKey       string `json:"emby_api_key"`
	UpdateInterval   int    `json:"update_interval"` // 以秒为单位
	ServerName       string `json:"server_name"`     // 自定义服务名称，显示在状态面板

	// AI 聊天相关配置
	AIEnabled            bool             `json:"ai_enabled"`             // 是否启用 AI 聊天
	AIBaseURL            string           `json:"ai_base_url"`            // OpenAI 兼容 API 地址
	AIAPIKey             string           `json:"ai_api_key"`             // AI API Key
	AIModel              string           `json:"ai_model"`               // 模型名称
	AISystemPrompt       string           `json:"ai_system_prompt"`       // 预设人设提示词
	AIMaxContext         int              `json:"ai_max_context"`         // 最大上下文轮数
	AIMaxTokens          int              `json:"ai_max_tokens"`          // 最大回复 token 数
	AITemperature        float64          `json:"ai_temperature"`         // 温度参数
	AIKnowledgeDir       string           `json:"ai_knowledge_dir"`       // 知识库目录
	AIEmbyStatsFormat    string           `json:"ai_emby_stats_format"`   // Emby 实时数据注入格式
	EmbyBossAPIUrl       string           `json:"embyboss_api_url"`       // EmbyBoss 本地服务地址 API
	EmbyBossAPIToken     string           `json:"embyboss_api_token"`     // EmbyBoss 的 bot_token（用于接口鉴权）
	EmbyBossCurrencyName string           `json:"embyboss_currency_name"` // 积分/货币名称，API 获取失败时的默认值
	WebhookPort          int              `json:"webhook_port"`           // Webhook 监听端口，供 EmbyBoss 推送事件使用
	WelcomeStickerID     string           `json:"welcome_sticker_id"`     // 新用户加群时发送的欢迎贴纸ID
	AITriggerKeywords    []string         `json:"ai_trigger_keywords"`    // 触发关键词列表
	AIRoles              map[int64]string `json:"ai_roles"`               // TG ID (string) 到 身份标识 (string) 的映射
	BotAdmins            []int64          `json:"bot_admins"`             // 指定拥有机器人命令管控权的 TG ID
}

// LoadConfig 从 config.json 加载配置
func LoadConfig(filename string) (*Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			// 如果不存在，创建一个默认模板文件以便用户填写
			defaultConfig := Config{
				TelegramBotToken: "YOUR_TELEGRAM_BOT_TOKEN_HERE",
				TelegramChatID:   -1000000000000,
				EmbyURL:          "http://127.0.0.1:8096",
				EmbyAPIKey:       "YOUR_EMBY_API_KEY_HERE",
				UpdateInterval:   60,

				AIEnabled:         false,
				AIBaseURL:         "https://api.openai.com/v1",
				AIAPIKey:          "YOUR_AI_API_KEY_HERE",
				AIModel:           "gpt-4o",
				AISystemPrompt:    "你是一个友好的群聊助手，请保持回复简洁有趣。",
				AIMaxContext:      20,
				AIMaxTokens:       1000,
				AITemperature:     0.7,
				AIKnowledgeDir:    "config/knowledge",
				AIEmbyStatsFormat: "\n\n【实时客观数据（仅作参考）】：当前你管理的【小鸡服】共有注册大臣/平民 %d 人，此时此刻服务器内正有 %d 人在流连佳作。",
				EmbyBossAPIUrl:    "http://127.0.0.1:8838",
				WebhookPort:       8889,
				AITriggerKeywords: make([]string, 0),
				AIRoles:           make(map[int64]string),
				BotAdmins:         make([]int64, 0),
			}
			bytes, _ := json.MarshalIndent(defaultConfig, "", "  ")
			os.WriteFile(filename, bytes, 0644)
			return nil, fmt.Errorf("配置文件 %s 不存在，已生成模板，请填写后重新运行", filename)
		}
		return nil, err
	}
	defer file.Close()

	var config Config
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}

	// 简单校验
	if config.TelegramBotToken == "" || config.TelegramBotToken == "YOUR_TELEGRAM_BOT_TOKEN_HERE" {
		return nil, fmt.Errorf("请在 %s 中配置正确的 Telegram Bot Token", filename)
	}
	if config.EmbyAPIKey == "" || config.EmbyAPIKey == "YOUR_EMBY_API_KEY_HERE" {
		return nil, fmt.Errorf("请在 %s 中配置正确的 Emby API Key", filename)
	}

	// AI 相关默认值
	if config.AIMaxContext <= 0 {
		config.AIMaxContext = 20
	}
	if config.AIMaxTokens <= 0 {
		config.AIMaxTokens = 1000
	}
	if config.AITemperature <= 0 {
		config.AITemperature = 0.7
	}
	if config.AIKnowledgeDir == "" {
		config.AIKnowledgeDir = "config/knowledge"
	}
	if config.AIEmbyStatsFormat == "" {
		config.AIEmbyStatsFormat = "\n\n【实时客观数据（仅作参考）】：当前你管理的【小鸡服】共有注册大臣/平民 %d 人，此时此刻服务器内正有 %d 人在流连佳作。"
	}
	if config.EmbyBossAPIUrl == "" {
		config.EmbyBossAPIUrl = "http://127.0.0.1:8838"
	}
	if config.EmbyBossCurrencyName == "" {
		config.EmbyBossCurrencyName = "鸡蛋"
	}
	if config.WebhookPort == 0 {
		config.WebhookPort = 8889
	}

	return &config, nil
}
