package main

import (
	"fmt"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Poller 入库轮询检测器，定时检查已审批通过的求片记录是否已入库 Emby
type Poller struct {
	store    *RequestStore
	embyMap  map[int64]*EmbyClient // 复用 ChatHandler 的 embyMap，按群聊 ID 索引
	bot      *tgbotapi.BotAPI
	interval time.Duration // 轮询间隔，默认 30 分钟
	stopCh   chan struct{} // 用于优雅停止轮询
}

// NewPoller 创建轮询器实例
func NewPoller(store *RequestStore, embyMap map[int64]*EmbyClient, bot *tgbotapi.BotAPI, interval time.Duration) *Poller {
	return &Poller{
		store:    store,
		embyMap:  embyMap,
		bot:      bot,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动后台轮询 goroutine，按配置间隔定时执行轮询
func (p *Poller) Start() {
	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		log.Printf("[Poller] 入库轮询已启动，间隔: %v", p.interval)

		for {
			select {
			case <-ticker.C:
				p.poll()
			case <-p.stopCh:
				log.Printf("[Poller] 入库轮询已停止")
				return
			}
		}
	}()
}

// Stop 停止轮询器，通知后台 goroutine 退出
func (p *Poller) Stop() {
	close(p.stopCh)
}

// poll 执行一次轮询：查询 approved 记录 → 逐条检测入库 → 处理过期记录
func (p *Poller) poll() {
	// 查询所有 approved 状态的记录
	records, err := p.store.ListApproved()
	if err != nil {
		log.Printf("[Poller] 查询 approved 记录失败: %v", err)
		return
	}

	// 逐条检测入库状态
	for _, record := range records {
		if err := p.checkAndNotify(record); err != nil {
			log.Printf("[Poller] 检测记录 %d 入库状态失败: %v", record.ID, err)
		}
	}

	// 查询超过 30 天仍未入库的 approved 记录，标记为过期
	expired, err := p.store.ListExpiredApproved(30)
	if err != nil {
		log.Printf("[Poller] 查询过期记录失败: %v", err)
		return
	}
	for _, record := range expired {
		if err := p.store.UpdateStatus(record.ID, "expired"); err != nil {
			log.Printf("[Poller] 标记记录 %d 为过期失败: %v", record.ID, err)
		} else {
			log.Printf("[Poller] 记录 %d (%s) 已超过 30 天未入库，标记为过期", record.ID, record.Title)
		}
	}
}

// checkAndNotify 检测单条记录的入库状态，入库则发送通知并更新状态
// 使用 TMDB ID + 媒体类型精确匹配，避免标题模糊搜索导致的误判
func (p *Poller) checkAndNotify(record *RequestRecord) error {
	// 根据记录的群聊 ID 查找对应的 EmbyClient
	embyClient := p.embyMap[record.ChatID]
	if embyClient == nil {
		log.Printf("[Poller] 群聊 %d 未配置 EmbyClient，跳过记录 %d", record.ChatID, record.ID)
		return nil
	}

	// 通过 TMDB ID 和媒体类型精确查询 Emby 媒体库
	items, err := embyClient.SearchByProviderID(record.TMDBID, record.MediaType)
	if err != nil {
		return fmt.Errorf("Emby 搜索失败: %w", err)
	}

	// 搜索结果为空，资源尚未入库
	if len(items) == 0 {
		return nil
	}

	// 资源已入库，发送通知消息到原群聊
	notifyText := FormatFulfilledNotify(record.UserID, record.Title)
	notifyMsg := tgbotapi.NewMessage(record.ChatID, notifyText)
	notifyMsg.ParseMode = "Markdown"
	if _, err := p.bot.Send(notifyMsg); err != nil {
		// 通知发送失败，保持 approved 状态，下一轮重试
		return fmt.Errorf("发送入库通知失败: %w", err)
	}

	// 通知成功，更新状态为 fulfilled
	if err := p.store.UpdateStatus(record.ID, "fulfilled"); err != nil {
		return fmt.Errorf("更新状态为 fulfilled 失败: %w", err)
	}

	log.Printf("[Poller] 记录 %d (%s) 已入库，通知已发送到群聊 %d", record.ID, record.Title, record.ChatID)
	return nil
}

// FormatFulfilledNotify 生成入库通知消息，包含用户 Markdown 链接和影视标题
func FormatFulfilledNotify(userID int64, title string) string {
	userLink := fmt.Sprintf("[用户](tg://user?id=%d)", userID)
	return fmt.Sprintf("🎉 %s 你求的「%s」已入库，快去看吧", userLink, title)
}
