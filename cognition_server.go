package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type cognitionPersonaState struct {
	Diligence   float64
	Curiosity   float64
	Sociability float64
	Caution     float64
}

type cognitionRelationship struct {
	Warmth            float64
	Reciprocity       float64
	Responsiveness    float64
	Familiarity       float64
	LastInteractionMS int64
	LastProactiveMS   int64
}

type CognitionServer struct {
	baseURL                  string
	listenAddr               string
	dbPath                   string
	proactiveCooldownMinutes int
	quietHours               []int
	relationshipDecayDays    int
	db                       *sql.DB
	server                   *http.Server
	httpClient               *http.Client
}

type cognitionActionRow struct {
	ID           string
	ChatID       int64
	UserID       sql.NullInt64
	Kind         string
	Command      string
	Status       string
	DueMS        int64
	LastError    string
	MetadataJSON string
}

const cognitionSchemaSQL = `
CREATE TABLE IF NOT EXISTS event_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_type TEXT NOT NULL,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  user_name TEXT,
  message_id INTEGER,
  text TEXT,
  timestamp_ms INTEGER NOT NULL,
  metadata_json TEXT
);
CREATE TABLE IF NOT EXISTS diary_entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  content TEXT NOT NULL,
  summary TEXT,
  created_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS beliefs (
  chat_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  user_tier TEXT DEFAULT 'regular',
  familiarity REAL DEFAULT 0.1,
  response_preference TEXT DEFAULT 'concise',
  interest_tags TEXT DEFAULT '',
  emby_preference TEXT DEFAULT '',
  trust_level REAL DEFAULT 0.3,
  last_emotional_signal TEXT DEFAULT 'neutral',
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY (chat_id, user_id)
);
CREATE TABLE IF NOT EXISTS relationships (
  chat_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  warmth REAL DEFAULT 0.2,
  reciprocity REAL DEFAULT 0.2,
  responsiveness REAL DEFAULT 0.2,
  familiarity REAL DEFAULT 0.2,
  last_interaction_ms INTEGER NOT NULL,
  last_proactive_ms INTEGER DEFAULT 0,
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY (chat_id, user_id)
);
CREATE TABLE IF NOT EXISTS narrative_threads (
  id TEXT PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  next_due_ms INTEGER DEFAULT 0,
  last_event_ms INTEGER NOT NULL,
  metadata_json TEXT DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS episodes (
  id TEXT PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  origin TEXT NOT NULL,
  status TEXT NOT NULL,
  voice TEXT NOT NULL,
  pressures_json TEXT NOT NULL,
  tool_call_count INTEGER DEFAULT 0,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS scheduled_actions (
  id TEXT PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  kind TEXT NOT NULL,
  command TEXT NOT NULL,
  status TEXT NOT NULL,
  due_ms INTEGER NOT NULL,
  last_error TEXT DEFAULT '',
  metadata_json TEXT DEFAULT '{}',
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS persona_state (
  chat_id INTEGER PRIMARY KEY,
  diligence REAL DEFAULT 0.25,
  curiosity REAL DEFAULT 0.25,
  sociability REAL DEFAULT 0.25,
  caution REAL DEFAULT 0.25,
  updated_at_ms INTEGER NOT NULL
);
`

func NewCognitionServer(global *GlobalConfig) (*CognitionServer, error) {
	baseURL := strings.TrimSpace(global.CognitionBaseURL)
	if baseURL == "" {
		baseURL = "http://127.0.0.1:3400"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("解析 cognition_base_url 失败: %w", err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("无效的 cognition_base_url: %s", baseURL)
	}

	listenAddr := parsed.Host
	if host, port, err := net.SplitHostPort(parsed.Host); err == nil {
		switch host {
		case "", "127.0.0.1", "localhost":
			listenAddr = net.JoinHostPort("127.0.0.1", port)
		default:
			listenAddr = ":" + port
		}
	}

	dbPath := strings.TrimSpace(global.CognitionDBPath)
	if dbPath == "" {
		dbPath = "config/cognition.db"
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建 cognition 数据目录失败: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 cognition sqlite 失败: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 cognition WAL 失败: %w", err)
	}
	if _, err := db.Exec(cognitionSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 cognition schema 失败: %w", err)
	}

	server := &CognitionServer{
		baseURL:                  strings.TrimRight(baseURL, "/"),
		listenAddr:               listenAddr,
		dbPath:                   dbPath,
		proactiveCooldownMinutes: global.ProactiveCooldownMinutes,
		quietHours:               append([]int(nil), global.QuietHours...),
		relationshipDecayDays:    global.RelationshipDecayDays,
		db:                       db,
		httpClient:               &http.Client{Timeout: 30 * time.Second}, // engine/run 是本地调用，30s 足矣
	}
	if server.proactiveCooldownMinutes <= 0 {
		server.proactiveCooldownMinutes = 720
	}
	if server.relationshipDecayDays <= 0 {
		server.relationshipDecayDays = 14
	}
	if len(server.quietHours) == 0 {
		server.quietHours = []int{0, 1, 2, 3, 4, 5, 6}
	}
	return server, nil
}

func (cs *CognitionServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", cs.handleHealth)
	mux.HandleFunc("/cognition/events", cs.handleEvents)
	mux.HandleFunc("/cognition/respond", cs.handleRespondHTTP)
	mux.HandleFunc("/cognition/request-intent", cs.handleRequestIntent)
	mux.HandleFunc("/cognition/format-knowledge", cs.handleTextTransform)
	mux.HandleFunc("/cognition/merge-knowledge", cs.handleTextTransform)
	mux.HandleFunc("/cognition/summarize-digest", cs.handleTextTransform)
	mux.HandleFunc("/cognition/actions/claim", cs.handleClaimAction)
	mux.HandleFunc("/cognition/actions/", cs.handleActionResult)

	ln, err := net.Listen("tcp", cs.listenAddr)
	if err != nil {
		return fmt.Errorf("监听 cognition 地址失败(%s): %w", cs.listenAddr, err)
	}
	cs.server = &http.Server{
		Addr:              cs.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("[Cognition] Go cognition 已监听 %s (db: %s)", cs.listenAddr, cs.dbPath)
		if err := cs.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Cognition Serve 失败: %v", err)
		}
	}()
	return nil
}

