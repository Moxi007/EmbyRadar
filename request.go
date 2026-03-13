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
	UserID      int64              // 请求者 Telegram 用户 ID
	UserName    string             // 请求者用户名
	ChatID      int64              // 所在群聊 ID
	TMDBResults []TMDBSearchResult // TMDB 搜索候选列表，用户通过按钮选择
	TMDBResult  *TMDBSearchResult  // 用户最终选定的 TMDB 条目
	IsRemaster  bool               // 是否为洗版请求
	State       RequestState       // 当前会话状态
	CreatedAt   time.Time          // 会话创建时间
	ExpiresAt   time.Time          // 会话超时时间（5分钟）
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

// selectCallbackPrefix 用户选择 TMDB 搜索结果的回调前缀
const selectCallbackPrefix = "reqsel"

// aiConfirmCallbackPrefix AI 识别求片意图后的确认按钮回调前缀
const aiConfirmCallbackPrefix = "reqai"

// SelectCallbackData 用户选择 TMDB 搜索结果的回调数据
type SelectCallbackData struct {
	ChatID int64 // 群聊 ID
	UserID int64 // 请求者用户 ID
	Index  int   // 选中的候选条目索引（0-based）
}

// FormatSelectCallbackData 编码用户选择回调数据
// 格式：reqsel:{chatID}:{userID}:{index}
func FormatSelectCallbackData(data *SelectCallbackData) string {
	return fmt.Sprintf("%s:%d:%d:%d", selectCallbackPrefix, data.ChatID, data.UserID, data.Index)
}

// ParseSelectCallbackData 解析用户选择回调数据
func ParseSelectCallbackData(data string) (*SelectCallbackData, error) {
	parts := strings.Split(data, ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("选择回调数据格式错误，期望 4 段，实际 %d 段", len(parts))
	}
	if parts[0] != selectCallbackPrefix {
		return nil, fmt.Errorf("选择回调前缀错误：%s", parts[0])
	}
	chatID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("解析 ChatID 失败：%w", err)
	}
	userID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("解析 UserID 失败：%w", err)
	}
	index, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil, fmt.Errorf("解析索引失败：%w", err)
	}
	return &SelectCallbackData{ChatID: chatID, UserID: userID, Index: index}, nil
}

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
	sb.WriteString(fmt.Sprintf("📌 影片：[%s](%s)\n", cleanMarkdownName(titleLine), result.GetTMDBURL()))
	sb.WriteString(fmt.Sprintf("📅 年份：%s\n", result.GetYear()))
	sb.WriteString(fmt.Sprintf("🎭 类型：%s\n", mediaTypeStr))
	sb.WriteString(fmt.Sprintf("👤 请求者：[%s](tg://user?id=%d)\n", cleanMarkdownName(session.UserName), session.UserID))

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

	// 构建群组链接：将负数 chat_id 转换为 Telegram 群组链接格式
	groupLink := cleanMarkdownName(groupName)
	chatIDStr := fmt.Sprintf("%d", session.ChatID)
	if strings.HasPrefix(chatIDStr, "-100") {
		groupLink = fmt.Sprintf("[%s](https://t.me/c/%s/1)", cleanMarkdownName(groupName), chatIDStr[4:])
	}
	sb.WriteString(fmt.Sprintf("\n来自群组：%s", groupLink))

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

	// 第五步：构建 Inline Keyboard 按钮列表，展示搜索结果供用户选择
	userName := msg.From.FirstName
	if msg.From.LastName != "" {
		userName += " " + msg.From.LastName
	}

	// 构建候选列表文本和选择按钮
	var sb strings.Builder
	sb.WriteString("🔍 找到以下结果，请点击选择：\n\n")
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, r := range results {
		mediaIcon := "🎬"
		if r.MediaType == "tv" {
			mediaIcon = "📺"
		}
		year := r.GetYear()
		title := r.GetDisplayTitle()
		sb.WriteString(fmt.Sprintf("%s %d. %s（%s）\n", mediaIcon, i+1, title, year))

		// 按钮文本：序号 + 标题 + 年份，回调数据编码索引
		btnText := fmt.Sprintf("%d. %s（%s）", i+1, title, year)
		// Telegram 按钮文本有长度限制，截断过长的标题
		if len([]rune(btnText)) > 40 {
			btnText = string([]rune(btnText)[:37]) + "..."
		}
		cbData := FormatSelectCallbackData(&SelectCallbackData{
			ChatID: chatID,
			UserID: userID,
			Index:  i,
		})
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(btnText, cbData),
		))
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	reply := tgbotapi.NewMessage(chatID, sb.String())
	reply.ReplyToMessageID = msg.MessageID
	reply.ReplyMarkup = keyboard
	ch.bot.Send(reply)

	// 第六步：创建 StateWaitConfirm 状态的会话，存储所有候选结果
	rh.mu.Lock()
	rh.sessions[sessionKey(chatID, userID)] = &RequestSession{
		UserID:      userID,
		UserName:    userName,
		ChatID:      chatID,
		TMDBResults: results,
		IsRemaster:  intent.IsRemaster,
		State:       StateWaitConfirm,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(sessionTimeout),
	}
	rh.mu.Unlock()
}

