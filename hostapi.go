package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultEngineListenAddr = "127.0.0.1:18081"

type HostCommandContext struct {
	ChatID           int64 `json:"chat_id"`
	SenderID         int64 `json:"sender_id,omitempty"`
	MessageID        int   `json:"message_id,omitempty"`
	ReplyToMessageID int   `json:"reply_to_message_id,omitempty"`
	IsPrivate        bool  `json:"is_private"`
	AllowSensitive   bool  `json:"allow_sensitive"`
}

type engineRunRequest struct {
	Command string             `json:"command"`
	Context HostCommandContext `json:"context"`
}

type engineRunResponse struct {
	Output    string `json:"output"`
	MessageID int    `json:"message_id,omitempty"`
}

type HostEngine struct {
	chatHandler *ChatHandler
}

func NewHostEngine(chatHandler *ChatHandler) *HostEngine {
	return &HostEngine{chatHandler: chatHandler}
}

func (he *HostEngine) BaseURL() string {
	return "http://" + defaultEngineListenAddr
}

func (he *HostEngine) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/engine/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/engine/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req engineRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid payload: %v", err), http.StatusBadRequest)
			return
		}
		result, messageID, err := he.Execute(req.Command, req.Context)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(engineRunResponse{
			Output:    result,
			MessageID: messageID,
		})
	})

	go func() {
		server := &http.Server{
			Addr:              defaultEngineListenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		log.Printf("[HostEngine] 本地宿主 API 已监听 %s", defaultEngineListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("启动宿主 API 失败: %v", err)
		}
	}()
}

