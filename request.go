package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// RequestState 求片会话状态
type RequestState int

const (
	StateIdle        RequestState = iota // 无活跃会话
	StateWaitConfirm                     // 等待用户确认 TMDB 匹配结果
)

// sessionTimeout 求片会话超时时间
const sessionTimeout = 5 * time.Minute

// RequestSession 单个求片会话，跟踪用户的求片流程状态
type RequestSession struct {
	UserID     int64             // 请求者 Telegram 用户 ID
	UserName   string            // 请求者用户名
	ChatID     int64             // 所在群聊 ID
	TMDBResult *TMDBSearchResult // AI 选中的 TMDB 条目
	IsRemaster bool              // 是否为洗版请求
	State      RequestState      // 当前会话状态
	CreatedAt  time.Time         // 会话创建时间
	ExpiresAt  time.Time         // 会话超时时间（5分钟）
}

// RequestHandler 求片请求处理器，管理所有活跃的求片会话
type RequestHandler struct {
	sessions map[string]*RequestSession // key: "chatID:userID"，仅用于短期 TMDB 确认交互
	mu       sync.RWMutex
	store    *RequestStore // 数据库访问层，持久化求片记录
}

// NewRequestHandler 创建求片处理器实例，注入数据库访问层
func NewRequestHandler(store *RequestStore) *RequestHandler {
	return &RequestHandler{
		sessions: make(map[string]*RequestSession),
		store:    store,
	}
}

// sessionKey 生成会话的唯一键，格式为 "chatID:userID"
func sessionKey(chatID, userID int64) string {
	return fmt.Sprintf("%d:%d", chatID, userID)
}

// GetSession 获取用户的活跃求片会话
// 如果会话已过期，自动删除并返回 nil
func (rh *RequestHandler) GetSession(chatID, userID int64) *RequestSession {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	key := sessionKey(chatID, userID)
	session, ok := rh.sessions[key]
	if !ok {
		return nil
	}

	// 会话已过期，删除并返回 nil
	if time.Now().After(session.ExpiresAt) {
		delete(rh.sessions, key)
		return nil
	}

	return session
}

// CleanExpired 遍历所有会话，删除已过期的
func (rh *RequestHandler) CleanExpired() {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	now := time.Now()
	for key, session := range rh.sessions {
		if now.After(session.ExpiresAt) {
			delete(rh.sessions, key)
		}
	}
}

// InventoryCheckResult 库存查重结果，用于决定求片请求是否继续流转
type InventoryCheckResult struct {
	Allowed           bool            // 是否允许继续
	Reason            string          // 拒绝原因或说明
	ExistingItems     []EmbyMediaItem // 已存在的条目，用于展示
	IsRemasterRequest bool            // 是否标记为洗版请求
}

// CheckInventory 根据 Emby 搜索结果和洗版标志判断求片请求是否允许继续
// 规则：
//   - Emby 结果为空 → 允许（库中无该资源）
//   - 非空且 isRemaster 为 false → 拒绝，附带已有资源信息
//   - 非空且 isRemaster 为 true → 允许，标记为洗版请求
func CheckInventory(embyItems []EmbyMediaItem, isRemaster bool) *InventoryCheckResult {
	// Emby 搜索结果为空，库中无该资源，允许请求继续
	if len(embyItems) == 0 {
		return &InventoryCheckResult{
			Allowed: true,
			Reason:  "媒体库中未找到该资源",
		}
	}

	// 库中已有资源且用户表达了洗版意图，允许继续并标记
	if isRemaster {
		return &InventoryCheckResult{
			Allowed:           true,
			Reason:            "洗版请求，现有资源将被替换",
			ExistingItems:     embyItems,
			IsRemasterRequest: true,
		}
	}

	// 库中已有资源且非洗版请求，拒绝
	// 构建已有资源信息用于反馈给用户
	info := "媒体库中已存在以下资源：\n"
	for _, item := range embyItems {
		info += fmt.Sprintf("  - %s\n", item.FormatMediaInfo())
	}
	return &InventoryCheckResult{
		Allowed:       false,
		Reason:        info,
		ExistingItems: embyItems,
	}
}