// HandleSelectCallback 处理用户点击 TMDB 搜索结果按钮的回调
// 流程：验证会话 → 设置选中条目 → 库存查重 → 数据库去重 → 转发管理员
func (rh *RequestHandler) HandleSelectCallback(ch *ChatHandler, query *tgbotapi.CallbackQuery) {
	cbData, err := ParseSelectCallbackData(query.Data)
	if err != nil {
		log.Printf("[求片] 解析选择回调数据失败: %v", err)
		callback := tgbotapi.NewCallback(query.ID, "数据解析失败")
		ch.bot.Request(callback)
		return
	}

	// 验证回调来源：只有发起求片的用户本人才能选择
	if query.From.ID != cbData.UserID {
		callback := tgbotapi.NewCallback(query.ID, "只有发起求片的用户才能选择")
		ch.bot.Request(callback)
		return
	}

	// 获取用户的活跃会话
	session := rh.GetSession(cbData.ChatID, cbData.UserID)
	if session == nil || session.State != StateWaitConfirm {
		callback := tgbotapi.NewCallback(query.ID, "会话已过期，请重新发起求片")
		ch.bot.Request(callback)
		return
	}

	// 验证索引有效性
	if cbData.Index < 0 || cbData.Index >= len(session.TMDBResults) {
		callback := tgbotapi.NewCallback(query.ID, "无效的选择")
		ch.bot.Request(callback)
		return
	}

	// 设置用户选中的条目
	selected := &session.TMDBResults[cbData.Index]
	session.TMDBResult = selected

	// 回复 CallbackQuery，消除按钮加载状态
	callback := tgbotapi.NewCallback(query.ID, fmt.Sprintf("已选择：%s", selected.GetDisplayTitle()))
	ch.bot.Request(callback)

	// 更新原消息：替换按钮列表为选中结果（Markdown 超链接），移除按钮
	if query.Message != nil {
		mediaIcon := "🎬"
		if selected.MediaType == "tv" {
			mediaIcon = "📺"
		}
		editText := fmt.Sprintf("%s 已选择：[%s（%s）](%s)",
			mediaIcon, cleanMarkdownName(selected.GetDisplayTitle()), selected.GetYear(), selected.GetTMDBURL())
		editMsg := tgbotapi.NewEditMessageText(cbData.ChatID, query.Message.MessageID, editText)
		editMsg.ParseMode = "Markdown"
		emptyMarkup := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		editMsg.ReplyMarkup = &emptyMarkup
		if _, err := ch.bot.Request(editMsg); err != nil {
			log.Printf("[求片] 更新选择确认消息失败: %v", err)
		}
	}

	// 进行 Emby 库存查重
	var embyItems []EmbyMediaItem
	embyClient := ch.embyMap[session.ChatID]
	if embyClient != nil {
		items, err := embyClient.SearchMedia(selected.GetDisplayTitle())
		if err != nil {
			log.Printf("[求片] Emby 搜索失败，视为库中无该资源: %v", err)
		} else {
			embyItems = items
		}
	}

	checkResult := CheckInventory(embyItems, session.IsRemaster)

	// 查重拒绝：通知用户资源已存在
	if !checkResult.Allowed {
		reply := tgbotapi.NewMessage(cbData.ChatID, fmt.Sprintf("该资源已存在，无需重复求片。\n%s", checkResult.Reason))
		ch.bot.Send(reply)
		rh.deleteSession(cbData.ChatID, cbData.UserID)
		return
	}

	// 数据库去重和写入
	var record *RequestRecord
	if rh.store != nil {
		exists, err := rh.store.HasActiveRequest(cbData.ChatID, cbData.UserID, selected.ID)
		if err != nil {
			log.Printf("[求片] 数据库去重检查失败: %v", err)
			reply := tgbotapi.NewMessage(cbData.ChatID, "⚠️ 系统错误，请稍后再试")
			ch.bot.Send(reply)
			rh.deleteSession(cbData.ChatID, cbData.UserID)
			return
		}
		if exists {
			reply := tgbotapi.NewMessage(cbData.ChatID, "你已提交过该影视的求片请求，请耐心等待处理")
			ch.bot.Send(reply)
			rh.deleteSession(cbData.ChatID, cbData.UserID)
			return
		}

		record = &RequestRecord{
			ChatID:     cbData.ChatID,
			UserID:     cbData.UserID,
			UserName:   session.UserName,
			TMDBID:     selected.ID,
			Title:      selected.GetDisplayTitle(),
			MediaType:  selected.MediaType,
			Year:       selected.GetYear(),
			IsRemaster: session.IsRemaster,
		}
		if err := rh.store.InsertRequest(record); err != nil {
			log.Printf("[求片] 写入数据库失败: %v", err)
			reply := tgbotapi.NewMessage(cbData.ChatID, "⚠️ 系统错误，请稍后再试")
			ch.bot.Send(reply)
			rh.deleteSession(cbData.ChatID, cbData.UserID)
			return
		}
	}

	// 获取群组名称，转发给管理员
	groupName := ""
	if group := ch.appConfig.GetGroupConfig(cbData.ChatID); group != nil && group.ServerName != "" {
		groupName = group.ServerName
	} else if query.Message != nil && query.Message.Chat.Title != "" {
		groupName = query.Message.Chat.Title
	}

	adminMsg := FormatAdminMessage(session, groupName, checkResult.ExistingItems)
	keyboard := BuildApprovalKeyboard(session)

	log.Printf("[求片] 准备转发管理员，管理员列表: %v，影片: %s", ch.appConfig.Global.BotAdmins, selected.GetDisplayTitle())

	forwardSuccess := 0
	for _, adminID := range ch.appConfig.Global.BotAdmins {
		forwardMsg := tgbotapi.NewMessage(adminID, adminMsg)
		forwardMsg.ParseMode = "Markdown"
		forwardMsg.ReplyMarkup = keyboard
		if sentMsg, err := ch.bot.Send(forwardMsg); err != nil {
			log.Printf("[求片] 向管理员 %d 转发求片请求失败: %v", adminID, err)
		} else {
			forwardSuccess++
			// 记录管理员消息 ID，用于审批后同步更新所有管理员的消息
			if rh.store != nil && record != nil {
				if err := rh.store.SaveAdminMessage(record.ID, adminID, sentMsg.MessageID); err != nil {
					log.Printf("[求片] 保存管理员 %d 消息记录失败: %v", adminID, err)
				}
			}
		}
	}

	var replyText string
	if forwardSuccess == 0 && len(ch.appConfig.Global.BotAdmins) > 0 {
		replyText = "请求已提交，但管理员暂时无法收到通知，请稍后联系管理员确认"
	} else {
		replyText = "请求已提交，等待管理员处理"
	}
	reply := tgbotapi.NewMessage(cbData.ChatID, replyText)
	ch.bot.Send(reply)

	rh.deleteSession(cbData.ChatID, cbData.UserID)
}

