package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// DigestScheduler 每日对话摘要调度器
type DigestScheduler struct {
	aiClient   *AIClient
	ctxManager *ContextManager
	kbMap      map[int64]*KnowledgeBase
	groups     []*GroupConfig
	digestHour int
	stopCh     chan struct{}
}

// NewDigestScheduler 创建摘要调度器
func NewDigestScheduler(aiClient *AIClient, ctxManager *ContextManager, appConfig *AppConfig) *DigestScheduler {
	// 如果全局未在此环节配置 aiClient (没有通过初始化检查)，则不能启动
	if aiClient == nil {
		return nil
	}
	
	ds := &DigestScheduler{
		aiClient:   aiClient,
		ctxManager: ctxManager,
		groups:     appConfig.Groups,
		digestHour: appConfig.Global.DigestHour,
		stopCh:     make(chan struct{}),
	}
	
	// 从各群组初始化信息里找到知识库引用，由于 main.go 中 ChatHandler 初始化了 kbMap，
	// 最方便的是我们在生成总结时直接调用对应的知识库，所以我们允许通过 group 中的 AIKnowledgeDir 拿到它，
	// 但实际上最好是直接操作对应路径的 KnowledgeBase 实例。
	ds.kbMap = make(map[int64]*KnowledgeBase)
	for _, g := range appConfig.Groups {
		ds.kbMap[g.TelegramChatID] = NewKnowledgeBase(g.AIKnowledgeDir)
	}

	return ds
}

// Start 开始调度定时任务
func (ds *DigestScheduler) Start() {
	go ds.scheduleLoop()
	log.Printf("[每日摘要] 调度器已启动，每天 %02d:00 自动执行知识整理", ds.digestHour)
}

// Stop 停止调度
func (ds *DigestScheduler) Stop() {
	close(ds.stopCh)
}

// scheduleLoop 定时循环
func (ds *DigestScheduler) scheduleLoop() {
	for {
		now := time.Now()
		// 计算距离下一次执行的时间
		next := time.Date(now.Year(), now.Month(), now.Day(), ds.digestHour, 0, 0, 0, now.Location())
		if now.After(next) {
			// 如果今天的时间已经过了，算到明天
			next = next.Add(24 * time.Hour)
		}
		
		duration := next.Sub(now)
		
		// 避免日志频繁输出，但作为启动提示可以在这里打一次
		
		select {
		case <-time.After(duration):
			// 时间到了，执行摘要
			ds.RunDigest()
		case <-ds.stopCh:
			return
		}
	}
}

// RunDigest 立即对所有群组执行摘要（支持外部直接触发）
func (ds *DigestScheduler) RunDigest() {
	log.Printf("[每日摘要] 开始执行群体日志清理与摘要任务...")
	for _, g := range ds.groups {
		if !g.AIEnabled {
			continue // 没开启 AI 的群不处理
		}
		err := ds.digestGroup(g.TelegramChatID)
		if err != nil {
			log.Printf("[每日摘要] 群组 %d 处理失败: %v", g.TelegramChatID, err)
		}
	}
	log.Printf("[每日摘要] 今日知识整理任务执行完毕")
}

// digestGroup 提取单个群的对话日志并交由 AI 总结
func (ds *DigestScheduler) digestGroup(chatID int64) error {
	logs := ds.ctxManager.ExtractAndClearDailyLog(chatID)
	if len(logs) == 0 {
		return nil // 今天没有说过话
	}
	
	// 如果只有寥寥几句，不到10条，可以考虑直接丢弃不提炼（避免无效触发），
	// 但为了简单，有哪怕一句话也让 AI 看看有没有价值
	
	var sb strings.Builder
	for _, msg := range logs {
		// msg.Content.Text 已经包含了 "User: message" 格式
		sb.WriteString(msg.Content.Text)
		sb.WriteString("\n")
	}
	rawText := sb.String()
	
	// 如果超过 Token 限制，简单截断（更好的是切片分批，但为了快速落地先截断前 15000 字符）
	if len(rawText) > 15000 {
		rawText = rawText[:15000] + "\n...[内容过长截断]"
	}

	systemPrompt := `你是一个群聊日志分析与知识沉淀引擎。
你的任务是从以下群聊的“今日完整对话记录”中提取有价值的信息，以Markdown列表的格式输出。

**提取规则：**
1. **用户偏好/画像：** 提取特定用户的偏好（例如“A用户喜欢看科幻电影”，“B用户寻求某个特定资源”）。
2. **共识与规矩：** 群成员达成的明显共识、新制定的规矩或群主下达的指令。
3. **重要事件：** 发生的重要讨论或结论。
4. **排除废话：** 剔除所有毫无意义的闲聊、打招呼、表情包等。
5. **高度浓缩：** 每个知识点必须尽可能简短，直接描述事实，不带时间戳。

如果今天的聊天记录中完全没有上述有价值的信息，请**直接回复“无”**，并坚决不要输出任何其他内容。
如果不为“无”，直接输出总结的内容。不需要开头结尾等客套语。`

	// 组装请求
	messages := []ChatMessage{
		{Role: "system", Content: MessageContent{Text: systemPrompt}},
		{Role: "user", Content: MessageContent{Text: "今日群聊记录如下：\n\n" + rawText}},
	}
	
	log.Printf("[每日摘要] 正在调用 AI 提炼群聊 %d 的知识点，共 %d 条原消息", chatID, len(logs))
	
	responseMsg, err := ds.aiClient.ChatCompletion(messages, nil)
	if err != nil {
		return fmt.Errorf("AI 提炼失败: %w", err)
	}
	
	result := strings.TrimSpace(responseMsg.Content.Text)
	if result == "" || result == "无" || strings.Contains(result, "没有提取到") {
		log.Printf("[每日摘要] 群聊 %d 今日无实质知识沉淀。", chatID)
		return nil
	}
	
	// 清洗脱敏标记：即使记录里遗留了标记也安全移除
	result = strings.ReplaceAll(result, "[INTERNAL_AUTH_TAG: ✅已验证身份]", "")
	result = strings.ReplaceAll(result, "[INTERNAL_AUTH_TAG: ⚠️未知平民]", "")
	
	// 按天区分词条，使用 KnowledgeBase.MergeEntry 将这一信息作为独立条目合入，或者以“群体记忆补充”名义合入
	entryName := "群体日常记忆沉淀.md"
	
	kb := ds.kbMap[chatID]
	if kb == nil {
		return fmt.Errorf("未找到对应的知识库实例")
	}
	
	// 在写入的内容前加上日期提示
	datePrefix := fmt.Sprintf("### %s 记忆更新\n", time.Now().Format("2006-01-02"))
	newContent := datePrefix + result
	
	// 使用现有的智能合并逻辑，AI 会将每天新学的记忆和以前的“群体日常记忆沉淀”条目进行智能融合与去重
	isNew, err := kb.MergeEntry(entryName, newContent, ds.aiClient)
	if err != nil {
		return fmt.Errorf("将摘要并入知识库失败: %w", err)
	}
	
	if isNew {
		log.Printf("[每日摘要] 群聊 %d 创建了新的沉淀词条。内容片段: %s...", chatID, result[:min(30, len(result))])
	} else {
		log.Printf("[每日摘要] 群聊 %d 沉淀内容已成功与已有记忆知识融合汇整。", chatID)
	}
	
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
