package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ToolContext 工具执行上下文，携带当前请求的所有相关信息
type ToolContext struct {
	ChatID    int64
	SenderID  int64
	Msg       *tgbotapi.Message
	Group     *GroupConfig
	AppConfig *AppConfig
	IsPrivate bool

	// 外部服务引用
	EmbyClient  *EmbyClient
	EBClient    *EmbyBossClient
	TMDBClient  *TMDBClient
	AIClient    *AIClient
	ChatHandler *ChatHandler // 用于图片生成等需要发送消息的场景

	// 权限标记
	IsSensitiveAllowed    bool // 是否允许查看敏感信息（管理员）
	AllowSensitiveDetails bool // 是否允许查看 IP/设备明细（管理员 + 私聊）
}

// ToolHandler 工具处理器接口
type ToolHandler interface {
	// Definition 返回工具的 OpenAI Function Calling 定义
	Definition(ctx *ToolContext) Tool
	// Execute 执行工具调用，返回文本结果
	Execute(args map[string]any, ctx *ToolContext) string
	// Enabled 在当前上下文中是否启用此工具
	Enabled(ctx *ToolContext) bool
	// Name 工具名称（用于路由）
	Name() string
}

// ToolRegistry 工具注册中心，统一管理所有 AI 可调用的工具
type ToolRegistry struct {
	handlers map[string]ToolHandler
	order    []string // 保持注册顺序
}

// NewToolRegistry 创建工具注册中心
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		handlers: make(map[string]ToolHandler),
	}
}

// Register 注册一个工具处理器
func (r *ToolRegistry) Register(handler ToolHandler) {
	name := handler.Name()
	r.handlers[name] = handler
	r.order = append(r.order, name)
}

// GetEnabledTools 获取当前上下文可用的工具定义列表
func (r *ToolRegistry) GetEnabledTools(ctx *ToolContext) []Tool {
	var tools []Tool
	for _, name := range r.order {
		handler := r.handlers[name]
		if handler.Enabled(ctx) {
			tools = append(tools, handler.Definition(ctx))
		}
	}
	return tools
}

// Execute 执行指定工具，返回执行结果文本
func (r *ToolRegistry) Execute(name string, argsJSON string, ctx *ToolContext) string {
	handler, ok := r.handlers[name]
	if !ok {
		return fmt.Sprintf("未知的工具: %s", name)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		log.Printf("[工具] 解析 %s 参数失败: %v", name, err)
		return fmt.Sprintf("参数解析失败: %v", err)
	}

	return handler.Execute(args, ctx)
}

// HasHandler 检查指定名称的工具是否已注册
func (r *ToolRegistry) HasHandler(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// ========== 以下为具体的工具 Handler 实现 ==========

// --- search_web ---
type SearchWebHandler struct{}

func (h *SearchWebHandler) Name() string { return "search_web" }
func (h *SearchWebHandler) Enabled(ctx *ToolContext) bool {
	return ctx.Group != nil && ctx.Group.AISearchEnabled
}
func (h *SearchWebHandler) Definition(ctx *ToolContext) Tool {
	// Gemini 的 Google Search 原生模式由调用层在外部特殊处理，
	// 这里只返回 DuckDuckGo Function Calling 定义
	currentDateStr := time.Now().Format("2006年01月")
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "search_web",
			Description: fmt.Sprintf("必须使用此工具来获取最新的资讯和新闻。当前时间是 %s，你的搜索关键词中必须主动携带 '%s' 或者具体日期作为检索词，否则你会搜到过时的旧新闻！", currentDateStr, currentDateStr),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": fmt.Sprintf("进行搜索引擎查询的关键词。务必包含时间如 '%s' 以保证时效性。", currentDateStr),
					},
				},
				"required": []string{"query"},
			},
		},
	}
}
func (h *SearchWebHandler) Execute(args map[string]any, ctx *ToolContext) string {
	query, _ := args["query"].(string)
	if query == "" {
		return "缺少 query 参数"
	}
	log.Printf("[工具] 【触发网络搜索】关键词: %s", query)
	return SearchWeb(query)
}

// --- search_tmdb ---
type SearchTMDBHandler struct{}

