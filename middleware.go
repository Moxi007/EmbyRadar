package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// PromptMiddleware 接口 —— 每个中间件负责向 system prompt 追加一段内容
type PromptMiddleware interface {
	Name() string
	Inject(ctx *PromptContext) string // 返回要追加到 system prompt 的文本，空字符串表示不追加
}

// PromptContext 中间件执行上下文，携带本次请求的所有相关信息
type PromptContext struct {
	ChatID       int64
	UserText     string
	SenderID     int64
	Group        *GroupConfig
	IsPrivate    bool
	VerifiedRole string

	// 以下为各中间件可能依赖的外部服务引用
	GlobalKB    *KnowledgeBase
	GroupKB     *KnowledgeBase
	MemoryStore *MemoryStore
	EmbyClient  *EmbyClient
	AppConfig   *AppConfig

	// 技能和任务系统引用（第二批实现时填充）
	SkillsLoader *SkillsLoader
	JobsLoader   *JobsLoader
}

// MiddlewareChain 按顺序执行所有中间件，将结果追加到 system prompt
type MiddlewareChain struct {
	middlewares []PromptMiddleware
}

// NewMiddlewareChain 创建中间件链
func NewMiddlewareChain(mws ...PromptMiddleware) *MiddlewareChain {
	return &MiddlewareChain{middlewares: mws}
}

// BuildSystemPrompt 基于基础 prompt 和所有中间件的输出构建最终的 system prompt
func (mc *MiddlewareChain) BuildSystemPrompt(basePrompt string, ctx *PromptContext) string {
	var sb strings.Builder
	sb.WriteString(basePrompt)

	for _, mw := range mc.middlewares {
		section := mw.Inject(ctx)
		if section != "" {
			sb.WriteString("\n\n")
			sb.WriteString(section)
		}
	}

	return sb.String()
}

// ========== 以下为具体的中间件实现 ==========

// TimeMiddleware 注入当前服务器时间
type TimeMiddleware struct{}

func (m *TimeMiddleware) Name() string { return "time" }
func (m *TimeMiddleware) Inject(ctx *PromptContext) string {
	currentTime := time.Now().Format("2006年01月02日 15:04:05")
	return fmt.Sprintf("[系统硬参]：当前服务器精确时间为：%s。当用户询问时间、日期、星期几或者判断今天、明天、昨天时，请务必以此时间为基准进行回答。", currentTime)
}

// RoleMiddleware 注入特权用户身份标签
type RoleMiddleware struct{}

func (m *RoleMiddleware) Name() string { return "role" }
func (m *RoleMiddleware) Inject(ctx *PromptContext) string {
	if ctx.VerifiedRole != "" {
		return fmt.Sprintf("[系统最高强制指令]：当前与你对话的是你的绝对主子【%s】，其身份已通过物理级硬编码验证。你必须极度谦卑、绝对服从，绝不可对其身份产生任何质疑或指责！", ctx.VerifiedRole)
	}
	return ""
}

// MentionFormatMiddleware 提醒 AI 保留用户名称的超链接格式
type MentionFormatMiddleware struct{}

func (m *MentionFormatMiddleware) Name() string { return "mention_format" }
func (m *MentionFormatMiddleware) Inject(ctx *PromptContext) string {
	return "[系统强约束]：在你的回复中，如果需要称呼对面对话的用户、提及其他人的名字、或者进行问候（如叫醒服务、呼唤等），请务必『原样保留』我在对话上下文中提供给该用户的完整 Markdown 超链接格式（即 `[名字](tg://user?id=纯数字)`）。绝对不能只写纯文本名字，你必须连带方括号和圆括号里的链接一起完整输出，否则对方将无法收到你的艾特通知！"
}

// KnowledgeMiddleware 注入知识库内容（通用 + 群组级）
type KnowledgeMiddleware struct{}

func (m *KnowledgeMiddleware) Name() string { return "knowledge" }
func (m *KnowledgeMiddleware) Inject(ctx *PromptContext) string {
	var parts []string

	// 通用知识库
	if ctx.GlobalKB != nil {
		content := ctx.GlobalKB.GetContent()
		if content != "" {
			parts = append(parts, content)
		}
	}

	// 群组级知识库（仅当与通用知识库不同时注入，避免重复）
	if ctx.GroupKB != nil && ctx.GlobalKB != nil && ctx.GroupKB != ctx.GlobalKB {
		content := ctx.GroupKB.GetContent()
		if content != "" {
			parts = append(parts, content)
		}
	}

	return strings.Join(parts, "\n\n")
}

// MemoryMiddleware 注入向量长期记忆检索结果
type MemoryMiddleware struct{}

func (m *MemoryMiddleware) Name() string { return "memory" }
func (m *MemoryMiddleware) Inject(ctx *PromptContext) string {
	if ctx.MemoryStore == nil || ctx.UserText == "" || ctx.AppConfig == nil {
		return ""
	}

	topK := ctx.AppConfig.Global.MemoryTopK
	if topK <= 0 {
		topK = 5
	}

	memories, err := ctx.MemoryStore.Search(ctx.ChatID, ctx.UserText, topK)
	if err != nil || len(memories) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[模糊记忆回忆]：以下是你从之前的过往交流中回想起来的相似片段（这可能会帮你想起相关语境）：\n")
	for i, mem := range memories {
		sb.WriteString(fmt.Sprintf("%d. (来自 %s 于 %s 的记录): %s\n", i+1, mem.UserName, mem.Timestamp, mem.Text))
	}

	return sb.String()
}

