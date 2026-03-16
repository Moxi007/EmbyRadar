package main

import (
	"sync"
	"time"
)

// ContextManager 管理每个群聊的对话上下文（内存存储）
type ContextManager struct {
	maxRounds int                    // 最大保留轮数
	timeout   time.Duration          // 上下文超时时间
	contexts  map[int64]*ChatContext // 按群聊 ID 分组
	mu        sync.RWMutex
}

// ChatContext 表示单个群聊的对话上下文
type ChatContext struct {
	Messages   []ChatMessage // 历史消息列表 (受最大轮数控制，用于即时对话)
	DailyLog   []ChatMessage // 每日对话日志（完整记录，用于 AI 的每日摘要沉淀）
	LastActive time.Time     // 最后活跃时间
}

// NewContextManager 创建上下文管理器
func NewContextManager(maxRounds int) *ContextManager {
	if maxRounds <= 0 {
		maxRounds = 20 // 默认保留 20 轮
	}
	cm := &ContextManager{
		maxRounds: maxRounds,
		timeout:   30 * time.Minute, // 30 分钟无活跃则清空
		contexts:  make(map[int64]*ChatContext),
	}
	// 启动定时清理 goroutine
	go cm.cleanupLoop()
	return cm
}

// AddMessage 向指定群聊添加一条消息
func (cm *ContextManager) AddMessage(chatID int64, msg ChatMessage) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	ctx, ok := cm.contexts[chatID]
	if !ok {
		ctx = &ChatContext{
			Messages: make([]ChatMessage, 0),
			DailyLog: make([]ChatMessage, 0),
		}
		cm.contexts[chatID] = ctx
	}

	ctx.Messages = append(ctx.Messages, msg)
	
	// 同时存入 DailyLog，过滤掉太长的媒体数据（实际上外层调用时已对图片等转为短文本描述）
	ctx.DailyLog = append(ctx.DailyLog, msg)
	
	ctx.LastActive = time.Now()

	// 超出最大轮数时，丢弃最早的消息（每轮 = 一问一答 = 2 条消息）
	maxMessages := cm.maxRounds * 2
	if len(ctx.Messages) > maxMessages {
		ctx.Messages = ctx.Messages[len(ctx.Messages)-maxMessages:]
	}
}

// GetMessages 获取指定群聊的历史消息
func (cm *ContextManager) GetMessages(chatID int64) []ChatMessage {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	ctx, ok := cm.contexts[chatID]
	if !ok {
		return nil
	}

	// 检查是否已超时
	if time.Since(ctx.LastActive) > cm.timeout {
		return nil
	}

	// 返回副本，避免外部修改
	result := make([]ChatMessage, len(ctx.Messages))
	copy(result, ctx.Messages)
	return result
}

// ClearContext 清空指定群聊的上下文
func (cm *ContextManager) ClearContext(chatID int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	// 注意：只清空短期记忆，不清空每日日志（DailyLog 由摘要定时器清空）
	if ctx, ok := cm.contexts[chatID]; ok {
		ctx.Messages = make([]ChatMessage, 0)
	}
}

// ExtractAndClearDailyLog 提取并清空指定群聊的每日对话日志
func (cm *ContextManager) ExtractAndClearDailyLog(chatID int64) []ChatMessage {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	ctx, ok := cm.contexts[chatID]
	if !ok || len(ctx.DailyLog) == 0 {
		return nil
	}

	result := make([]ChatMessage, len(ctx.DailyLog))
	copy(result, ctx.DailyLog)
	
	// 清空日志，准备开启新的一天
	ctx.DailyLog = make([]ChatMessage, 0)
	return result
}

// cleanupLoop 定期清理超时的上下文
func (cm *ContextManager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		cm.mu.Lock()
		for chatID, ctx := range cm.contexts {
			if time.Since(ctx.LastActive) > cm.timeout {
				// 仅清空即时短期记忆，保留 DailyLog 避免丢失
				ctx.Messages = make([]ChatMessage, 0)
				
				// 如果 DailyLog 也是空的，证明不仅超时还没留存实质对话，可整体释放
				if len(ctx.DailyLog) == 0 {
					delete(cm.contexts, chatID)
				}
			}
		}
		cm.mu.Unlock()
	}
}