func (h *SearchTMDBHandler) Name() string { return "search_tmdb" }
func (h *SearchTMDBHandler) Enabled(ctx *ToolContext) bool {
	return ctx.TMDBClient != nil
}
func (h *SearchTMDBHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name: "search_tmdb",
			Description: "搜索 TMDB（The Movie Database）获取电影或电视剧的详细信息，包括简介、上映日期等。当用户询问影视相关问题时使用此工具。" +
				"⚠️在回复中引用影视信息时，你必须原样使用搜索结果中提供的完整 TMDB 链接（格式为 https://www.themoviedb.org/...），绝对不能自行拼凑或修改链接中的任何部分。" +
				"⚠️关于评分和评价：给用户展示评分时，必须【只提供豆瓣评分】（通过 search_web 工具搜索获取），只有在确实找不到豆瓣评分的情况下，才允许给出 TMDB 或 IMDb 等其他评分。评价一部影片时，绝对不能仅仅根据 TMDB 简介进行评价，你必须通过 search_web 工具搜索网上的真实影评和口碑，综合真实观众的评价来给出结论。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "搜索关键词（电影或电视剧名称）",
					},
					"media_type": map[string]any{
						"type":        "string",
						"enum":        []string{"movie", "tv", ""},
						"description": "媒体类型：movie（电影）、tv（电视剧），留空则搜索全部",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}
func (h *SearchTMDBHandler) Execute(args map[string]any, ctx *ToolContext) string {
	query, _ := args["query"].(string)
	mediaType, _ := args["media_type"].(string)
	log.Printf("[工具] 【触发 TMDB 搜索】关键词: %s, 类型: %s", query, mediaType)

	if ctx.TMDBClient == nil {
		return "TMDB 功能未配置，无法搜索影视信息。"
	}

	results, err := ctx.TMDBClient.SearchMulti(query, mediaType)
	if err != nil {
		log.Printf("[工具] TMDB 搜索失败: %v", err)
		return fmt.Sprintf("TMDB 搜索失败: %v", err)
	}
	return FormatTMDBResultsForAI(results)
}

// --- generate_image ---
type GenerateImageHandler struct{}

func (h *GenerateImageHandler) Name() string { return "generate_image" }
func (h *GenerateImageHandler) Enabled(ctx *ToolContext) bool {
	return ctx.Group != nil && ctx.Group.AIImageEnabled && ctx.Group.AIImageModel != ""
}
func (h *GenerateImageHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "generate_image",
			Description: "根据用户的文字描述生成图片。当用户要求画图、生成图片、创作图像时使用此工具。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "图片描述文本，详细描述要生成的图片内容",
					},
				},
				"required": []string{"prompt"},
			},
		},
	}
}
func (h *GenerateImageHandler) Execute(args map[string]any, ctx *ToolContext) string {
	prompt, _ := args["prompt"].(string)
	if prompt == "" {
		return "缺少 prompt 参数"
	}
	log.Printf("[工具] 【触发图片生成】描述: %s", prompt)

	if ctx.ChatHandler == nil || ctx.Msg == nil {
		return "图片生成功能不可用（内部错误）"
	}

	err := ctx.ChatHandler.handleImageGeneration(ctx.Msg, prompt, ctx.Group)
	if err != nil {
		return fmt.Sprintf("图片生成失败: %v", err)
	}
	return "图片已成功生成并发送"
}

// --- search_emby_library ---
type SearchEmbyLibraryHandler struct{}