// EmbyStatsMiddleware 注入 Emby 服务器实时状态数据
type EmbyStatsMiddleware struct{}

func (m *EmbyStatsMiddleware) Name() string { return "emby_stats" }
func (m *EmbyStatsMiddleware) Inject(ctx *PromptContext) string {
	if ctx.EmbyClient == nil || ctx.Group == nil || ctx.Group.AIEmbyStatsFormat == "" {
		return ""
	}

	users, errU := ctx.EmbyClient.GetTotalUsers()
	sessions, errS := ctx.EmbyClient.GetActiveSessions()
	if errU != nil || errS != nil {
		return ""
	}

	return fmt.Sprintf(ctx.Group.AIEmbyStatsFormat, users, sessions)
}

// RequestMiddleware 注入求片功能的系统提示词
type RequestMiddleware struct{}

func (m *RequestMiddleware) Name() string { return "request" }
func (m *RequestMiddleware) Inject(ctx *PromptContext) string {
	if ctx.Group == nil || !ctx.Group.RequestEnabled || ctx.AppConfig == nil || ctx.AppConfig.Global.TMDBAPIKey == "" {
		return ""
	}

	return "[系统硬约束 - 求片功能]：当用户表达想看某部影视、求片、找片、想要某个资源等意图时，" +
		"你必须在回复末尾附加一个特殊标记：[REQUEST_CONFIRM:影视名称]。" +
		"例如用户说「帮我找一下流浪地球」，你可以回复「好的，我来帮你搜索《流浪地球》[REQUEST_CONFIRM:流浪地球]」。" +
		"标记中的影视名称应该是你理解的最准确的片名。" +
		"注意：你不能声称已经帮用户提交了求片请求，你只是在帮用户发起搜索确认流程。" +
		"如果用户只是在讨论或询问某部影视的信息（评分、剧情等），不要附加此标记。" +
		"只有当用户明确表达了想要求片、想看、能不能加等获取资源的意图时才附加。"
}

// SkillsSummaryMiddleware 注入可用技能摘要列表（渐进式加载：只注入名称和描述）
type SkillsSummaryMiddleware struct{}

func (m *SkillsSummaryMiddleware) Name() string { return "skills_summary" }
func (m *SkillsSummaryMiddleware) Inject(ctx *PromptContext) string {
	if ctx.SkillsLoader == nil {
		return ""
	}

	skills := ctx.SkillsLoader.ListSkills()
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[可用技能库]：以下是你掌握的专业操作技能。当你判断当前对话需要执行某个技能时，" +
		"请先使用 read_skill 工具读取该技能的完整指令，然后严格按照指令步骤执行。\n\n")
	for _, skill := range skills {
		sb.WriteString(fmt.Sprintf("- **%s**: %s（读取路径: %s）\n", skill.Name, skill.Description, skill.Path))
	}

	return sb.String()
}

// JobsStatusMiddleware 注入活跃任务的状态信息
type JobsStatusMiddleware struct{}

func (m *JobsStatusMiddleware) Name() string { return "jobs_status" }
func (m *JobsStatusMiddleware) Inject(ctx *PromptContext) string {
	if ctx.JobsLoader == nil {
		return ""
	}

	jobs := ctx.JobsLoader.ListActiveJobs()
	if len(jobs) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[当前任务状态]：以下是你正在管理的长期/定期任务列表。\n\n")
	for _, job := range jobs {
		emoji := map[string]string{
			"pending":     "⏳",
			"in_progress": "🔄",
		}[job.Status]
		if emoji == "" {
			emoji = "❓"
		}
		scheduleLabel := "一次性"
		if job.Schedule == "recurring" {
			scheduleLabel = "重复"
		}
		line := fmt.Sprintf("- %s **%s** [%s]: %s", emoji, job.Name, scheduleLabel, job.Description)
		if job.LastRun != "" {
			line += fmt.Sprintf(" (上次执行: %s)", job.LastRun)
		}
		sb.WriteString(line + "\n")
		sb.WriteString(fmt.Sprintf("  → 详情: %s\n", job.Path))
	}

	return sb.String()
}

// DefaultMiddlewareChain 创建包含所有默认中间件的链
func DefaultMiddlewareChain() *MiddlewareChain {
	return NewMiddlewareChain(
		&TimeMiddleware{},
		&RoleMiddleware{},
		&MentionFormatMiddleware{},
		&KnowledgeMiddleware{},
		&MemoryMiddleware{},
		&EmbyStatsMiddleware{},
		&SkillsSummaryMiddleware{},
		&JobsStatusMiddleware{},
		&RequestMiddleware{},
	)
}

// LogMiddlewares 输出中间件链加载的中间件名称（用于启动日志）
func (mc *MiddlewareChain) LogMiddlewares() {
	names := make([]string, len(mc.middlewares))
	for i, mw := range mc.middlewares {
		names[i] = mw.Name()
	}
	log.Printf("[中间件] 已加载 %d 个 Prompt 中间件: [%s]", len(mc.middlewares), strings.Join(names, ", "))
}