// CallbackData 管理员审批回调数据结构
// 用于在 Inline Keyboard 按钮的回调中传递求片请求信息
type CallbackData struct {
	Action string // "approve" 或 "reject"
	ChatID int64  // 原求片群聊 ID
	UserID int64  // 请求者用户 ID
	TMDBID int    // TMDB 条目 ID
	Title  string // 影视名称，仅用于通知消息，不编码到回调数据中
}

// callbackPrefix 回调数据前缀，用于区分求片相关的回调
const callbackPrefix = "request"

// FormatCallbackData 将回调数据编码为字符串
// 格式：request:{action}:{chatID}:{userID}:{tmdbID}
// 注意：Title 不编码到回调数据中，因为 Telegram 回调数据有 64 字节限制
func FormatCallbackData(data *CallbackData) string {
	return fmt.Sprintf("%s:%s:%d:%d:%d",
		callbackPrefix, data.Action, data.ChatID, data.UserID, data.TMDBID)
}

// ParseCallbackData 从回调字符串解析数据
// 期望格式：request:{action}:{chatID}:{userID}:{tmdbID}
// action 只能为 "approve" 或 "reject"，其他值返回错误
func ParseCallbackData(data string) (*CallbackData, error) {
	parts := strings.Split(data, ":")
	if len(parts) != 5 {
		return nil, fmt.Errorf("回调数据格式错误，期望 5 段，实际 %d 段", len(parts))
	}

	if parts[0] != callbackPrefix {
		return nil, fmt.Errorf("回调数据前缀错误：%s", parts[0])
	}

	action := parts[1]
	if action != "approve" && action != "reject" {
		return nil, fmt.Errorf("无效的操作类型：%s，仅支持 approve 或 reject", action)
	}

	chatID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("解析 ChatID 失败：%w", err)
	}

	userID, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("解析 UserID 失败：%w", err)
	}

	tmdbID, err := strconv.Atoi(parts[4])
	if err != nil {
		return nil, fmt.Errorf("解析 TMDBID 失败：%w", err)
	}

	return &CallbackData{
		Action: action,
		ChatID: chatID,
		UserID: userID,
		TMDBID: tmdbID,
	}, nil
}

// FormatAdminMessage 格式化转发给管理员的求片请求消息
// 包含影片信息、请求者信息、洗版标记和来源群组
func FormatAdminMessage(session *RequestSession, groupName string, existingItems []EmbyMediaItem) string {
	result := session.TMDBResult

	// 构建显示标题：中文标题 / 原始标题
	displayTitle := result.GetDisplayTitle()
	originalTitle := result.OriginalTitle
	if result.MediaType == "tv" {
		originalTitle = result.OriginalName
	}
	titleLine := displayTitle
	if originalTitle != "" && originalTitle != displayTitle {
		titleLine = displayTitle + " / " + originalTitle
	}

	// 媒体类型中文描述
	mediaTypeStr := "电影"
	if result.MediaType == "tv" {
		mediaTypeStr = "电视剧"
	}

	var sb strings.Builder
	sb.WriteString("🎬 新求片请求\n\n")
	sb.WriteString(fmt.Sprintf("📌 影片：%s\n", titleLine))
	sb.WriteString(fmt.Sprintf("📅 年份：%s\n", result.GetYear()))
	sb.WriteString(fmt.Sprintf("🎭 类型：%s\n", mediaTypeStr))
	sb.WriteString(fmt.Sprintf("🔗 TMDB：%s\n", result.GetTMDBURL()))
	sb.WriteString(fmt.Sprintf("👤 请求者：%s (ID: %d)\n", session.UserName, session.UserID))

	// 仅当洗版请求时显示洗版标记和现有版本质量信息
	if session.IsRemaster {
		qualityInfo := ""
		for _, item := range existingItems {
			if qualityInfo != "" {
				qualityInfo += "、"
			}
			qualityInfo += item.GetResolution()
		}
		if qualityInfo != "" {
			sb.WriteString(fmt.Sprintf("⚠️ 洗版请求：是（现有版本：%s）\n", qualityInfo))
		} else {
			sb.WriteString("⚠️ 洗版请求：是\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n来自群组：%s", groupName))

	return sb.String()
}

// BuildApprovalKeyboard 构建管理员审批用的 Inline Keyboard
// 包含 "✅ 通过" 和 "❌ 拒绝" 两个按钮，回调数据中编码了求片请求的关键信息
func BuildApprovalKeyboard(session *RequestSession) tgbotapi.InlineKeyboardMarkup {
	approveData := FormatCallbackData(&CallbackData{
		Action: "approve",
		ChatID: session.ChatID,
		UserID: session.UserID,
		TMDBID: session.TMDBResult.ID,
	})
	rejectData := FormatCallbackData(&CallbackData{
		Action: "reject",
		ChatID: session.ChatID,
		UserID: session.UserID,
		TMDBID: session.TMDBResult.ID,
	})

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ 通过", approveData),
			tgbotapi.NewInlineKeyboardButtonData("❌ 拒绝", rejectData),
		),
	)
}

