package main

import (
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// TransportExecutor 抽象 Telegram 动作执行面，为后续替换 userbot 预留接口。
type TransportExecutor interface {
	Say(chatID int64, text string) (int, error)
	Reply(chatID int64, replyToMessageID int, text string) (int, error)
	Sticker(chatID int64, sticker string) (int, error)
	Pin(chatID int64, messageID int, disableNotification bool) error
}

type TelegramTransport struct {
	bot *tgbotapi.BotAPI
}

func NewTelegramTransport(bot *tgbotapi.BotAPI) *TelegramTransport {
	return &TelegramTransport{bot: bot}
}

func (t *TelegramTransport) Say(chatID int64, text string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	sent, err := t.bot.Send(msg)
	if err != nil {
		msg.ParseMode = ""
		sent, err = t.bot.Send(msg)
		if err != nil {
			return 0, fmt.Errorf("发送消息失败: %w", err)
		}
	}
	return sent.MessageID, nil
}

func (t *TelegramTransport) Reply(chatID int64, replyToMessageID int, text string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyToMessageID = replyToMessageID
	msg.ParseMode = "Markdown"
	sent, err := t.bot.Send(msg)
	if err != nil {
		msg.ParseMode = ""
		sent, err = t.bot.Send(msg)
		if err != nil {
			return 0, fmt.Errorf("回复消息失败: %w", err)
		}
	}
	return sent.MessageID, nil
}

func (t *TelegramTransport) Sticker(chatID int64, sticker string) (int, error) {
	msg := tgbotapi.NewSticker(chatID, tgbotapi.FileID(sticker))
	sent, err := t.bot.Send(msg)
	if err != nil {
		return 0, fmt.Errorf("发送贴纸失败: %w", err)
	}
	return sent.MessageID, nil
}

func (t *TelegramTransport) Pin(chatID int64, messageID int, disableNotification bool) error {
	_, err := t.bot.Request(tgbotapi.PinChatMessageConfig{
		ChatID:              chatID,
		MessageID:           messageID,
		DisableNotification: disableNotification,
	})
	if err != nil {
		return fmt.Errorf("置顶消息失败: %w", err)
	}
	return nil
}