func (h *SearchEmbyLibraryHandler) Name() string { return "search_emby_library" }
func (h *SearchEmbyLibraryHandler) Enabled(ctx *ToolContext) bool {
	return ctx.EmbyClient != nil
}
func (h *SearchEmbyLibraryHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "search_emby_library",
			Description: "在私有 Emby 影音库中搜索是否拥有某部完整的电影或电视剧源文件。当用户问库里有没有什么片子时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "搜索关键词，例如电影名或导演名",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}
func (h *SearchEmbyLibraryHandler) Execute(args map[string]any, ctx *ToolContext) string {
	query, _ := args["query"].(string)
	log.Printf("[工具] 【触发片库搜索】关键词: %s", query)

	if ctx.EmbyClient == nil {
		return "未配置 EmbyClient，无法查询片库。"
	}

	items, err := ctx.EmbyClient.SearchMedia(query)
	if err != nil {
		return fmt.Sprintf("搜索失败: %v", err)
	}
	if len(items) == 0 {
		return "库中未找到相关资源。"
	}

	var sb strings.Builder
	for i, item := range items {
		if i >= 10 {
			break
		}
		sb.WriteString(fmt.Sprintf("- %s\n", item.FormatMediaInfo()))
	}
	return sb.String()
}

// --- get_emby_latest_added ---
type GetEmbyLatestHandler struct{}

func (h *GetEmbyLatestHandler) Name() string { return "get_emby_latest_added" }
func (h *GetEmbyLatestHandler) Enabled(ctx *ToolContext) bool {
	return ctx.EmbyClient != nil
}
func (h *GetEmbyLatestHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "get_emby_latest_added",
			Description: "查询私有 Emby 影音库最近上传入库的电影或电视剧列表。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "需要查询的最近更新数量，建议 5 到 10",
					},
				},
			},
		},
	}
}
func (h *GetEmbyLatestHandler) Execute(args map[string]any, ctx *ToolContext) string {
	limitF, ok := args["limit"].(float64)
	limit := 10
	if ok && limitF > 0 {
		limit = int(limitF)
	}
	log.Printf("[工具] 【触发最新入库】Limit: %d", limit)

	if ctx.EmbyClient == nil {
		return "未配置 EmbyClient，无法查询。"
	}

	items, err := ctx.EmbyClient.GetLatestMedia(limit)
	if err != nil {
		return fmt.Sprintf("查询最新入库失败: %v", err)
	}
	if len(items) == 0 {
		return "暂无最新入库记录。"
	}

	var sb strings.Builder
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("- %s\n", item.FormatMediaInfo()))
	}
	return sb.String()
}

// --- get_user_playback_stats ---
type GetUserPlaybackStatsHandler struct{}

func (h *GetUserPlaybackStatsHandler) Name() string { return "get_user_playback_stats" }
func (h *GetUserPlaybackStatsHandler) Enabled(ctx *ToolContext) bool {
	return ctx.EmbyClient != nil
}
func (h *GetUserPlaybackStatsHandler) Definition(ctx *ToolContext) Tool {
	playbackDesc := "全能管家探针：通过底层系统查询特定用户在指定天数内的所有综合观影记录（包含：观看了哪些剧集、耗时多久、使用了几个独立的公网 IP、用了几台设备等）。当提问涉及到查水表、防分享、借号抓内鬼、或者单纯询问\u201c某某看了什么好东西\u201d时必须调用。"
	required := []string{"target_tg_id"}
	if !ctx.IsSensitiveAllowed {
		playbackDesc += " 非管理员仅允许查询自己的观影记录，且不会返回任何 IP 或设备信息。"
		required = []string{}
	} else if !ctx.AllowSensitiveDetails {
		playbackDesc += " 群聊只返回汇总信息，不会输出具体 IP 或设备明细。"
	}

	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "get_user_playback_stats",
			Description: playbackDesc,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_tg_id": map[string]any{
						"type":        "integer",
						"description": "需要重点关照/查询的目标用户的 Telegram ID (如果没指定别人，默认就是当前跟你对话的这个人的 ID)",
					},
					"days": map[string]any{
						"type":        "integer",
						"description": "要查询的历史时间跨度（天），默认 1（代表今天/最近24小时），可以指定长达 7 或者 30。",
					},
				},
				"required": required,
			},
		},
	}
}
func (h *GetUserPlaybackStatsHandler) Execute(args map[string]any, ctx *ToolContext) string {
	// 目标用户是谁？
	requestedTgID := ctx.SenderID
	if val, ok := args["target_tg_id"].(float64); ok && val > 0 {
		requestedTgID = int64(val)
	}
	requestedOther := requestedTgID != ctx.SenderID
	targetTgID := ctx.SenderID
	if ctx.IsSensitiveAllowed {
		targetTgID = requestedTgID
	}

	// 查几天
	days := 1
	if val, ok := args["days"].(float64); ok && val > 0 {
		days = int(val)
	}

	if ctx.EBClient == nil || ctx.EmbyClient == nil {
		return "未配置相关服务接口，功能受限。"
	}

	userInfo, err := ctx.EBClient.GetUserInfo(targetTgID)
	if err != nil || userInfo == nil || userInfo.Data.EmbyID == "" {
		return fmt.Sprintf("无法获取目标TGID=%d 的 Emby 生效账号数据（未绑定或不存在），无法查阅他的播放记录。", targetTgID)
	}

	log.Printf("[工具] 【触发全能观影探针】TGID: %d, EmbyID: %s, 天数: %d", targetTgID, userInfo.Data.EmbyID, days)
	stats, err := ctx.EmbyClient.GetUserPlaybackReportingStats(userInfo.Data.EmbyID, days)
	if err != nil {
		return fmt.Sprintf("查询失败(可能尚未安装 Playback Reporting 插件或数据库异常): %v", err)
	}

	var sb strings.Builder
	if !ctx.IsSensitiveAllowed && requestedOther {
		sb.WriteString("提示：仅允许查询自己的记录，已自动切换为你的账号。\n")
	}
	sb.WriteString(fmt.Sprintf("以下是该用户在最近 %d 天内的全景观影报告：\n", days))
	sb.WriteString(fmt.Sprintf("- 累计观看时长：约 %d 分钟\n", stats.TotalDuration))

	if ctx.IsSensitiveAllowed {
		sb.WriteString(fmt.Sprintf("- 使用独立外网IP数：%d 个\n", stats.UniqueIPs))
		sb.WriteString(fmt.Sprintf("- 使用独立设备数：%d 台\n", stats.UniqueDevices))
		if ctx.AllowSensitiveDetails {
			if stats.UniqueIPs > 0 {
				sb.WriteString(fmt.Sprintf("- IP明细: %s\n", strings.Join(stats.IPList, ", ")))
			}
			if stats.UniqueDevices > 0 {
				sb.WriteString(fmt.Sprintf("- 设备明细: %s\n", strings.Join(stats.DeviceList, ", ")))
			}
		} else {
			sb.WriteString("提示：群聊不输出具体 IP/设备明细，如需查看请私聊机器人。\n")
		}
	}

	if len(stats.WatchedItems) > 0 {
		sb.WriteString("- 观看过的内容清单（已去重）：\n")
		for _, item := range stats.WatchedItems {
			sb.WriteString(fmt.Sprintf("  * %s\n", item))
		}
	} else {
		sb.WriteString("- 观看过的内容清单：无（该用户这几天彻底没看剧，可能确实在摸鱼）\n")
	}

	return sb.String()
}