// aiIntentResult AI 分析用户求片意图后返回的结构化结果
type aiIntentResult struct {
	Name       string `json:"name"`        // 影视名称
	Type       string `json:"type"`        // "movie" 或 "tv"
	Year       string `json:"year"`        // 年份（可能为空）
	IsRemaster bool   `json:"is_remaster"` // 是否为洗版请求
}

// HandleRequest 处理求片请求入口
// 流程：AI 意图分析 → TMDB 搜索 → AI 选择最佳匹配 → 用户确认
func (rh *RequestHandler) HandleRequest(ch *ChatHandler, msg *tgbotapi.Message, text string) {
	chatID := msg.Chat.ID
	userID := msg.From.ID

	// 第一步：调用 AI 分析用户输入，提取影视名称、类型、年份、洗版意图
	intentMessages := []ChatMessage{
		{
			Role: "system",
			Content: MessageContent{Text: "你是一个影视信息提取助手。请从用户的文本中提取以下信息并以 JSON 格式返回：\n" +
				"1. name: 影视作品名称\n" +
				"2. type: 类型，\"movie\"（电影）或 \"tv\"（电视剧），无法判断时留空\n" +
				"3. year: 年份，无法判断时留空\n" +
				"4. is_remaster: 是否有洗版意图（用户想要更高清版本），布尔值\n\n" +
				"只返回 JSON，不要包含其他文字。示例：{\"name\":\"流浪地球2\",\"type\":\"movie\",\"year\":\"2023\",\"is_remaster\":false}"},
		},
		{
			Role:    "user",
			Content: MessageContent{Text: text},
		},
	}

	aiResp, err := ch.aiClient.ChatCompletion(intentMessages, nil)
	if err != nil {
		log.Printf("[求片] AI 意图分析失败: %v", err)
		reply := tgbotapi.NewMessage(chatID, "⚠️ AI 暂时无法处理你的请求，请稍后再试")
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		return
	}

	// 第二步：解析 AI 返回的 JSON，提取关键信息
	var intent aiIntentResult
	respText := strings.TrimSpace(aiResp.Content.Text)
	// 处理 AI 可能返回的 markdown 代码块包裹
	respText = strings.TrimPrefix(respText, "```json")
	respText = strings.TrimPrefix(respText, "```")
	respText = strings.TrimSuffix(respText, "```")
	respText = strings.TrimSpace(respText)

	if err := json.Unmarshal([]byte(respText), &intent); err != nil {
		log.Printf("[求片] 解析 AI 意图结果失败: %v, 原始响应: %s", err, aiResp.Content.Text)
		reply := tgbotapi.NewMessage(chatID, "无法识别你想要的影视作品，请提供更具体的片名或描述")
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		return
	}

	if strings.TrimSpace(intent.Name) == "" {
		reply := tgbotapi.NewMessage(chatID, "无法识别你想要的影视作品，请提供更具体的片名或描述")
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		return
	}

	// 第三步：获取群组的 TMDBClient，调用 SearchMulti 搜索 TMDB
	tmdbClient := ch.tmdbMap[chatID]
	if tmdbClient == nil {
		reply := tgbotapi.NewMessage(chatID, "⚠️ 当前群组未配置 TMDB，无法搜索影视信息")
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		return
	}

	results, err := tmdbClient.SearchMulti(intent.Name, intent.Type)
	if err != nil {
		log.Printf("[求片] TMDB 搜索失败: %v", err)
		reply := tgbotapi.NewMessage(chatID, "⚠️ 搜索影视信息时出错，请稍后再试")
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		return
	}

	// 第四步：搜索结果为空，通知用户
	if len(results) == 0 {
		reply := tgbotapi.NewMessage(chatID, "未找到相关影视，请尝试更精确的片名")
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		return
	}

	// 第五步：如果有多个结果，调用 AI 从候选列表中选择最匹配的条目
	selectedIdx := 0
	if len(results) > 1 {
		formatted := FormatTMDBResultsForAI(results)
		selectMessages := []ChatMessage{
			{
				Role: "system",
				Content: MessageContent{Text: "你是一个影视匹配助手。用户想找一部影视作品，以下是 TMDB 搜索到的候选列表。" +
					"请根据用户的描述选择最匹配的一条，只返回序号数字（如 1、2、3），不要返回其他内容。"},
			},
			{
				Role:    "user",
				Content: MessageContent{Text: fmt.Sprintf("用户描述：%s\n\n候选列表：\n%s", text, formatted)},
			},
		}

		selectResp, err := ch.aiClient.ChatCompletion(selectMessages, nil)
		if err != nil {
			log.Printf("[求片] AI 选择候选条目失败: %v，默认选择第一条", err)
		} else {
			// 解析 AI 返回的序号
			idxStr := strings.TrimSpace(selectResp.Content.Text)
			if idx, err := strconv.Atoi(idxStr); err == nil && idx >= 1 && idx <= len(results) {
				selectedIdx = idx - 1
			}
		}
	}

	selected := &results[selectedIdx]

	// 第六步：向用户发送确认消息
	confirmText := fmt.Sprintf("你要找的是《%s》（%s年）吗？回复「是」确认，「不是」重新搜索",
		selected.GetDisplayTitle(), selected.GetYear())
	reply := tgbotapi.NewMessage(chatID, confirmText)
	reply.ReplyToMessageID = msg.MessageID
	ch.bot.Send(reply)

	// 第七步：创建 StateWaitConfirm 状态的会话，存入 sessions map
	userName := msg.From.FirstName
	if msg.From.LastName != "" {
		userName += " " + msg.From.LastName
	}

	rh.mu.Lock()
	rh.sessions[sessionKey(chatID, userID)] = &RequestSession{
		UserID:     userID,
		UserName:   userName,
		ChatID:     chatID,
		TMDBResult: selected,
		IsRemaster: intent.IsRemaster,
		State:      StateWaitConfirm,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(sessionTimeout),
	}
	rh.mu.Unlock()
}

