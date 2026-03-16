package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
)

// WebhookPayload 接收从 EmbyBoss 推送过来的建号通知
type WebhookPayload struct {
	TgID     int64  `json:"tg_id"`
	TgName   string `json:"tg_name"`
	EmbyName string `json:"emby_name"`
}

// EmbyWebhookPayload 接收从 Emby 推送过来的 Webhook 通知
type EmbyWebhookPayload struct {
	Event       string `json:"Event"`       // 如 library.new
	Server      EmbyWebhookServer `json:"Server"`
	Item        EmbyWebhookItem   `json:"Item"`
}

type EmbyWebhookServer struct {
	Name    string `json:"Name"`
	Version string `json:"Version"`
}

type EmbyWebhookItem struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"` // Movie, Episode, Series
	ProviderIds struct {
		Tmdb string `json:"Tmdb,omitempty"`
		Imdb string `json:"Imdb,omitempty"`
	} `json:"ProviderIds"`
}

// StartGlobalWebhook 启动全局统一的 Webhook HTTP 服务
func StartGlobalWebhook(appConfig *AppConfig, ch *ChatHandler) {
	mux := http.NewServeMux()
	
	mux.HandleFunc("/webhook/new_user", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		chatIDStr := r.URL.Query().Get("chat_id")
		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid chat_id", http.StatusBadRequest)
			return
		}

		group := appConfig.GetGroupConfig(chatID)
		if group == nil {
			http.Error(w, "Group not found", http.StatusNotFound)
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

	mux.HandleFunc("/webhook/emby", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 从 URL 参数中获取 chat_id，以便确认发送到哪个群组。如果支持跨群组入库通知，也可以直接查库，这里采用指定 chat_id 方案
		chatIDStr := r.URL.Query().Get("chat_id")
		var targetChatID int64
		if chatIDStr != "" {
			parsedID, err := strconv.ParseInt(chatIDStr, 10, 64)
			if err == nil {
				targetChatID = parsedID
			}
		}

		var payload EmbyWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		// 目前我们只关心新资源入库事件 library.new
		if payload.Event != "library.new" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Ignored event"))
			return
		}

		tmdbIDStr := payload.Item.ProviderIds.Tmdb
		if tmdbIDStr == "" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("No TMDB ID"))
			return
		}

		log.Printf("[Webhook] 收到 Emby 入库推送: Server=%s, Item=%s, TMDB=%s, Event=%s", payload.Server.Name, payload.Item.Name, tmdbIDStr, payload.Event)
		
		go ch.requestHandler.HandleLibraryNewNotify(ch, targetChatID, tmdbIDStr)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	addr := fmt.Sprintf("0.0.0.0:%d", appConfig.Global.WebhookPort)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	log.Printf("[Webhook] 全局 Webhook 服务开始监听端口 %d 用于接收推送...", appConfig.Global.WebhookPort)
	if err := server.ListenAndServe(); err != nil {
		log.Printf("[Err] 全局 Webhook 服务启动失败 (端口 %d): %v", appConfig.Global.WebhookPort, err)
	}
}