// --- get_user_info ---
type GetUserInfoHandler struct{}

func (h *GetUserInfoHandler) Name() string { return "get_user_info" }
func (h *GetUserInfoHandler) Enabled(ctx *ToolContext) bool {
	return ctx.EmbyClient != nil
}
func (h *GetUserInfoHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "get_user_info",
			Description: "查询特定用户的 Emby 账号基础信息、VIP 状态等级、积分余额和账号过期时间等。当问及\u201c我的账号\u201d、\u201c我有多少钱\u201d、\u201c他过期了吗\u201d等涉及系统数据时必须调用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_tg_id": map[string]any{
						"type":        "integer",
						"description": "需要查询的目标用户的 Telegram ID (如果没指定别人，默认查当前跟你对话的这个人的 ID)",
					},
				},
				"required": []string{"target_tg_id"},
			},
		},
	}
}
func (h *GetUserInfoHandler) Execute(args map[string]any, ctx *ToolContext) string {
	targetTgID := ctx.SenderID
	if val, ok := args["target_tg_id"].(float64); ok && val > 0 {
		targetTgID = int64(val)
	}

	if ctx.EBClient == nil {
		return "未开启或未配置相关 EmbyBoss 服务接口，功能受限。"
	}

	userInfoResp, err := ctx.EBClient.GetUserInfo(targetTgID)
	if err != nil || userInfoResp == nil || userInfoResp.Data.Tg.String() == "" {
		return fmt.Sprintf("无法获取目标TGID=%d 的 Emby 生效账号数据（未绑定或不存在）。", targetTgID)
	}

	log.Printf("[工具] 【触发私人资产探针】TGID: %d", targetTgID)

	// 获取群组配置中的货币名称
	currencyName := "鸡蛋"
	if ctx.Group != nil && ctx.Group.EmbyBossCurrencyName != "" {
		currencyName = ctx.Group.EmbyBossCurrencyName
	}

	if targetTgID == ctx.SenderID {
		return userInfoResp.FormatForAI(currencyName)
	}

	return fmt.Sprintf("【内部数据查询结果 - 对象TG ID: %d】：账号名「%s」，余额 %d %s，状态「%s」，过期时间戳: %s。货币须称「%s」。",
		targetTgID, userInfoResp.Data.Name, userInfoResp.Data.Iv, currencyName,
		func() string {
			if userInfoResp.Data.Lv == "c" {
				return "封禁状态(被禁止登录)"
			}
			return "正常"
		}(),
		userInfoResp.Data.Ex,
		currencyName)
}

