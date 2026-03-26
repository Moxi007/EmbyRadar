package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// AppConfig 应用顶层配置，包含全局配置和群组配置列表
type AppConfig struct {
	Global GlobalConfig   `json:"global"`
	Groups []*GroupConfig `json:"groups"`
	// 运行时构建的路由 map，按 chat_id 索引群组配置，不序列化
	groupMap map[int64]*GroupConfig
}

// GlobalConfig 全局共享配置，所有群组复用
type GlobalConfig struct {
	TelegramBotToken string  `json:"telegram_bot_token"`
	AIBaseURL        string  `json:"ai_base_url"`
	AIAPIKey         string  `json:"ai_api_key"`
	AIModel          string  `json:"ai_model"`
	AIMaxTokens      int     `json:"ai_max_tokens"`
	AITemperature    float64 `json:"ai_temperature"`
	AIMaxContext     int     `json:"ai_max_context"`
	BotAdmins        []int64 `json:"bot_admins"`
	DBPath           string  `json:"db_path"`      // SQLite 数据库路径，默认 config/embyradar.db
	TMDBAPIKey       string  `json:"tmdb_api_key"` // TMDB API 密钥，为空时禁用 TMDB 相关功能
	WebhookPort      int     `json:"webhook_port"` // 全局 Webhook 端口
	
	// AI 记忆系统配置
	DigestEnabled    bool    `json:"digest_enabled"`    // 是否开启每日对话摘要（方案四）
	DigestHour       int     `json:"digest_hour"`       // 每日执行摘要的小时（0-23），默认 3 (凌晨3点)
	QdrantURL        string  `json:"qdrant_url"`        // Qdrant 数据库 HTTP 地址，为空则禁用向量记忆
	EmbeddingAPIURL  string  `json:"embedding_api_url"` // 独立的 Embedding API 地址 (例如硅基流动)
	EmbeddingAPIKey  string  `json:"embedding_api_key"` // Embedding API 密钥
	EmbeddingModel   string  `json:"embedding_model"`   // Embedding 模型名称
	MemoryTopK       int     `json:"memory_top_k"`      // 检索的记忆条数，默认 5
}

// GroupConfig 群组级独立配置，每个 Telegram 群组一份
type GroupConfig struct {
	TelegramChatID       int64            `json:"telegram_chat_id"`
	EmbyURL              string           `json:"emby_url"`
	EmbyAPIKey           string           `json:"emby_api_key"`
	EmbyBossAPIUrl       string           `json:"embyboss_api_url"`
	EmbyBossAPIToken     string           `json:"embyboss_api_token"`
	EmbyBossCurrencyName string           `json:"embyboss_currency_name"`
	ServerName           string           `json:"server_name"`
	UpdateInterval       int              `json:"update_interval"`
	WelcomeStickerID     string           `json:"welcome_sticker_id"`
	WelcomeEmbyPrompt    string           `json:"welcome_emby_prompt"`
	WelcomeCodePrompt    string           `json:"welcome_code_prompt"`
	AIEnabled            bool             `json:"ai_enabled"`
	AISearchEnabled      bool             `json:"ai_search_enabled"`
	AISystemPrompt       string           `json:"ai_system_prompt"`
	AITriggerKeywords    []string         `json:"ai_trigger_keywords"`
	AIRoles              map[int64]string `json:"ai_roles"`
	AIKnowledgeDir       string           `json:"ai_knowledge_dir"`
	AIEmbyStatsFormat    string           `json:"ai_emby_stats_format"`
	RequestEnabled       bool             `json:"request_enabled"`   // 求片功能开关，默认 false
	RequestAdmins        []int64          `json:"request_admins"`    // 群组级求片管理员列表，为空时回退到全局 bot_admins
	RequestCoinCost      int              `json:"request_coin_cost"` // 每次求片消耗货币数，默认 0（不消耗）
	AIImageEnabled       bool             `json:"ai_image_enabled"`  // 图片生成开关，默认 false
	AIImageModel         string           `json:"ai_image_model"`    // 图片生成模型名称，如 "dall-e-3"
	AIImageSize          string           `json:"ai_image_size"`     // 图片尺寸，默认 "1024x1024"
}

// GetGroupConfig 根据 chatID 查找群组配置，O(1) map 查找
// 若 chatID 未匹配到任何群组配置，返回 nil
func (ac *AppConfig) GetGroupConfig(chatID int64) *GroupConfig {
	if ac.groupMap == nil {
		return nil
	}
	return ac.groupMap[chatID]
}

