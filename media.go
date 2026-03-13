package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// MediaContent 表示已处理的媒体内容，包含 Base64 编码数据和格式信息
type MediaContent struct {
	Base64Data string // Base64 编码后的文件数据
	MIMEType   string // MIME 类型，如 "image/jpeg" 或 "video/mp4"
	IsVideo    bool   // 是否为视频文件（影响构建 content 数组时的类型字段）
}

// 支持的图片 MIME 类型白名单
var supportedImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// 支持的视频 MIME 类型白名单
var supportedVideoTypes = map[string]bool{
	"video/mp4":  true,
	"video/webm": true,
	"video/mpeg": true,
	"video/avi":  true,
}

// 文件扩展名到 MIME 类型的映射，用于 Telegram 未提供 MIME 信息时的回退推断
var extToMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mpeg": "video/mpeg",
	".avi":  "video/avi",
}

// 最大图片文件大小限制：20MB
const maxImageFileSize = 20 * 1024 * 1024

// 最大视频文件大小限制：50MB
const maxVideoFileSize = 50 * 1024 * 1024

// inferMIMEType 根据文件路径的扩展名推断 MIME 类型。
// 不支持的扩展名返回空字符串，调用方应据此决定是否跳过该文件。
func inferMIMEType(filePath string) string {
	ext := strings.ToLower(path.Ext(filePath))
	if mime, ok := extToMIME[ext]; ok {
		return mime
	}
	return ""
}

// selectBestPhoto 从 Telegram 提供的多个尺寸版本中选择最合适的图片。
// 优先选择文件大小最大但不超过 maxImageFileSize 的版本；
// 如果所有版本都超过限制，则返回最小的版本作为兜底。
func selectBestPhoto(photos []tgbotapi.PhotoSize) *tgbotapi.PhotoSize {
	if len(photos) == 0 {
		return nil
	}

	// 在不超过限制的版本中找最大的
	var best *tgbotapi.PhotoSize
	for i := range photos {
		if photos[i].FileSize <= maxImageFileSize {
			if best == nil || photos[i].FileSize > best.FileSize {
				best = &photos[i]
			}
		}
	}
	if best != nil {
		return best
	}

	// 所有版本都超过限制，返回最小的版本
	smallest := &photos[0]
	for i := 1; i < len(photos); i++ {
		if photos[i].FileSize < smallest.FileSize {
			smallest = &photos[i]
		}
	}
	return smallest
}

// downloadFile 通过 Telegram Bot API 下载指定 fileID 对应的文件。
// 先调用 GetFile 获取文件元信息，再通过 HTTP GET 下载实际字节数据。
// 同时返回服务端文件路径（FilePath），供调用方推断 MIME 类型。
func downloadFile(bot *tgbotapi.BotAPI, fileID string) ([]byte, string, error) {
	// 获取文件信息（含服务端路径）
	fileConfig := tgbotapi.FileConfig{FileID: fileID}
	file, err := bot.GetFile(fileConfig)
	if err != nil {
		return nil, "", fmt.Errorf("获取文件信息失败: %w", err)
	}

	// 拼接下载链接并发起 HTTP 请求
	fileURL := file.Link(bot.Token)
	resp, err := http.Get(fileURL)
	if err != nil {
		return nil, "", fmt.Errorf("下载文件失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("下载文件返回非 200 状态码: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("读取文件数据失败: %w", err)
	}

	return data, file.FilePath, nil
}

// extractMedia 从 Telegram 消息中提取媒体内容（图片或视频）。
// 返回 nil 表示消息不含可处理的媒体，或提取过程中发生错误（优雅降级）。
func extractMedia(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) *MediaContent {
	// 优先处理图片消息
	if len(msg.Photo) > 0 {
		return extractPhoto(bot, msg.Photo)
	}

	// 处理视频消息
	if msg.Video != nil {
		return extractVideo(bot, msg.Video)
	}

	return nil
}

// extractPhoto 处理图片消息：选择最佳尺寸 → 下载 → 格式校验 → Base64 编码
func extractPhoto(bot *tgbotapi.BotAPI, photos []tgbotapi.PhotoSize) *MediaContent {
	photo := selectBestPhoto(photos)
	if photo == nil {
		return nil
	}

	// 检查文件大小是否超过限制
	if photo.FileSize > maxImageFileSize {
		log.Printf("[媒体] 图片文件大小 %d 字节超过 20MB 限制，跳过", photo.FileSize)
		return nil
	}

	// 下载图片文件，同时获取服务端文件路径用于 MIME 推断
	data, filePath, err := downloadFile(bot, photo.FileID)
	if err != nil {
		log.Printf("[媒体] 下载图片失败: %v", err)
		return nil
	}

	// 根据服务端文件路径推断 MIME 类型，无法推断时默认 image/jpeg
	mimeType := inferMIMEType(filePath)
	if mimeType == "" {
		mimeType = "image/jpeg"
	}

	// 校验是否为支持的图片格式
	if !supportedImageTypes[mimeType] {
		log.Printf("[媒体] 不支持的图片格式: %s，跳过", mimeType)
		return nil
	}

	return &MediaContent{
		Base64Data: base64.StdEncoding.EncodeToString(data),
		MIMEType:   mimeType,
		IsVideo:    false,
	}
}

// extractVideo 处理视频消息：检查大小 → 下载完整视频或回退到缩略图 → Base64 编码
func extractVideo(bot *tgbotapi.BotAPI, video *tgbotapi.Video) *MediaContent {
	// 视频文件大小在限制范围内，下载完整视频
	if video.FileSize <= maxVideoFileSize {
		data, filePath, err := downloadFile(bot, video.FileID)
		if err != nil {
			log.Printf("[媒体] 下载视频失败: %v", err)
			return nil
		}

		// 优先根据服务端路径推断，其次使用 Telegram 提供的 MimeType，最后兜底 video/mp4
		mimeType := inferMIMEType(filePath)
		if mimeType == "" {
			mimeType = inferMIMEType(video.FileName)
		}
		if mimeType == "" && video.MimeType != "" {
			mimeType = video.MimeType
		}
		if mimeType == "" {
			mimeType = "video/mp4"
		}

		// 校验是否为支持的视频格式
		if !supportedVideoTypes[mimeType] {
			log.Printf("[媒体] 不支持的视频格式: %s，跳过", mimeType)
			return nil
		}

		return &MediaContent{
			Base64Data: base64.StdEncoding.EncodeToString(data),
			MIMEType:   mimeType,
			IsVideo:    true,
		}
	}

	// 视频超过 50MB，回退到缩略图
	log.Printf("[媒体] 视频文件大小 %d 字节超过 50MB 限制，回退到缩略图", video.FileSize)
	if video.Thumbnail == nil {
		log.Printf("[媒体] 视频无缩略图可用，跳过媒体提取")
		return nil
	}

	// 下载缩略图
	data, filePath, err := downloadFile(bot, video.Thumbnail.FileID)
	if err != nil {
		log.Printf("[媒体] 下载视频缩略图失败: %v", err)
		return nil
	}

	// 根据服务端路径推断缩略图格式，默认 image/jpeg
	mimeType := inferMIMEType(filePath)
	if mimeType == "" {
		mimeType = "image/jpeg"
	}

	if !supportedImageTypes[mimeType] {
		log.Printf("[媒体] 不支持的缩略图格式: %s，跳过", mimeType)
		return nil
	}

	// 缩略图作为图片处理，不标记为视频
	return &MediaContent{
		Base64Data: base64.StdEncoding.EncodeToString(data),
		MIMEType:   mimeType,
		IsVideo:    false,
	}
}