// DefaultToolRegistry 创建包含所有内置工具的注册中心
func DefaultToolRegistry() *ToolRegistry {
	registry := NewToolRegistry()
	registry.Register(&SearchWebHandler{})
	registry.Register(&SearchTMDBHandler{})
	registry.Register(&GenerateImageHandler{})
	registry.Register(&SearchEmbyLibraryHandler{})
	registry.Register(&GetEmbyLatestHandler{})
	registry.Register(&GetUserPlaybackStatsHandler{})
	registry.Register(&GetUserInfoHandler{})
	return registry
}

// --- read_skill ---
// ReadSkillHandler 读取指定技能的完整 SKILL.md 内容（按需加载机制）
type ReadSkillHandler struct {
	loader *SkillsLoader
}

func (h *ReadSkillHandler) Name() string { return "read_skill" }
func (h *ReadSkillHandler) Enabled(ctx *ToolContext) bool {
	return h.loader != nil && len(h.loader.ListSkills()) > 0
}
func (h *ReadSkillHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "read_skill",
			Description: "读取指定技能的完整操作手册。当你决定执行某个技能时，必须先调用此工具获取完整的操作步骤。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"skill_name": map[string]any{
						"type":        "string",
						"description": "技能名称或技能目录名",
					},
				},
				"required": []string{"skill_name"},
			},
		},
	}
}
func (h *ReadSkillHandler) Execute(args map[string]any, ctx *ToolContext) string {
	skillName, _ := args["skill_name"].(string)
	if skillName == "" {
		return "缺少 skill_name 参数"
	}
	log.Printf("[工具] 【读取技能】名称: %s", skillName)

	content, err := h.loader.ReadSkill(skillName)
	if err != nil {
		return fmt.Sprintf("读取技能失败: %v", err)
	}
	return content
}

// --- read_job ---
// ReadJobHandler 读取指定任务的完整 JOB.md 内容
type ReadJobHandler struct {
	loader *JobsLoader
}

func (h *ReadJobHandler) Name() string { return "read_job" }
func (h *ReadJobHandler) Enabled(ctx *ToolContext) bool {
	return h.loader != nil
}
func (h *ReadJobHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "read_job",
			Description: "读取指定任务的完整任务文档，了解任务的详细内容和执行进度。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "任务ID（即 config/jobs/ 下的目录名）",
					},
				},
				"required": []string{"job_id"},
			},
		},
	}
}
func (h *ReadJobHandler) Execute(args map[string]any, ctx *ToolContext) string {
	jobID, _ := args["job_id"].(string)
	if jobID == "" {
		return "缺少 job_id 参数"
	}
	log.Printf("[工具] 【读取任务】ID: %s", jobID)

	content, err := h.loader.ReadJob(jobID)
	if err != nil {
		return fmt.Sprintf("读取任务失败: %v", err)
	}
	return content
}

// --- write_job ---
// WriteJobHandler 写入/更新任务文档（仅管理员可用）
type WriteJobHandler struct {
	loader *JobsLoader
}

func (h *WriteJobHandler) Name() string { return "write_job" }
func (h *WriteJobHandler) Enabled(ctx *ToolContext) bool {
	// 仅管理员可创建/修改任务
	return h.loader != nil && ctx.IsSensitiveAllowed
}
func (h *WriteJobHandler) Definition(ctx *ToolContext) Tool {
	return Tool{
		Type: "function",
		Function: &ToolFunction{
			Name:        "write_job",
			Description: "创建或更新一个任务文档。任务内容必须使用包含 YAML 前言的 Markdown 格式。仅管理员可用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "任务ID（会作为 config/jobs/ 下的目录名）",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "完整的 JOB.md 内容（包含 YAML 前言和 Markdown 正文）",
					},
				},
				"required": []string{"job_id", "content"},
			},
		},
	}
}
func (h *WriteJobHandler) Execute(args map[string]any, ctx *ToolContext) string {
	jobID, _ := args["job_id"].(string)
	content, _ := args["content"].(string)
	if jobID == "" || content == "" {
		return "缺少 job_id 或 content 参数"
	}
	log.Printf("[工具] 【写入任务】ID: %s", jobID)

	if err := h.loader.WriteJob(jobID, content); err != nil {
		return fmt.Sprintf("写入任务失败: %v", err)
	}
	return fmt.Sprintf("任务 %s 已成功保存", jobID)
}