// SaveConfig 将当前配置序列化为 JSON 并写入指定文件
// 用于运行时通过 Bot 命令修改配置后的持久化
func (ac *AppConfig) SaveConfig(filename string) error {
	// 构建与配置文件一致的顶层结构
	raw := map[string]interface{}{
		"telegram_bot_token": ac.Global.TelegramBotToken,
		"ai_base_url":        ac.Global.AIBaseURL,
		"ai_api_key":         ac.Global.AIAPIKey,
		"ai_model":           ac.Global.AIModel,
		"ai_max_tokens":      ac.Global.AIMaxTokens,
		"ai_temperature":     ac.Global.AITemperature,
		"ai_max_context":     ac.Global.AIMaxContext,
		"bot_admins":         ac.Global.BotAdmins,
		"db_path":            ac.Global.DBPath,
		"tmdb_api_key":       ac.Global.TMDBAPIKey,
		"webhook_port":       ac.Global.WebhookPort,
		"digest_enabled":     ac.Global.DigestEnabled,
		"digest_hour":        ac.Global.DigestHour,
		"qdrant_url":         ac.Global.QdrantURL,
		"embedding_api_url":  ac.Global.EmbeddingAPIURL,
		"embedding_api_key":  ac.Global.EmbeddingAPIKey,
		"embedding_model":    ac.Global.EmbeddingModel,
		"memory_top_k":       ac.Global.MemoryTopK,
		"groups":             ac.Groups,
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %v", err)
	}
	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %v", err)
	}
	return nil
}

// IsAuthorizedGroup 检查 chatID 是否为已配置的授权群组
func (ac *AppConfig) IsAuthorizedGroup(chatID int64) bool {
	return ac.GetGroupConfig(chatID) != nil
}

