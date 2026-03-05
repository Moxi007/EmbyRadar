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
}

// LoadConfig 从 config.json 加载配置
func LoadConfig(filename string) (*Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			// 如果不存在，创建一个默认模板文件以便用户填写
			defaultConfig := Config{
				TelegramBotToken: "YOUR_TELEGRAM_BOT_TOKEN_HERE",
				TelegramChatID:   -1000000000000, // 替换为真实 Chat ID，通常以 -100 开头
				EmbyURL:          "http://127.0.0.1:8096",
				EmbyAPIKey:       "YOUR_EMBY_API_KEY_HERE",
				UpdateInterval:   60,
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

	return &config, nil
}
