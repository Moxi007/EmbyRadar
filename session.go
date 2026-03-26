package main

import (
	"log"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// MessageTask 待处理的消息任务
type MessageTask struct {
	Msg    *tgbotapi.Message
	DoneCh chan struct{} // 处理完成信号（可选，用于同步等待）
}

// SessionManager 管理每个会话（按 chatID 隔离）的消息处理队列。
// 同一 chatID 的消息严格按顺序处理，不同 chatID 之间互不影响。
// 解决原来 `go ch.handleAIResponse(msg)` 导致的并发上下文竞争问题。
type SessionManager struct {
	queues   map[int64]chan *MessageTask // chatID → 消息管道
	workers  map[int64]bool             // chatID → worker 是否活跃
	mu       sync.Mutex
	handler  func(msg *tgbotapi.Message) // 实际的消息处理函数
	chanSize int                         // 每个 chatID 的队列缓冲大小
}

// NewSessionManager 创建会话管理器
// handler 为实际的消息处理函数（如 ChatHandler.handleAIResponse）
func NewSessionManager(handler func(msg *tgbotapi.Message)) *SessionManager {
	return &SessionManager{
		queues:   make(map[int64]chan *MessageTask),
		workers:  make(map[int64]bool),
		handler:  handler,
		chanSize: 10, // 每个会话最多缓冲 10 条消息
	}
}

// Enqueue 将消息放入对应 chatID 的队列，确保同一会话的消息按顺序处理
func (sm *SessionManager) Enqueue(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	sm.mu.Lock()

	// 获取或创建该 chatID 的消息队列
	queue, exists := sm.queues[chatID]
	if !exists {
		queue = make(chan *MessageTask, sm.chanSize)
		sm.queues[chatID] = queue
	}

	// 确保有 worker 在处理该队列
	if !sm.workers[chatID] {
		sm.workers[chatID] = true
		go sm.worker(chatID, queue)
	}

	sm.mu.Unlock()

	// 将任务放入队列（非阻塞，如果满了则丢弃并提示）
	task := &MessageTask{
		Msg:    msg,
		DoneCh: make(chan struct{}),
	}

	select {
	case queue <- task:
		// 入队成功
	default:
		// 队列溢出，丢弃消息
		log.Printf("[会话队列] 警告: chatID=%d 的消息队列已满 (%d 条)，丢弃消息", chatID, sm.chanSize)
	}
}

// worker 为单个 chatID 的消息处理 goroutine
// 从队列中逐条取出消息并处理，确保同一会话的消息顺序执行
// 当队列空闲超过 60 秒后自动退出，释放资源
func (sm *SessionManager) worker(chatID int64, queue chan *MessageTask) {
	idleTimeout := 60 * time.Second

	defer func() {
		sm.mu.Lock()
		delete(sm.workers, chatID)
		// 如果队列已空，也清理队列引用
		if len(queue) == 0 {
			delete(sm.queues, chatID)
		}
		sm.mu.Unlock()
		log.Printf("[会话队列] chatID=%d 的 worker 已退出（空闲超时）", chatID)
	}()

	for {
		select {
		case task := <-queue:
			// 处理消息（同步执行，保证顺序）
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[会话队列] chatID=%d 的消息处理 panic: %v", chatID, r)
					}
					close(task.DoneCh)
				}()
				sm.handler(task.Msg)
			}()

		case <-time.After(idleTimeout):
			// 空闲超时，退出 worker
			return
		}
	}
}