// HandleConfirmation 处理用户对 TMDB 匹配结果的确认或否认回复
// 流程：获取会话 → 判断确认/否认 → 库存查重 → 转发管理员或拒绝
func (rh *RequestHandler) HandleConfirmation(ch *ChatHandler, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	userID := msg.From.ID

	// 获取用户的活跃求片会话
	session := rh.GetSession(chatID, userID)
	if session == nil || session.State != StateWaitConfirm {
		return
	}

	text := strings.TrimSpace(strings.ToLower(msg.Text))

	// 判断用户回复是确认还是否认
	confirmKeywords := []string{"是", "对", "没错", "确认", "yes", "y"}
	denyKeywords := []string{"不是", "不对", "错了", "否", "no", "n", "换一个"}

	isConfirm := false
	isDeny := false

	// 优先匹配否认关键词（"不是"包含"是"，需先判断否认）
	for _, kw := range denyKeywords {
		if text == kw {
			isDeny = true
			break
		}
	}
	if !isDeny {
		for _, kw := range confirmKeywords {
			if text == kw {
				isConfirm = true
				break
			}
		}
	}

	// 既不是确认也不是否认，不处理
	if !isConfirm && !isDeny {
		return
	}

	// 用户否认：删除会话，提示重新发起
	if isDeny {
		rh.deleteSession(chatID, userID)
		reply := tgbotapi.NewMessage(chatID, "请提供更精确的描述，然后重新发起求片请求")
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		return
	}

	// 用户确认：进行 Emby 库存查重
	var embyItems []EmbyMediaItem
	embyClient := ch.embyMap[session.ChatID]
	if embyClient != nil {
		items, err := embyClient.SearchMedia(session.TMDBResult.GetDisplayTitle())
		if err != nil {
			// Emby 搜索失败时视为库中无该资源，记录日志后继续流程
			log.Printf("[求片] Emby 搜索失败，视为库中无该资源: %v", err)
		} else {
			embyItems = items
		}
	}

	// 调用库存查重决策
	checkResult := CheckInventory(embyItems, session.IsRemaster)

	// 查重结果为拒绝：通知用户资源已存在，删除会话
	if !checkResult.Allowed {
		reply := tgbotapi.NewMessage(chatID, fmt.Sprintf("该资源已存在，无需重复求片。\n%s", checkResult.Reason))
		reply.ReplyToMessageID = msg.MessageID
		ch.bot.Send(reply)
		rh.deleteSession(chatID, userID)
		return
	}

	// 查重结果为允许：先进行数据库去重检查，再写入数据库
	if rh.store != nil {
		// 数据库去重：检查是否已存在相同用户和 TMDB ID 的活跃请求
		exists, err := rh.store.HasActiveRequest(chatID, userID, session.TMDBResult.ID)
		if err != nil {
			log.Printf("[求片] 数据库去重检查失败: %v", err)
			reply := tgbotapi.NewMessage(chatID, "⚠️ 系统错误，请稍后再试")
			reply.ReplyToMessageID = msg.MessageID
			ch.bot.Send(reply)
			rh.deleteSession(chatID, userID)
			return
		}
		if exists {
			reply := tgbotapi.NewMessage(chatID, "你已提交过该影视的求片请求，请耐心等待处理")
			reply.ReplyToMessageID = msg.MessageID
			ch.bot.Send(reply)
			rh.deleteSession(chatID, userID)
			return
		}

		// 写入数据库，状态为 pending
		record := &RequestRecord{
			ChatID:     chatID,
			UserID:     userID,
			UserName:   session.UserName,
			TMDBID:     session.TMDBResult.ID,
			Title:      session.TMDBResult.GetDisplayTitle(),
			MediaType:  session.TMDBResult.MediaType,
			Year:       session.TMDBResult.GetYear(),
			IsRemaster: session.IsRemaster,
		}
		if err := rh.store.InsertRequest(record); err != nil {
			log.Printf("[求片] 写入数据库失败: %v", err)
			reply := tgbotapi.NewMessage(chatID, "⚠️ 系统错误，请稍后再试")
			reply.ReplyToMessageID = msg.MessageID
			ch.bot.Send(reply)
			rh.deleteSession(chatID, userID)
			return
		}
	}

	// 获取群组名称，格式化消息，转发给管理员
	groupName := ""
	if group := ch.appConfig.GetGroupConfig(chatID); group != nil && group.ServerName != "" {
		groupName = group.ServerName
	} else if msg.Chat.Title != "" {
		groupName = msg.Chat.Title
	}

	adminMsg := FormatAdminMessage(session, groupName, checkResult.ExistingItems)
	keyboard := BuildApprovalKeyboard(session)

	// 遍历管理员列表，向每个管理员私聊发送转发消息
	// 跟踪转发成功数，用于判断是否所有管理员都无法收到通知
	forwardSuccess := 0
	for _, adminID := range ch.appConfig.Global.BotAdmins {
		forwardMsg := tgbotapi.NewMessage(adminID, adminMsg)
		forwardMsg.ReplyMarkup = keyboard
		if _, err := ch.bot.Send(forwardMsg); err != nil {
			log.Printf("[求片] 向管理员 %d 转发求片请求失败: %v", adminID, err)
		} else {
			forwardSuccess++
		}
	}

	// 根据转发结果在群聊中给出不同反馈
	var replyText string
	if forwardSuccess == 0 && len(ch.appConfig.Global.BotAdmins) > 0 {
		// 所有管理员转发均失败，告知用户管理员暂时无法收到通知
		replyText = "请求已提交，但管理员暂时无法收到通知，请稍后联系管理员确认"
	} else {
		replyText = "请求已提交，等待管理员处理"
	}
	reply := tgbotapi.NewMessage(chatID, replyText)
	reply.ReplyToMessageID = msg.MessageID
	ch.bot.Send(reply)

	// 清理会话
	rh.deleteSession(chatID, userID)
}