// LoadConfig 加载并解析新格式的多群组配置文件
// 若文件不存在则生成包含 groups 数组示例的默认模板并返回错误
func LoadConfig(filename string) (*AppConfig, error) {
	// 中间结构体，用于解析顶层全局字段和 groups 原始 JSON
	type rawConfig struct {
		TelegramBotToken string          `json:"telegram_bot_token"`
		AIBaseURL        string          `json:"ai_base_url"`
		AIAPIKey         string          `json:"ai_api_key"`
		AIModel          string          `json:"ai_model"`
		AIMaxTokens      int             `json:"ai_max_tokens"`
		AITemperature    float64         `json:"ai_temperature"`
		AIMaxContext     int             `json:"ai_max_context"`
		BotAdmins        []int64         `json:"bot_admins"`
		DBPath           string          `json:"db_path"`
		TMDBAPIKey       string          `json:"tmdb_api_key"`
		WebhookPort      int             `json:"webhook_port"`
		DigestEnabled    bool            `json:"digest_enabled"`
		DigestHour       int             `json:"digest_hour"`
		QdrantURL        string          `json:"qdrant_url"`
		EmbeddingAPIURL  string          `json:"embedding_api_url"`
		EmbeddingAPIKey  string          `json:"embedding_api_key"`
		EmbeddingModel   string          `json:"embedding_model"`
		MemoryTopK       int             `json:"memory_top_k"`
		Groups           json.RawMessage `json:"groups"`
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			// 生成新格式的默认模板，包含 groups 数组示例
			defaultTemplate := map[string]interface{}{
				"telegram_bot_token": "YOUR_TELEGRAM_BOT_TOKEN_HERE",
				"ai_base_url":        "https://api.openai.com/v1",
				"ai_api_key":         "YOUR_AI_API_KEY_HERE",
				"ai_model":           "gpt-4o",
				"ai_max_tokens":      1000,
				"ai_temperature":     0.7,
				"ai_max_context":     20,
				"bot_admins":         []int64{},
				"webhook_port":       8080,
				"digest_enabled":     true,
				"digest_hour":        3,
				"qdrant_url":         "",
				"embedding_api_url":  "",
				"embedding_api_key":  "",
				"embedding_model":    "",
				"memory_top_k":       5,
				"groups": []map[string]interface{}{
					{
						"telegram_chat_id":    -1000000000000,
						"emby_url":            "http://127.0.0.1:8096",
						"emby_api_key":        "YOUR_EMBY_API_KEY_HERE",
						"embyboss_api_url":    "http://127.0.0.1:8838",
						"embyboss_api_token":  "",
						"server_name":         "EMBY",
						"update_interval":     60,
						"ai_enabled":          false,
						"ai_search_enabled":   false,
						"ai_trigger_keywords": []string{},
						"ai_roles":            map[string]string{},
						"request_coin_cost":   0,
					},
				},
			}
			bytes, _ := json.MarshalIndent(defaultTemplate, "", "  ")
			os.WriteFile(filename, bytes, 0644)
			return nil, fmt.Errorf("配置文件 %s 不存在，已生成新格式模板（含 groups 数组），请填写后重新运行", filename)
		}
		return nil, err
	}

	// 解析为中间结构体
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}

	// 校验 telegram_bot_token 不为空且不为占位符
	if raw.TelegramBotToken == "" || raw.TelegramBotToken == "YOUR_TELEGRAM_BOT_TOKEN_HERE" {
		return nil, fmt.Errorf("请在 %s 中配置正确的 Telegram Bot Token", filename)
	}

	// 检查 groups 字段是否存在
	if len(raw.Groups) == 0 {
		return nil, fmt.Errorf("配置文件缺少 groups 字段，请使用新的 groups 数组格式进行配置")
	}

	// 解析 groups 数组
	var groups []*GroupConfig
	if err := json.Unmarshal(raw.Groups, &groups); err != nil {
		return nil, fmt.Errorf("解析 groups 数组失败: %v", err)
	}

	// groups 数组为空时返回错误
	if len(groups) == 0 {
		return nil, fmt.Errorf("groups 数组为空，至少需要配置一个群组")
	}

	// 逐个校验 GroupConfig 并填充默认值
	for i, g := range groups {
		// 校验 telegram_chat_id 不为零值
		if g.TelegramChatID == 0 {
			return nil, fmt.Errorf("groups[%d] 缺少 telegram_chat_id 字段", i)
		}

		// 填充群组级默认值
		if g.AIKnowledgeDir == "" {
			g.AIKnowledgeDir = "config/knowledge"
		}
		if g.AISystemPrompt == "" {
			g.AISystemPrompt = "你是一个群聊助手，请保持回复简洁友好。"
		}
		if g.UpdateInterval <= 0 {
			g.UpdateInterval = 60
		}
		if g.EmbyBossAPIUrl == "" {
			g.EmbyBossAPIUrl = "http://127.0.0.1:8838"
		}
		if g.EmbyBossCurrencyName == "" {
			g.EmbyBossCurrencyName = "鸡蛋"
		}
		// 图片生成尺寸默认值
		if g.AIImageSize == "" {
			g.AIImageSize = "1024x1024"
		}
		// 启用图片生成但未配置模型时输出警告
		if g.AIImageEnabled && g.AIImageModel == "" {
			log.Printf("[Warn] 群组 %d 启用了图片生成但未配置模型名称", g.TelegramChatID)
		}
	}

	// 填充全局默认值
	if raw.AIMaxTokens <= 0 {
		raw.AIMaxTokens = 1000
	}
	if raw.AITemperature <= 0 {
		raw.AITemperature = 0.7
	}
	if raw.AIMaxContext <= 0 {
		raw.AIMaxContext = 20
	}
	if raw.WebhookPort <= 0 {
		raw.WebhookPort = 8080
	}
	if raw.MemoryTopK <= 0 {
		raw.MemoryTopK = 5
	}
	// 如果配置了 DigestHour 但超出了 0-23 的范围，则重置为凌晨 3 点
	if raw.DigestHour < 0 || raw.DigestHour > 23 {
		raw.DigestHour = 3
	}

	// 构建 AppConfig
	appConfig := &AppConfig{
		Global: GlobalConfig{
			TelegramBotToken: raw.TelegramBotToken,
			AIBaseURL:        raw.AIBaseURL,
			AIAPIKey:         raw.AIAPIKey,
			AIModel:          raw.AIModel,
			AIMaxTokens:      raw.AIMaxTokens,
			AITemperature:    raw.AITemperature,
			AIMaxContext:     raw.AIMaxContext,
			BotAdmins:        raw.BotAdmins,
			DBPath:           raw.DBPath,
			TMDBAPIKey:       raw.TMDBAPIKey,
			WebhookPort:      raw.WebhookPort,
			DigestEnabled:    raw.DigestEnabled,
			DigestHour:       raw.DigestHour,
			QdrantURL:        raw.QdrantURL,
			EmbeddingAPIURL:  raw.EmbeddingAPIURL,
			EmbeddingAPIKey:  raw.EmbeddingAPIKey,
			EmbeddingModel:   raw.EmbeddingModel,
			MemoryTopK:       raw.MemoryTopK,
		},
		Groups: groups,
	}

	// 构建 groupMap 用于 O(1) 路由查找
	appConfig.groupMap = make(map[int64]*GroupConfig, len(groups))
	for _, g := range groups {
		appConfig.groupMap[g.TelegramChatID] = g
	}

	return appConfig, nil
}