func (cs *CognitionServer) Close() error {
	if cs.server != nil {
		_ = cs.server.Close()
	}
	if cs.db != nil {
		return cs.db.Close()
	}
	return nil
}

func (cs *CognitionServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	cs.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "db_path": cs.dbPath})
}

func (cs *CognitionServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var event CognitionEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := cs.persistEvent(&event); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cs.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (cs *CognitionServer) handleRespondHTTP(w http.ResponseWriter, r *http.Request) {
	// HTTP 适配层：注入 300s 超时，确保服务端比客户端先完成
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RespondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := cs.Respond(ctx, &req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cs.writeJSON(w, http.StatusOK, resp)
}

func (cs *CognitionServer) handleRequestIntent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RequestIntentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := cs.completeText(r.Context(), req.LLM,
		"你是一个影视信息提取助手。请从用户文本中提取 name、type(movie/tv)、year、is_remaster、season。只返回 JSON。",
		req.Text,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cs.writeJSON(w, http.StatusOK, cs.coerceIntent(raw))
}

func (cs *CognitionServer) handleTextTransform(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req TextTransformRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text, err := cs.completeText(r.Context(), req.LLM, req.SystemHint, req.UserText)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cs.writeJSON(w, http.StatusOK, &TextTransformResponse{Text: text})
}

func (cs *CognitionServer) handleClaimAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action, err := cs.claimAction()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if action == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cs.writeJSON(w, http.StatusOK, action)
}

func (cs *CognitionServer) handleActionResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/result") {
		http.NotFound(w, r)
		return
	}
	actionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/cognition/actions/"), "/result")
	if actionID == "" {
		http.Error(w, "missing action id", http.StatusBadRequest)
		return
	}
	var req ActionResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := cs.completeAction(actionID, &req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cs.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Respond 实现 CognitionEngine 接口。
// 处理完整对话请求：压力计算 → 声部选择 → 工具循环 → 回复生成。
func (cs *CognitionServer) Respond(ctx context.Context, req *RespondRequest) (*RespondResponse, error) {
	pressures, err := cs.computePressures(req)
	if err != nil {
		return nil, err
	}
	voice, persona, err := cs.selectVoice(req.ChatID, pressures)
	if err != nil {
		return nil, err
	}
	dynamicTail, err := cs.buildDynamicTail(req)
	if err != nil {
		return nil, err
	}
	systemPrompt := cs.buildStaticPrompt(req, voice, pressures)
	userPrompt := cs.buildUserPrompt(req, dynamicTail)

	episodeID := fmt.Sprintf("episode-%d", nowMS())
	pressureBytes, _ := json.Marshal(pressures)
	if _, err := cs.db.Exec(
		`INSERT INTO episodes (id, chat_id, user_id, origin, status, voice, pressures_json, tool_call_count, created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		episodeID, req.ChatID, nullableInt64(req.UserID), "respond", "running", voice, string(pressureBytes), 0, nowMS(), nowMS(),
	); err != nil {
		return nil, err
	}

	replyText, transcript, err := cs.runToolLoop(ctx, req, systemPrompt, userPrompt)
	if err != nil {
		_, _ = cs.db.Exec(`UPDATE episodes SET status = ?, updated_at_ms = ? WHERE id = ?`, "failed", nowMS(), episodeID)
		return nil, err
	}
	if strings.TrimSpace(replyText) == "" {
		replyText = "（思考了很久，不知道该说什么）"
	}
	replyText = cs.compactReplyIfNeeded(req, replyText)
	_, _ = cs.db.Exec(`UPDATE episodes SET status = ?, tool_call_count = ?, updated_at_ms = ? WHERE id = ?`, "completed", len(transcript), nowMS(), episodeID)
	_, _ = cs.db.Exec(
		`INSERT INTO diary_entries (chat_id, user_id, content, summary, created_at_ms) VALUES (?, ?, ?, ?, ?)`,
		req.ChatID,
		nullableInt64(req.UserID),
		fmt.Sprintf("用户: %s\nAI: %s", cs.normalizeText(req.Text), cs.normalizeText(replyText)),
		truncateString(cs.normalizeText(replyText), 240),
		nowMS(),
	)

	return &RespondResponse{
		EpisodeID:      episodeID,
		ReplyText:      replyText,
		ToolTranscript: transcript,
		MemoryHits:     []string{},
		PersonaSnapshot: map[string]any{
			"voice":     voice,
			"pressures": pressures,
			"persona":   persona,
		},
	}, nil
}

func (cs *CognitionServer) runToolLoop(ctx context.Context, req *RespondRequest, systemPrompt, userPrompt string) (string, []ToolTrace, error) {
	aiClient := newAIClientFromLLM(req.LLM)
	messages := []ChatMessage{
		{Role: "system", Content: MessageContent{Text: systemPrompt}},
		{Role: "user", Content: MessageContent{Text: userPrompt}},
	}
	transcript := make([]ToolTrace, 0, 8)
	messageDeliveries := 0
	stickerDeliveries := 0
	runTool := Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "run",
			Description: "执行命令空间中的受控命令。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "要执行的命令字符串。",
					},
				},
				"required": []string{"command"},
			},
		},
	}

	for i := 0; i < 8; i++ {
		// 每轮开始前检查 context 是否已到期（优雅降级）
		if err := ctx.Err(); err != nil {
			log.Printf("[Cognition] runToolLoop 总预算耗尽 (轮次 %d/%d)，用已有结果返回", i+1, 8)
			for j := len(messages) - 1; j >= 0; j-- {
				if messages[j].Role == "assistant" && strings.TrimSpace(messages[j].Content.Text) != "" {
					return messages[j].Content.Text, transcript, nil
				}
			}
			return "", transcript, fmt.Errorf("cognition 处理超时")
		}

		resp, err := aiClient.ChatCompletionWithContext(ctx, messages, []Tool{runTool})
		if err != nil {
			return "", transcript, err
		}
		messages = append(messages, *resp)
		if len(resp.ToolCalls) == 0 {
			return cs.extractAssistantText(resp), transcript, nil
		}
		for _, call := range resp.ToolCalls {
			var args struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
			command := strings.TrimSpace(args.Command)
			if command == "" {
				messages = append(messages, ChatMessage{
					Role:       "tool",
					Name:       "run",
					ToolCallID: call.ID,
					Content:    MessageContent{Text: "missing command"},
				})
				continue
			}
			command, budgetErr := cs.applyDeliveryBudgets(req, command, messageDeliveries, stickerDeliveries)
			if budgetErr != nil {
				output := budgetErr.Error()
				transcript = append(transcript, ToolTrace{Command: command, Output: output})
				messages = append(messages, ChatMessage{
					Role:       "tool",
					Name:       "run",
					ToolCallID: call.ID,
					Content:    MessageContent{Text: output},
				})
				continue
			}
			output, err := cs.executeRunCommand(ctx, command, req)
			if err != nil {
				output = fmt.Sprintf("tool error: %v", err)
			}
			if strings.HasPrefix(command, "chat.say") || strings.HasPrefix(command, "chat.reply") {
				messageDeliveries++
			}
			if strings.HasPrefix(command, "chat.sticker") {
				stickerDeliveries++
			}
			transcript = append(transcript, ToolTrace{Command: command, Output: output})
			messages = append(messages, ChatMessage{
				Role:       "tool",
				Name:       "run",
				ToolCallID: call.ID,
				Content:    MessageContent{Text: output},
			})
		}
	}

	return "（达到工具预算上限，先收一下）", transcript, nil
}

func (cs *CognitionServer) executeRunCommand(ctx context.Context, command string, req *RespondRequest) (string, error) {
	name, args := splitCommand(command)
	if name == "diary.write" {
		text := cs.normalizeText(args)
		if text == "" {
			text = cs.normalizeText(req.Text)
		}
		_, err := cs.db.Exec(
			`INSERT INTO diary_entries (chat_id, user_id, content, summary, created_at_ms) VALUES (?, ?, ?, ?, ?)`,
			req.ChatID,
			nullableInt64(req.UserID),
			truncateString(text, 4000),
			truncateString(text, 240),
			nowMS(),
		)
		if err != nil {
			return "", err
		}
		return "diary entry written", nil
	}

	body, err := json.Marshal(map[string]any{
		"command": command,
		"context": map[string]any{
			"chat_id":             req.ChatID,
			"sender_id":           req.UserID,
			"message_id":          req.MessageID,
			"reply_to_message_id": req.ReplyToMessageID,
			"is_private":          req.IsPrivate,
			"allow_sensitive":     req.AllowSensitive,
		},
	})
	if err != nil {
		return "", err
	}
	engineURL := strings.TrimRight(req.EngineBaseURL, "/") + "/engine/run"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, engineURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := cs.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return string(respBody), nil
	}
	return parsed.Output, nil
}

func (cs *CognitionServer) completeText(ctx context.Context, llm LLMConfig, systemPrompt, userText string) (string, error) {
	aiClient := newAIClientFromLLM(llm)
	msg, err := aiClient.ChatCompletionWithContext(ctx, []ChatMessage{
		{Role: "system", Content: MessageContent{Text: systemPrompt}},
		{Role: "user", Content: MessageContent{Text: userText}},
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(cs.extractAssistantText(msg)), nil
}

// SendEvent 实现 CognitionEngine 接口。记录认知事件到事件日志。
func (cs *CognitionServer) SendEvent(ctx context.Context, event *CognitionEvent) error {
	return cs.persistEvent(event)
}

// TransformText 实现 CognitionEngine 接口。通用文本转换（知识格式化、摘要等）。
func (cs *CognitionServer) TransformText(ctx context.Context, path string, req *TextTransformRequest) (string, error) {
	text, err := cs.completeText(ctx, req.LLM, req.SystemHint, req.UserText)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

// ParseRequestIntent 实现 CognitionEngine 接口。从用户文本中提取影视求片意图。
func (cs *CognitionServer) ParseRequestIntent(ctx context.Context, llm LLMConfig, text string) (*RequestIntentResponse, error) {
	raw, err := cs.completeText(ctx, llm,
		"你是一个影视信息提取助手。请从用户文本中提取 name、type(movie/tv)、year、is_remaster、season。只返回 JSON。",
		text,
	)
	if err != nil {
		return nil, err
	}
	return cs.coerceIntent(raw), nil
}

// ClaimAction 实现 CognitionEngine 接口。领取一个待执行的计划任务。
func (cs *CognitionServer) ClaimAction(ctx context.Context) (*PlannedAction, error) {
	return cs.claimAction()
}

// SubmitActionResult 实现 CognitionEngine 接口。提交任务执行结果。
func (cs *CognitionServer) SubmitActionResult(ctx context.Context, actionID string, result *ActionResultRequest) error {
	return cs.completeAction(actionID, result)
}

func (cs *CognitionServer) persistEvent(event *CognitionEvent) error {
	ts := nowMS()
	if parsed, err := time.Parse(time.RFC3339, event.Timestamp); err == nil {
		ts = parsed.UnixMilli()
	}
	metadataJSON := "{}"
	if event.Metadata != nil {
		if b, err := json.Marshal(event.Metadata); err == nil {
			metadataJSON = string(b)
		}
	}
	if _, err := cs.db.Exec(
		`INSERT INTO event_log (event_type, chat_id, user_id, user_name, message_id, text, timestamp_ms, metadata_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Type, event.ChatID, nullableInt64(event.UserID), event.UserName, nullableInt(event.MessageID), event.Text, ts, metadataJSON,
	); err != nil {
		return err
	}

	if event.UserID != 0 && strings.TrimSpace(event.Text) != "" {
		if err := cs.upsertBeliefAndRelationship(event.ChatID, event.UserID, event.UserName, event.Text, metadataBool(event.Metadata, "is_private")); err != nil {
			return err
		}
	}
	if strings.HasPrefix(event.Type, "request.") {
		return cs.upsertRequestThread(event, ts, metadataJSON)
	}
	return nil
}

func (cs *CognitionServer) upsertBeliefAndRelationship(chatID, userID int64, userName, text string, isPrivate bool) error {
	ts := nowMS()
	belief := struct {
		UserTier            string
		Familiarity         float64
		ResponsePreference  string
		InterestTags        string
		EmbyPreference      string
		TrustLevel          float64
		LastEmotionalSignal string
	}{UserTier: "regular", Familiarity: 0.1, ResponsePreference: "concise", TrustLevel: 0.3, LastEmotionalSignal: "neutral"}
	row := cs.db.QueryRow(`SELECT user_tier, familiarity, response_preference, interest_tags, emby_preference, trust_level, last_emotional_signal FROM beliefs WHERE chat_id = ? AND user_id = ?`, chatID, userID)
	_ = row.Scan(&belief.UserTier, &belief.Familiarity, &belief.ResponsePreference, &belief.InterestTags, &belief.EmbyPreference, &belief.TrustLevel, &belief.LastEmotionalSignal)

	belief.Familiarity = clampFloat(belief.Familiarity+0.04, 0.1, 1)
	if isPrivate {
		belief.ResponsePreference = "warm"
		belief.TrustLevel = clampFloat(belief.TrustLevel+0.03, 0.2, 1)
	} else {
		belief.ResponsePreference = "concise"
		belief.TrustLevel = clampFloat(belief.TrustLevel+0.01, 0.2, 1)
	}
	if tags := cs.inferInterestTags(text); tags != "" {
		belief.InterestTags = tags
	}
	if pref := cs.inferEmbyPreference(text); pref != "" {
		belief.EmbyPreference = pref
	}
	belief.LastEmotionalSignal = cs.detectEmotion(text)

	if _, err := cs.db.Exec(
		`INSERT INTO beliefs (chat_id, user_id, user_tier, familiarity, response_preference, interest_tags, emby_preference, trust_level, last_emotional_signal, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(chat_id, user_id) DO UPDATE SET
		   user_tier=excluded.user_tier,
		   familiarity=excluded.familiarity,
		   response_preference=excluded.response_preference,
		   interest_tags=excluded.interest_tags,
		   emby_preference=excluded.emby_preference,
		   trust_level=excluded.trust_level,
		   last_emotional_signal=excluded.last_emotional_signal,
		   updated_at_ms=excluded.updated_at_ms`,
		chatID, userID, belief.UserTier, belief.Familiarity, belief.ResponsePreference, belief.InterestTags, belief.EmbyPreference, belief.TrustLevel, belief.LastEmotionalSignal, ts,
	); err != nil {
		return err
	}

	rel := cognitionRelationship{
		Warmth:            0.2,
		Reciprocity:       0.2,
		Responsiveness:    0.2,
		Familiarity:       0.2,
		LastInteractionMS: ts,
		LastProactiveMS:   0,
	}
	row = cs.db.QueryRow(`SELECT warmth, reciprocity, responsiveness, familiarity, last_interaction_ms, last_proactive_ms FROM relationships WHERE chat_id = ? AND user_id = ?`, chatID, userID)
	_ = row.Scan(&rel.Warmth, &rel.Reciprocity, &rel.Responsiveness, &rel.Familiarity, &rel.LastInteractionMS, &rel.LastProactiveMS)
	rel.Warmth = clampFloat(rel.Warmth+0.02, 0.1, 1)
	rel.Reciprocity = clampFloat(rel.Reciprocity+0.01, 0.1, 1)
	rel.Responsiveness = clampFloat(rel.Responsiveness+0.02, 0.1, 1)
	rel.Familiarity = clampFloat(rel.Familiarity+0.03, 0.1, 1)
	rel.LastInteractionMS = ts

	if _, err := cs.db.Exec(
		`INSERT INTO relationships (chat_id, user_id, warmth, reciprocity, responsiveness, familiarity, last_interaction_ms, last_proactive_ms, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(chat_id, user_id) DO UPDATE SET
		   warmth=excluded.warmth,
		   reciprocity=excluded.reciprocity,
		   responsiveness=excluded.responsiveness,
		   familiarity=excluded.familiarity,
		   last_interaction_ms=excluded.last_interaction_ms,
		   last_proactive_ms=excluded.last_proactive_ms,
		   updated_at_ms=excluded.updated_at_ms`,
		chatID, userID, rel.Warmth, rel.Reciprocity, rel.Responsiveness, rel.Familiarity, rel.LastInteractionMS, rel.LastProactiveMS, ts,
	); err != nil {
		return err
	}

	_, err := cs.db.Exec(
		`INSERT INTO diary_entries (chat_id, user_id, content, summary, created_at_ms) VALUES (?, ?, ?, ?, ?)`,
		chatID, userID, fmt.Sprintf("用户 %s: %s", fallbackString(userName, fmt.Sprint(userID)), cs.normalizeText(text)), truncateString(cs.normalizeText(text), 240), ts,
	)
	return err
}

func (cs *CognitionServer) upsertRequestThread(event *CognitionEvent, ts int64, metadataJSON string) error {
	status := metadataString(event.Metadata, "status", "open")
	title := metadataString(event.Metadata, "title", "求片流程")
	tmdbID := metadataString(event.Metadata, "tmdb_id", "")
	threadID := fmt.Sprintf("request:%d:%s", event.ChatID, tmdbID)
	if tmdbID == "" {
		threadID = fmt.Sprintf("request:%d:%d", event.ChatID, maxInt(event.MessageID, int(ts)))
	}
	nextDue := ts + 6*60*60*1000
	threadStatus := "open"
	if status == "fulfilled" || status == "rejected" {
		threadStatus = "resolved"
		nextDue = 0
	}
	_, err := cs.db.Exec(
		`INSERT INTO narrative_threads (id, chat_id, user_id, kind, title, status, summary, next_due_ms, last_event_ms, metadata_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   status=excluded.status,
		   summary=excluded.summary,
		   next_due_ms=excluded.next_due_ms,
		   last_event_ms=excluded.last_event_ms,
		   metadata_json=excluded.metadata_json`,
		threadID, event.ChatID, nullableInt64(event.UserID), "request", title, threadStatus, fallbackString(event.Text, "求片线程更新"), nextDue, ts, metadataJSON,
	)
	return err
}

func (cs *CognitionServer) computePressures(req *RespondRequest) (map[string]float64, error) {
	ts := nowMS()
	var recentCount, openThreads int
	if err := cs.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE chat_id = ? AND timestamp_ms >= ?`, req.ChatID, ts-60*60*1000).Scan(&recentCount); err != nil {
		return nil, err
	}
	if err := cs.db.QueryRow(`SELECT COUNT(*) FROM narrative_threads WHERE chat_id = ? AND status = 'open'`, req.ChatID).Scan(&openThreads); err != nil {
		return nil, err
	}

	var lastInteraction sql.NullInt64
	if req.UserID != 0 {
		_ = cs.db.QueryRow(`SELECT last_interaction_ms FROM relationships WHERE chat_id = ? AND user_id = ?`, req.ChatID, req.UserID).Scan(&lastInteraction)
	}
	deltaMS := float64(0)
	if lastInteraction.Valid {
		deltaMS = float64(ts - lastInteraction.Int64)
	}
	decayDays := req.RelationshipDecayD
	if decayDays <= 0 {
		decayDays = cs.relationshipDecayDays
	}
	if decayDays <= 0 {
		decayDays = 14
	}

	return map[string]float64{
		"P1": clampFloat(float64(recentCount)*0.35, 0.1, 4),
		"P2": clampFloat(func() float64 {
			if strings.TrimSpace(req.KnowledgeSummary) != "" {
				return 0.4
			}
			return 1.2
		}(), 0.2, 2.2),
		"P3": clampFloat(deltaMS/(1000*60*60*24*float64(decayDays)), 0.1, 3.5),
		"P4": clampFloat(float64(openThreads)*0.8, 0.1, 4),
		"P5": clampFloat(func() float64 {
			base := 0.5
			if req.IsPrivate {
				base = 1.2
			}
			if strings.Contains(req.Text, "?") || strings.Contains(req.Text, "？") || containsAny(req.Text, "吗", "呢", "如何", "怎么") {
				base += 1.1
			} else {
				base += 0.2
			}
			return base
		}(), 0.2, 3.5),
		"P6": clampFloat(func() float64 {
			if containsAny(req.Text, "最新", "推荐", "想看", "有没有", "什么", "推荐一下", "新闻", "发现") {
				return 1.4
			}
			return 0.4
		}(), 0.1, 2.5),
	}, nil
}

func (cs *CognitionServer) selectVoice(chatID int64, pressures map[string]float64) (string, cognitionPersonaState, error) {
	persona, err := cs.getPersonaState(chatID)
	if err != nil {
		return "", cognitionPersonaState{}, err
	}
	scores := map[string]float64{
		"diligence":   persona.Diligence * (pressures["P4"] + pressures["P5"]),
		"curiosity":   persona.Curiosity * (pressures["P2"] + pressures["P6"]),
		"sociability": persona.Sociability * (pressures["P3"] + pressures["P1"]*0.5),
		"caution":     persona.Caution * (pressures["P5"] + 0.6),
	}
	voice := "diligence"
	best := -1.0
	for name, score := range scores {
		if score > best {
			best = score
			voice = name
		}
	}
	return voice, persona, nil
}

func (cs *CognitionServer) getPersonaState(chatID int64) (cognitionPersonaState, error) {
	var persona cognitionPersonaState
	err := cs.db.QueryRow(`SELECT diligence, curiosity, sociability, caution FROM persona_state WHERE chat_id = ?`, chatID).
		Scan(&persona.Diligence, &persona.Curiosity, &persona.Sociability, &persona.Caution)
	if err == nil {
		return persona, nil
	}
	if err != sql.ErrNoRows {
		return cognitionPersonaState{}, err
	}
	persona = cognitionPersonaState{Diligence: 0.25, Curiosity: 0.25, Sociability: 0.25, Caution: 0.25}
	_, err = cs.db.Exec(
		`INSERT INTO persona_state (chat_id, diligence, curiosity, sociability, caution, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		chatID, persona.Diligence, persona.Curiosity, persona.Sociability, persona.Caution, nowMS(),
	)
	return persona, err
}

func (cs *CognitionServer) buildDynamicTail(req *RespondRequest) (string, error) {
	parts := make([]string, 0, 8)

	if req.UserID != 0 {
		var userTier, responsePreference, interestTags, embyPreference, emotion string
		var familiarity, trustLevel float64
		err := cs.db.QueryRow(
			`SELECT user_tier, familiarity, response_preference, interest_tags, emby_preference, trust_level, last_emotional_signal FROM beliefs WHERE chat_id = ? AND user_id = ?`,
			req.ChatID, req.UserID,
		).Scan(&userTier, &familiarity, &responsePreference, &interestTags, &embyPreference, &trustLevel, &emotion)
		if err == nil {
			parts = append(parts, fmt.Sprintf("当前用户画像：tier=%s, familiarity=%.2f, trust=%.2f, emotion=%s.", userTier, familiarity, trustLevel, emotion))
			if interestTags != "" {
				parts = append(parts, "兴趣标签："+interestTags+"。")
			}
			if embyPreference != "" {
				parts = append(parts, "资源偏好："+embyPreference+"。")
			}
			_ = responsePreference
		}

		var rel cognitionRelationship
		err = cs.db.QueryRow(
			`SELECT warmth, reciprocity, responsiveness, familiarity, last_interaction_ms, last_proactive_ms FROM relationships WHERE chat_id = ? AND user_id = ?`,
			req.ChatID, req.UserID,
		).Scan(&rel.Warmth, &rel.Reciprocity, &rel.Responsiveness, &rel.Familiarity, &rel.LastInteractionMS, &rel.LastProactiveMS)
		if err == nil {
			parts = append(parts, fmt.Sprintf("关系向量：warmth=%.2f, reciprocity=%.2f, responsiveness=%.2f, familiarity=%.2f.", rel.Warmth, rel.Reciprocity, rel.Responsiveness, rel.Familiarity))
		}
	}

	threadRows, err := cs.db.Query(`SELECT title, summary FROM narrative_threads WHERE chat_id = ? AND status = 'open' ORDER BY last_event_ms DESC LIMIT 5`, req.ChatID)
	if err != nil {
		return "", err
	}
	defer threadRows.Close()
	threadParts := make([]string, 0, 5)
	for threadRows.Next() {
		var title, summary string
		if err := threadRows.Scan(&title, &summary); err != nil {
			return "", err
		}
		threadParts = append(threadParts, fmt.Sprintf("%s(%s)", title, summary))
	}
	if len(threadParts) > 0 {
		parts = append(parts, "开放线程："+strings.Join(threadParts, "；"))
	}

	diaryRows, err := cs.db.Query(`SELECT summary FROM diary_entries WHERE chat_id = ? ORDER BY created_at_ms DESC LIMIT 5`, req.ChatID)
	if err != nil {
		return "", err
	}
	defer diaryRows.Close()
	diaryParts := make([]string, 0, 5)
	for diaryRows.Next() {
		var summary string
		if err := diaryRows.Scan(&summary); err != nil {
			return "", err
		}
		diaryParts = append(diaryParts, summary)
	}
	if len(diaryParts) > 0 {
		parts = append(parts, "近期日记："+strings.Join(diaryParts, "；"))
	}

	if strings.TrimSpace(req.KnowledgeSummary) != "" {
		parts = append(parts, "知识摘要："+truncateString(req.KnowledgeSummary, 1200))
	}
	if len(req.SkillSummaries) > 0 {
		parts = append(parts, "可按需读取的技能："+strings.Join(req.SkillSummaries, "；"))
	}
	if len(req.JobSummaries) > 0 {
		parts = append(parts, "当前任务摘要："+strings.Join(req.JobSummaries, "；"))
	}
	if len(req.StickerSummaries) > 0 {
		parts = append(parts, "可用贴纸："+strings.Join(req.StickerSummaries, "；")+"。如需发送贴纸，使用 chat.sticker <别名>。")
	}
	return strings.Join(parts, "\n"), nil
}

func (cs *CognitionServer) buildStaticPrompt(req *RespondRequest, voice string, pressures map[string]float64) string {
	pressureJSON, _ := json.Marshal(pressures)
	return strings.Join([]string{
		fallbackString(strings.TrimSpace(req.StaticPrompt), "你是 EmbyRadar 的自治型 AI 助手。"),
		"人格种子：" + fallbackString(req.PersonaSeed, "谨慎、会长期记住关系变化、保持直接。") + "。",
		"你使用单一工具 run(command)。命令空间仅限：chat.say、chat.reply、chat.sticker、chat.pin、web.search、tmdb.search、emby.search、emby.latest、embyboss.user、skill.read、job.read、job.write、memory.search、diary.write。",
		"复杂写入命令使用 JSON 作为参数体，例如：job.write {\"job_id\":\"daily-check\",\"content\":\"...\"}。",
		"绝对禁止编造系统事实。需要外部事实时先用 run(command) 取证。",
		"回复保持自然、简洁、像长期生活在群里的实体，不要自称在执行流程，也不要暴露 pressure、voice、tool budget 这些内部术语。",
		"默认不要一口气发很长一段。更像真人聊天：一条消息尽量只说 1 到 2 句；有情绪、有解释、有追问时，可以拆成 2 到 3 条短消息连续发出，但不要刷屏。",
		"如果你已经通过 chat.say、chat.reply 或 chat.sticker 把回复发出去了，最终可以不再输出额外正文，避免重复。",
		"贴纸只在情绪明显、撒娇、安慰、活跃气氛时使用，不要每次都发，也不要把贴纸当成固定结尾。",
		"不要自我重复，不要复述用户原话，不要写像客服或作文一样的收尾。能短就短，能直接就直接。",
		fmt.Sprintf("当前主导倾向：%s。六维压力快照：%s。", voice, string(pressureJSON)),
	}, "\n\n")
}

func (cs *CognitionServer) buildUserPrompt(req *RespondRequest, dynamicTail string) string {
	historyParts := make([]string, 0, 12)
	start := 0
	if len(req.RecentContext) > 12 {
		start = len(req.RecentContext) - 12
	}
	for _, item := range req.RecentContext[start:] {
		historyParts = append(historyParts, fmt.Sprintf("%s: %s", item.Role, item.Text))
	}
	segments := []string{dynamicTail}
	if len(historyParts) > 0 {
		segments = append(segments, "近期上下文：\n"+strings.Join(historyParts, "\n"))
	}
	segments = append(segments, fmt.Sprintf("当前消息来自 %s：%s", fallbackString(req.UserName, fmt.Sprint(req.UserID)), req.Text))
	return strings.Join(filterNonEmpty(segments), "\n\n")
}

func (cs *CognitionServer) applyDeliveryBudgets(req *RespondRequest, command string, messageDeliveries, stickerDeliveries int) (string, error) {
	command = strings.TrimSpace(command)
	switch {
	case strings.HasPrefix(command, "chat.say"):
		if messageDeliveries >= 3 {
			return command, fmt.Errorf("message budget exceeded: 最多连续发送 3 条消息")
		}
		name, rest := splitCommand(command)
		_ = name
		return "chat.say " + cs.compactOutgoingMessage(req, rest), nil
	case strings.HasPrefix(command, "chat.reply"):
		if messageDeliveries >= 3 {
			return command, fmt.Errorf("message budget exceeded: 最多连续发送 3 条消息")
		}
		name, rest := splitCommand(command)
		_ = name
		return "chat.reply " + cs.compactOutgoingMessage(req, rest), nil
	case strings.HasPrefix(command, "chat.sticker"):
		if stickerDeliveries >= 1 {
			return command, fmt.Errorf("sticker budget exceeded: 单轮最多发送 1 张贴纸")
		}
		if !cs.shouldAllowSticker(req) {
			return command, fmt.Errorf("sticker gate blocked: 当前情境不适合发送贴纸")
		}
	}
	return command, nil
}

func (cs *CognitionServer) shouldAllowSticker(req *RespondRequest) bool {
	text := strings.TrimSpace(req.Text)
	emotion := cs.detectEmotion(text)
	if emotion == "negative" || emotion == "positive" {
		return true
	}
	return containsAny(text, "哭", "难过", "委屈", "抱抱", "可爱", "紧张", "开心", "哈哈", "呜呜", "对不起", "没事吧")
}

func (cs *CognitionServer) compactOutgoingMessage(req *RespondRequest, text string) string {
	text = sanitizeChatText(text) // 先清洗 JSON 包裹和引号
	if text == "" || cs.wantsDetailedAnswer(req.Text) {
		return text
	}
	text = cs.limitSentences(text, 2)
	text = truncateString(text, 120)
	return strings.TrimSpace(text)
}

func (cs *CognitionServer) compactReplyIfNeeded(req *RespondRequest, text string) string {
	text = sanitizeChatText(text) // 先清洗 JSON 包裹和引号
	if text == "" || cs.wantsDetailedAnswer(req.Text) {
		return text
	}
	if len(text) <= 120 && strings.Count(text, "\n") <= 1 {
		return text
	}
	text = cs.limitSentences(text, 3)
	text = truncateString(text, 180)
	return strings.TrimSpace(text)
}

func (cs *CognitionServer) wantsDetailedAnswer(text string) bool {
	return containsAny(text, "详细", "具体", "步骤", "教程", "如何", "怎么", "为什么", "分析", "总结", "列出", "帮我写", "解释", "说明")
}

func (cs *CognitionServer) limitSentences(text string, maxSentences int) string {
	if maxSentences <= 0 {
		return text
	}
	count := 0
	for i, r := range text {
		switch r {
		case '。', '！', '？', '!', '?', '\n':
			count++
			if count >= maxSentences {
				return strings.TrimSpace(text[:i+len(string(r))])
			}
		}
	}
	return text
}

func (cs *CognitionServer) claimAction() (*PlannedAction, error) {
	if _, err := cs.maybeCreateThreadAction(); err != nil {
		return nil, err
	}
	if _, err := cs.maybeCreateRelationshipAction(); err != nil {
		return nil, err
	}

	var row cognitionActionRow
	err := cs.db.QueryRow(
		`SELECT id, chat_id, user_id, kind, command, status, due_ms, last_error, metadata_json
		 FROM scheduled_actions WHERE status = 'pending' AND due_ms <= ? ORDER BY due_ms ASC LIMIT 1`,
		nowMS(),
	).Scan(&row.ID, &row.ChatID, &row.UserID, &row.Kind, &row.Command, &row.Status, &row.DueMS, &row.LastError, &row.MetadataJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := cs.db.Exec(`UPDATE scheduled_actions SET status = 'in_progress', updated_at_ms = ? WHERE id = ?`, nowMS(), row.ID); err != nil {
		return nil, err
	}
	meta := map[string]any{}
	_ = json.Unmarshal([]byte(row.MetadataJSON), &meta)
	action := &PlannedAction{
		ID:      row.ID,
		Kind:    row.Kind,
		Command: row.Command,
		Context: HostCommandContext{
			ChatID:         row.ChatID,
			SenderID:       nullInt64Value(row.UserID),
			IsPrivate:      true,
			AllowSensitive: false,
		},
		Meta: meta,
	}
	return action, nil
}

func (cs *CognitionServer) maybeCreateRelationshipAction() (string, error) {
	if cs.inQuietHours() {
		return "", nil
	}
	thresholdMS := int64(cs.relationshipDecayDays) * 24 * 60 * 60 * 1000
	cooldownMS := int64(cs.proactiveCooldownMinutes) * 60 * 1000

	var rel cognitionRelationship
	var chatID int64
	var userID int64
	err := cs.db.QueryRow(
		`SELECT chat_id, user_id, warmth, reciprocity, responsiveness, familiarity, last_interaction_ms, last_proactive_ms
		 FROM relationships
		 WHERE last_interaction_ms <= ? AND (last_proactive_ms IS NULL OR last_proactive_ms <= ?)
		 ORDER BY last_interaction_ms ASC
		 LIMIT 1`,
		nowMS()-thresholdMS, nowMS()-cooldownMS,
	).Scan(&chatID, &userID, &rel.Warmth, &rel.Reciprocity, &rel.Responsiveness, &rel.Familiarity, &rel.LastInteractionMS, &rel.LastProactiveMS)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	actionID := fmt.Sprintf("action-%d", nowMS())
	_, err = cs.db.Exec(
		`INSERT INTO scheduled_actions (id, chat_id, user_id, kind, command, status, due_ms, last_error, metadata_json, created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', '{}', ?, ?)`,
		actionID, chatID, userID, "relationship_checkin", "chat.say 想起你了，最近怎么样？", "pending", nowMS(), nowMS(), nowMS(),
	)
	if err != nil {
		return "", err
	}
	return actionID, nil
}

func (cs *CognitionServer) maybeCreateThreadAction() (string, error) {
	if cs.inQuietHours() {
		return "", nil
	}
	var threadID, title, summary, metadataJSON string
	var chatID int64
	var userID sql.NullInt64
	err := cs.db.QueryRow(
		`SELECT id, chat_id, user_id, title, summary, metadata_json FROM narrative_threads
		 WHERE status = 'open' AND next_due_ms > 0 AND next_due_ms <= ?
		 ORDER BY next_due_ms ASC LIMIT 1`,
		nowMS(),
	).Scan(&threadID, &chatID, &userID, &title, &summary, &metadataJSON)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	actionID := fmt.Sprintf("action-%d", nowMS())
	command := fmt.Sprintf("chat.say 提醒一下，%s 这件事我还记着：%s", title, summary)
	if _, err := cs.db.Exec(
		`INSERT INTO scheduled_actions (id, chat_id, user_id, kind, command, status, due_ms, last_error, metadata_json, created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		actionID, chatID, nullableSQLInt64(userID), "thread_followup", command, "pending", nowMS(), metadataJSON, nowMS(), nowMS(),
	); err != nil {
		return "", err
	}
	_, err = cs.db.Exec(`UPDATE narrative_threads SET next_due_ms = ?, last_event_ms = ? WHERE id = ?`, nowMS()+12*60*60*1000, nowMS(), threadID)
	if err != nil {
		return "", err
	}
	return actionID, nil
}

func (cs *CognitionServer) completeAction(actionID string, req *ActionResultRequest) error {
	status := "failed"
	if req.Success {
		status = "completed"
	}
	if _, err := cs.db.Exec(`UPDATE scheduled_actions SET status = ?, last_error = ?, updated_at_ms = ? WHERE id = ?`, status, req.ErrorText, nowMS(), actionID); err != nil {
		return err
	}

	var chatID int64
	var userID sql.NullInt64
	err := cs.db.QueryRow(`SELECT chat_id, user_id FROM scheduled_actions WHERE id = ?`, actionID).Scan(&chatID, &userID)
	if err != nil {
		return err
	}
	if userID.Valid {
		_, _ = cs.db.Exec(`UPDATE relationships SET last_proactive_ms = ?, updated_at_ms = ? WHERE chat_id = ? AND user_id = ?`, nowMS(), nowMS(), chatID, userID.Int64)
	}
	return nil
}

func (cs *CognitionServer) coerceIntent(raw string) *RequestIntentResponse {
	normalized := strings.TrimSpace(raw)
	normalized = strings.TrimPrefix(normalized, "```json")
	normalized = strings.TrimPrefix(normalized, "```")
	normalized = strings.TrimSuffix(normalized, "```")
	normalized = strings.TrimSpace(normalized)
	var parsed map[string]any
	_ = json.Unmarshal([]byte(normalized), &parsed)
	resp := &RequestIntentResponse{}
	resp.Name, _ = parsed["name"].(string)
	resp.Type, _ = parsed["type"].(string)
	resp.Year, _ = parsed["year"].(string)
	if v, ok := parsed["is_remaster"].(bool); ok {
		resp.IsRemaster = v
	}
	switch v := parsed["season"].(type) {
	case float64:
		resp.Season = int(v)
	case int:
		resp.Season = v
	}
	return resp
}

func (cs *CognitionServer) extractAssistantText(msg *ChatMessage) string {
	var raw string
	if strings.TrimSpace(msg.Content.Text) != "" {
		raw = strings.TrimSpace(msg.Content.Text)
	} else if len(msg.Content.Parts) > 0 {
		lines := make([]string, 0, len(msg.Content.Parts))
		for _, part := range msg.Content.Parts {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				lines = append(lines, strings.TrimSpace(part.Text))
			}
		}
		raw = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	// 清洗 AI 可能输出的 JSON 包裹、引号、思维链等
	return sanitizeChatText(raw)
}

func (cs *CognitionServer) detectEmotion(text string) string {
	switch {
	case containsAny(text, "难受", "伤心", "烦", "崩溃", "累", "孤独", "不开心", "委屈"):
		return "negative"
	case containsAny(text, "开心", "高兴", "哈哈", "喜欢", "爱", "棒", "爽"):
		return "positive"
	case strings.Contains(text, "?") || strings.Contains(text, "？") || containsAny(text, "吗", "呢", "如何", "怎么"):
		return "seeking"
	default:
		return "neutral"
	}
}

func (cs *CognitionServer) inferInterestTags(text string) string {
	tags := make([]string, 0, 4)
	if containsAny(text, "电影", "电视剧", "剧集", "片子", "动漫") {
		tags = append(tags, "影视")
	}
	if containsAny(strings.ToLower(text), "emby") || containsAny(text, "洗版", "资源", "入库") {
		tags = append(tags, "Emby")
	}
	if containsAny(text, "音乐", "歌", "专辑") {
		tags = append(tags, "音乐")
	}
	if containsAny(text, "求片", "想看", "有没有") {
		tags = append(tags, "求片")
	}
	return strings.Join(uniqueStrings(tags), ",")
}

func (cs *CognitionServer) inferEmbyPreference(text string) string {
	switch {
	case containsAny(strings.ToLower(text), "4k", "hdr") || containsAny(text, "蓝光", "洗版", "更高清"):
		return "quality-first"
	case containsAny(strings.ToLower(text), "season") || strings.Contains(text, "第") && strings.Contains(text, "季"):
		return "series"
	case containsAny(text, "电影", "电影版"):
		return "movie"
	default:
		return ""
	}
}

func (cs *CognitionServer) normalizeText(text string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}

func (cs *CognitionServer) inQuietHours() bool {
	hour := time.Now().Hour()
	for _, item := range cs.quietHours {
		if item == hour {
			return true
		}
	}
	return false
}

func (cs *CognitionServer) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func newAIClientFromLLM(llm LLMConfig) *AIClient {
	return &AIClient{
		BaseURL:     llm.BaseURL,
		APIKey:      llm.APIKey,
		Model:       llm.Model,
		MaxTokens:   llm.MaxTokens,
		Temperature: llm.Temperature,
		HTTPClient:  &http.Client{}, // 超时由 context 控制，不再硬编码
	}
}

func nowMS() int64 {
	return time.Now().UnixMilli()
}

func clampFloat(value, min, max float64) float64 {
	return math.Max(min, math.Min(max, value))
}

func truncateString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func containsAny(text string, patterns ...string) bool {
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

func filterNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func metadataBool(meta map[string]any, key string) bool {
	if meta == nil {
		return false
	}
	v, ok := meta[key]
	if !ok {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func metadataString(meta map[string]any, key string, fallback string) string {
	if meta == nil {
		return fallback
	}
	v, ok := meta[key]
	if !ok || v == nil {
		return fallback
	}
	switch typed := v.(type) {
	case string:
		if typed == "" {
			return fallback
		}
		return typed
	case float64:
		return fmt.Sprintf("%.0f", typed)
	case int:
		return fmt.Sprintf("%d", typed)
	default:
		return fallback
	}
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableSQLInt64(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func nullInt64Value(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