func (he *HostEngine) Execute(command string, ctx HostCommandContext) (string, int, error) {
	name, rest := splitCommand(command)
	if name == "" {
		return "", 0, fmt.Errorf("空命令")
	}

	switch name {
	case "chat.say":
		text := strings.TrimSpace(rest)
		if text == "" {
			return "", 0, fmt.Errorf("chat.say 需要消息内容")
		}
		messageID, err := he.chatHandler.transport.Say(ctx.ChatID, text)
		if err != nil {
			return "", 0, err
		}
		he.chatHandler.emitCognitionEvent("message.sent", ctx.ChatID, ctx.SenderID, "", messageID, text, map[string]any{
			"kind": "chat.say",
		})
		return "消息已发送", messageID, nil
	case "chat.reply":
		text := strings.TrimSpace(rest)
		if text == "" {
			return "", 0, fmt.Errorf("chat.reply 需要消息内容")
		}
		replyTo := ctx.ReplyToMessageID
		if replyTo == 0 {
			replyTo = ctx.MessageID
		}
		messageID, err := he.chatHandler.transport.Reply(ctx.ChatID, replyTo, text)
		if err != nil {
			return "", 0, err
		}
		he.chatHandler.emitCognitionEvent("message.sent", ctx.ChatID, ctx.SenderID, "", messageID, text, map[string]any{
			"kind": "chat.reply",
		})
		return "消息已回复", messageID, nil
	case "chat.pin":
		msgID, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil || msgID == 0 {
			return "", 0, fmt.Errorf("chat.pin 需要合法的 message_id")
		}
		if err := he.chatHandler.transport.Pin(ctx.ChatID, msgID, false); err != nil {
			return "", 0, err
		}
		return "消息已置顶", 0, nil
	case "web.search":
		query := strings.TrimSpace(rest)
		if query == "" {
			return "", 0, fmt.Errorf("web.search 需要 query")
		}
		return he.executeTool("search_web", map[string]any{"query": query}, ctx)
	case "tmdb.search":
		query, mediaType := parseTMDBCommand(rest)
		if query == "" {
			return "", 0, fmt.Errorf("tmdb.search 需要 query")
		}
		args := map[string]any{"query": query}
		if mediaType != "" {
			args["media_type"] = mediaType
		}
		return he.executeTool("search_tmdb", args, ctx)
	case "emby.search":
		query := strings.TrimSpace(rest)
		if query == "" {
			return "", 0, fmt.Errorf("emby.search 需要 query")
		}
		return he.executeTool("search_emby_library", map[string]any{"query": query}, ctx)
	case "emby.latest":
		return he.executeTool("get_emby_latest_added", map[string]any{}, ctx)
	case "embyboss.user":
		targetID, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("embyboss.user 需要 telegram id")
		}
		return he.executeTool("get_user_info", map[string]any{"target_tg_id": float64(targetID)}, ctx)
	case "skill.read":
		name := strings.TrimSpace(rest)
		if name == "" {
			return "", 0, fmt.Errorf("skill.read 需要 skill_name")
		}
		return he.executeTool("read_skill", map[string]any{"skill_name": name}, ctx)
	case "job.read":
		jobID := strings.TrimSpace(rest)
		if jobID == "" {
			return "", 0, fmt.Errorf("job.read 需要 job_id")
		}
		return he.executeTool("read_job", map[string]any{"job_id": jobID}, ctx)
	case "job.write":
		var payload struct {
			JobID   string `json:"job_id"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &payload); err != nil {
			return "", 0, fmt.Errorf("job.write 需要 JSON 参数: %w", err)
		}
		return he.executeTool("write_job", map[string]any{"job_id": payload.JobID, "content": payload.Content}, ctx)
	case "memory.search":
		query := strings.TrimSpace(rest)
		if query == "" {
			return "", 0, fmt.Errorf("memory.search 需要 query")
		}
		return he.searchMemory(query, ctx)
	default:
		return "", 0, fmt.Errorf("不支持的宿主命令: %s", name)
	}
}

func splitCommand(input string) (string, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}
	for i, r := range input {
		if r == ' ' || r == '\n' || r == '\t' {
			return strings.TrimSpace(input[:i]), strings.TrimSpace(input[i+1:])
		}
	}
	return input, ""
}

func parseTMDBCommand(rest string) (string, string) {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "{") {
		var payload struct {
			Query     string `json:"query"`
			MediaType string `json:"media_type"`
		}
		if err := json.Unmarshal([]byte(rest), &payload); err == nil {
			return strings.TrimSpace(payload.Query), strings.TrimSpace(payload.MediaType)
		}
	}
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) >= 2 && (parts[len(parts)-1] == "movie" || parts[len(parts)-1] == "tv") {
		return strings.TrimSpace(strings.Join(parts[:len(parts)-1], " ")), parts[len(parts)-1]
	}
	return rest, ""
}

func (he *HostEngine) executeTool(toolName string, args map[string]any, ctx HostCommandContext) (string, int, error) {
	toolCtx := he.buildToolContext(ctx)
	if !he.chatHandler.toolRegistry.HasHandler(toolName) {
		return "", 0, fmt.Errorf("未注册工具: %s", toolName)
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return "", 0, fmt.Errorf("序列化工具参数失败: %w", err)
	}
	return he.chatHandler.toolRegistry.Execute(toolName, string(payload), toolCtx), 0, nil
}

func (he *HostEngine) searchMemory(query string, ctx HostCommandContext) (string, int, error) {
	if he.chatHandler.memoryStore == nil {
		return "向量记忆未启用", 0, nil
	}
	topK := he.chatHandler.appConfig.Global.MemoryTopK
	if topK <= 0 {
		topK = 5
	}
	results, err := he.chatHandler.memoryStore.Search(ctx.ChatID, query, topK)
	if err != nil {
		return "", 0, err
	}
	if len(results) == 0 {
		return "未找到相关长期记忆", 0, nil
	}
	var lines []string
	for i, item := range results {
		lines = append(lines, fmt.Sprintf("%d. [%s %s] %s", i+1, item.UserName, item.Timestamp, item.Text))
	}
	return strings.Join(lines, "\n"), 0, nil
}

func (he *HostEngine) buildToolContext(ctx HostCommandContext) *ToolContext {
	group := he.chatHandler.appConfig.GetGroupConfig(ctx.ChatID)
	return &ToolContext{
		ChatID:                ctx.ChatID,
		SenderID:              ctx.SenderID,
		Group:                 group,
		AppConfig:             he.chatHandler.appConfig,
		IsPrivate:             ctx.IsPrivate,
		EmbyClient:            he.chatHandler.embyMap[ctx.ChatID],
		EBClient:              he.chatHandler.ebMap[ctx.ChatID],
		TMDBClient:            he.chatHandler.tmdbMap[ctx.ChatID],
		AIClient:              he.chatHandler.aiClient,
		ChatHandler:           he.chatHandler,
		IsSensitiveAllowed:    ctx.AllowSensitive,
		AllowSensitiveDetails: ctx.AllowSensitive && ctx.IsPrivate,
	}
}

func readBodyText(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body))
}