// HandleCallbackQuery 处理管理员点击 Inline Keyboard 按钮的回调
// 流程：解析回调数据 → 在原群聊通知请求者 → 更新管理员消息 → 回复 CallbackQuery
func (rh *RequestHandler) HandleCallbackQuery(ch *ChatHandler, query *tgbotapi.CallbackQuery) {
	// 解析回调数据
	cbData, err := ParseCallbackData(query.Data)
	if err != nil {
		log.Printf("[求片] 解析回调数据失败: %v", err)
		callback := tgbotapi.NewCallback(query.ID, "数据解析失败")
		ch.bot.Request(callback)
		return
	}

	// 根据 action 类型构建通知消息，使用 Markdown 链接 @ 请求者
	userLink := fmt.Sprintf("[用户](tg://user?id=%d)", cbData.UserID)
	var notifyText string
	var statusLabel string

	switch cbData.Action {
	case "approve":
		notifyText = fmt.Sprintf("%s 你的求片请求已通过，管理员正在处理中", userLink)
		statusLabel = "🎬 求片请求（已通过 ✅）"
	case "reject":
		notifyText = fmt.Sprintf("%s 你的求片请求已被拒绝", userLink)
		statusLabel = "🎬 求片请求（已拒绝 ❌）"
	}

	// 在原求片群聊中发送通知消息
	notifyMsg := tgbotapi.NewMessage(cbData.ChatID, notifyText)
	notifyMsg.ParseMode = "Markdown"
	if _, err := ch.bot.Send(notifyMsg); err != nil {
		log.Printf("[求片] 在群聊 %d 中发送通知失败: %v", cbData.ChatID, err)
	}

	// 更新数据库中对应记录的状态
	if rh.store != nil {
		record, err := rh.store.FindPendingRequest(cbData.ChatID, cbData.UserID, cbData.TMDBID)
		if err != nil {
			log.Printf("[求片] 查找数据库记录失败: %v", err)
		} else if record != nil {
			var dbStatus string
			switch cbData.Action {
			case "approve":
				dbStatus = "approved"
			case "reject":
				dbStatus = "rejected"
			}
			if err := rh.store.UpdateStatus(record.ID, dbStatus); err != nil {
				log.Printf("[求片] 更新数据库状态失败: %v", err)
			}
		} else {
			log.Printf("[求片] 未找到对应的 pending 记录: chatID=%d, userID=%d, tmdbID=%d", cbData.ChatID, cbData.UserID, cbData.TMDBID)
		}
	}

	// 更新管理员私聊中的消息：替换标题为处理结果标记，并移除按钮
	if query.Message != nil {
		adminChatID := query.Message.Chat.ID
		adminMsgID := query.Message.MessageID
		originalText := query.Message.Text

		// 将原消息开头的 "🎬 新求片请求" 替换为处理结果标记
		newText := strings.Replace(originalText, "🎬 新求片请求", statusLabel, 1)
		// 若原文本中没有预期的标题前缀，则直接在前面加上标记
		if newText == originalText {
			newText = statusLabel + "\n" + originalText
		}

		// 编辑消息文本
		editMsg := tgbotapi.NewEditMessageText(adminChatID, adminMsgID, newText)
		if _, err := ch.bot.Request(editMsg); err != nil {
			log.Printf("[求片] 更新管理员消息文本失败: %v", err)
		}

		// 移除 Inline Keyboard 按钮
		emptyMarkup := tgbotapi.NewEditMessageReplyMarkup(adminChatID, adminMsgID, tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
		})
		if _, err := ch.bot.Request(emptyMarkup); err != nil {
			log.Printf("[求片] 移除管理员消息按钮失败: %v", err)
		}
	}

	// 回复 CallbackQuery，消除按钮上的加载状态
	callback := tgbotapi.NewCallback(query.ID, "已处理")
	ch.bot.Request(callback)
}

// deleteSession 删除指定用户的求片会话
func (rh *RequestHandler) deleteSession(chatID, userID int64) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	delete(rh.sessions, sessionKey(chatID, userID))
}