// HandleAIConfirmCallback 处理用户点击 AI 回复中"确认求片"按钮的回调
// 流程：解析回调数据 → 触发 TMDB 搜索 → 展示搜索结果按钮供用户选择
func (rh *RequestHandler) HandleAIConfirmCallback(ch *ChatHandler, query *tgbotapi.CallbackQuery) {
	// 解析回调数据，格式：reqai:{chatID}:{userID}:{movieName}
	parts := strings.SplitN(query.Data, ":", 4)
	if len(parts) != 4 {
		callback := tgbotapi.NewCallback(query.ID, "数据解析失败")
		ch.bot.Request(callback)
		return
	}

	chatID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		callback := tgbotapi.NewCallback(query.ID, "数据解析失败")
		ch.bot.Request(callback)
		return
	}
	userID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		callback := tgbotapi.NewCallback(query.ID, "数据解析失败")
		ch.bot.Request(callback)
		return
	}
	movieName := parts[3]

	// 验证回调来源：只有发起者本人才能确认
	if query.From.ID != userID {
		callback := tgbotapi.NewCallback(query.ID, "只有发起者才能确认求片")
		ch.bot.Request(callback)
		return
	}

	// 回复 CallbackQuery
	callback := tgbotapi.NewCallback(query.ID, "正在搜索...")
	ch.bot.Request(callback)

	// 更新原消息：移除确认按钮，标记已确认
	if query.Message != nil {
		editText := query.Message.Text + "\n\n✅ 已确认，正在搜索..."
		editMsg := tgbotapi.NewEditMessageText(chatID, query.Message.MessageID, editText)
		emptyMarkup := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		editMsg.ReplyMarkup = &emptyMarkup
		ch.bot.Request(editMsg)
	}

	// 获取 TMDB 客户端
	tmdbClient := ch.tmdbMap[chatID]
	if tmdbClient == nil {
		reply := tgbotapi.NewMessage(chatID, "⚠️ 当前群组未配置 TMDB，无法搜索影视信息")
		ch.bot.Send(reply)
		return
	}

	// 调用 AI 分析影视名称和类型
	intentMessages := []ChatMessage{
		{
			Role: "system",
			Content: MessageContent{Text: "你是一个影视信息提取助手。请从用户的文本中提取以下信息并以 JSON 格式返回：\n" +
				"1. name: 影视作品名称\n" +
				"2. type: 类型，\"movie\"（电影）或 \"tv\"（电视剧），无法判断时留空\n" +
				"3. is_remaster: 是否有洗版意图（用户想要更高清版本），布尔值\n\n" +
				"只返回 JSON，不要包含其他文字。示例：{\"name\":\"流浪地球2\",\"type\":\"movie\",\"is_remaster\":false}"},
		},
		{
			Role:    "user",
			Content: MessageContent{Text: movieName},
		},
	}

	var mediaType string
	var isRemaster bool

	aiResp, err := ch.aiClient.ChatCompletion(intentMessages, nil)
	if err != nil {
		log.Printf("[求片] AI 意图分析失败: %v，使用原始片名搜索", err)
	} else {
		respText := strings.TrimSpace(aiResp.Content.Text)
		respText = strings.TrimPrefix(respText, "```json")
		respText = strings.TrimPrefix(respText, "```")
		respText = strings.TrimSuffix(respText, "```")
		respText = strings.TrimSpace(respText)

		var intent aiIntentResult
		if err := json.Unmarshal([]byte(respText), &intent); err == nil {
			if intent.Name != "" {
				movieName = intent.Name
			}
			mediaType = intent.Type
			isRemaster = intent.IsRemaster
		}
	}

	// 搜索 TMDB
	results, err := tmdbClient.SearchMulti(movieName, mediaType)
	if err != nil {
		log.Printf("[求片] TMDB 搜索失败: %v", err)
		reply := tgbotapi.NewMessage(chatID, "⚠️ 搜索影视信息时出错，请稍后再试")
		ch.bot.Send(reply)
		return
	}

	if len(results) == 0 {
		reply := tgbotapi.NewMessage(chatID, "未找到相关影视，请尝试更精确的片名")
		ch.bot.Send(reply)
		return
	}

	// 构建候选列表和选择按钮
	userName := query.From.FirstName
	if query.From.LastName != "" {
		userName += " " + query.From.LastName
	}

	var sb strings.Builder
	sb.WriteString("🔍 找到以下结果，请点击选择：\n\n")
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, r := range results {
		mediaIcon := "🎬"
		if r.MediaType == "tv" {
			mediaIcon = "📺"
		}
		year := r.GetYear()
		title := r.GetDisplayTitle()
		sb.WriteString(fmt.Sprintf("%s %d. %s（%s）\n", mediaIcon, i+1, title, year))

		btnText := fmt.Sprintf("%d. %s（%s）", i+1, title, year)
		if len([]rune(btnText)) > 40 {
			btnText = string([]rune(btnText)[:37]) + "..."
		}
		selCbData := FormatSelectCallbackData(&SelectCallbackData{
			ChatID: chatID,
			UserID: userID,
			Index:  i,
		})
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(btnText, selCbData),
		))
	}

	selectKeyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	selectMsg := tgbotapi.NewMessage(chatID, sb.String())
	selectMsg.ReplyMarkup = selectKeyboard
	ch.bot.Send(selectMsg)

	// 创建会话，存储候选结果
	rh.mu.Lock()
	rh.sessions[sessionKey(chatID, userID)] = &RequestSession{
		UserID:      userID,
		UserName:    userName,
		ChatID:      chatID,
		TMDBResults: results,
		IsRemaster:  isRemaster,
		State:       StateWaitConfirm,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(sessionTimeout),
	}
	rh.mu.Unlock()
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
	// 先从数据库查找用户名，用于构建更友好的链接文本
	userName := "用户"
	var dbRecord *RequestRecord
	if rh.store != nil {
		record, err := rh.store.FindPendingRequest(cbData.ChatID, cbData.UserID, cbData.TMDBID)
		if err != nil {
			log.Printf("[求片] 查找数据库记录失败: %v", err)
		} else if record != nil {
			dbRecord = record
			if record.UserName != "" {
				userName = record.UserName
			}
		} else {
			log.Printf("[求片] 未找到对应的 pending 记录: chatID=%d, userID=%d, tmdbID=%d", cbData.ChatID, cbData.UserID, cbData.TMDBID)
		}
	}

	userLink := fmt.Sprintf("[%s](tg://user?id=%d)", cleanMarkdownName(userName), cbData.UserID)
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
	if dbRecord != nil {
		var dbStatus string
		switch cbData.Action {
		case "approve":
			dbStatus = "approved"
		case "reject":
			dbStatus = "rejected"
		}
		if err := rh.store.UpdateStatus(dbRecord.ID, dbStatus); err != nil {
			log.Printf("[求片] 更新数据库状态失败: %v", err)
		}
	}

	// 更新所有管理员私聊中的消息：替换标题为处理结果标记，并移除按钮
	// 通过数据库查询所有管理员的消息 ID，确保每个管理员的消息都被更新
	if dbRecord != nil && rh.store != nil {
		adminMsgs, err := rh.store.GetAdminMessages(dbRecord.ID)
		if err != nil {
			log.Printf("[求片] 查询管理员消息记录失败: %v", err)
		}
		for _, am := range adminMsgs {
			// 对于当前点击的管理员，使用原消息文本来构建新文本
			// 对于其他管理员，也使用相同的状态标记
			var newText string
			if query.Message != nil && am.AdminID == query.Message.Chat.ID && am.MessageID == query.Message.MessageID {
				originalText := query.Message.Text
				newText = strings.Replace(originalText, "🎬 新求片请求", statusLabel, 1)
				if newText == originalText {
					newText = statusLabel + "\n" + originalText
				}
			} else {
				// 其他管理员的消息无法获取原文，直接用状态标记
				newText = statusLabel
			}

			editMsg := tgbotapi.NewEditMessageText(am.AdminID, am.MessageID, newText)
			emptyMarkup := tgbotapi.InlineKeyboardMarkup{
				InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
			}
			editMsg.ReplyMarkup = &emptyMarkup
			if _, err := ch.bot.Request(editMsg); err != nil {
				log.Printf("[求片] 更新管理员 %d 消息 %d 失败: %v", am.AdminID, am.MessageID, err)
			}
		}
	} else if query.Message != nil {
		// 回退逻辑：数据库不可用时只更新当前管理员的消息
		adminChatID := query.Message.Chat.ID
		adminMsgID := query.Message.MessageID
		originalText := query.Message.Text

		newText := strings.Replace(originalText, "🎬 新求片请求", statusLabel, 1)
		if newText == originalText {
			newText = statusLabel + "\n" + originalText
		}

		editMsg := tgbotapi.NewEditMessageText(adminChatID, adminMsgID, newText)
		emptyMarkup := tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
		}
		editMsg.ReplyMarkup = &emptyMarkup
		if _, err := ch.bot.Request(editMsg); err != nil {
			log.Printf("[求片] 更新管理员消息失败: %v", err)
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
