package main

import (
	"encoding/json"
	"fmt"
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
	WebhookPort          int              `json:"webhook_port"`
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
	RequestEnabled       bool             `json:"request_enabled"` // 求片功能开关，默认 false
}

// GetGroupConfig 根据 chatID 查找群组配置，O(1) map 查找
// 若 chatID 未匹配到任何群组配置，返回 nil
func (ac *AppConfig) GetGroupConfig(chatID int64) *GroupConfig {
	if ac.groupMap == nil {
		return nil
	}
	return ac.groupMap[chatID]
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
				"groups": []map[string]interface{}{
					{
						"telegram_chat_id":    -1000000000000,
						"emby_url":            "http://127.0.0.1:8096",
						"emby_api_key":        "YOUR_EMBY_API_KEY_HERE",
						"embyboss_api_url":    "http://127.0.0.1:8838",
						"embyboss_api_token":  "",
						"server_name":         "EMBY",
						"update_interval":     60,
						"webhook_port":        0,
						"ai_enabled":          false,
						"ai_trigger_keywords": []string{},
						"ai_roles":            map[string]string{},
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
	portMap := make(map[int]int) // webhook_port → 首次出现的群组索引
	for i, g := range groups {
		// 校验 telegram_chat_id 不为零值
		if g.TelegramChatID == 0 {
			return nil, fmt.Errorf("groups[%d] 缺少 telegram_chat_id 字段", i)
		}

		// 检测 webhook_port 冲突（仅非零端口）
		if g.WebhookPort != 0 {
			if prevIdx, exists := portMap[g.WebhookPort]; exists {
				return nil, fmt.Errorf("webhook_port %d 冲突：groups[%d] 与 groups[%d] 使用了相同的端口", g.WebhookPort, prevIdx, i)
			}
			portMap[g.WebhookPort] = i
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
