package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// EmbyClient 封装了对 Emby API 的调用
type EmbyClient struct {
	URL    string
	APIKey string
	Client *http.Client
}

// NewEmbyClient 创建一个新的 EmbyClient 实例
func NewEmbyClient(url, apiKey string) *EmbyClient {
	return &EmbyClient{
		URL:    url,
		APIKey: apiKey,
		Client: &http.Client{
			Timeout: 10 * time.Second, // 设置 10 秒超时
		},
	}
}

// GetActiveSessions 获取当前正在播放/有活动的 Session 数量
func (ec *EmbyClient) GetActiveSessions() (int, error) {
	// 获取所有的会话
	reqURL := fmt.Sprintf("%s/Sessions?api_key=%s", ec.URL, ec.APIKey)
	resp, err := ec.Client.Get(reqURL)
	if err != nil {
		return 0, fmt.Errorf("获取 Sessions 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Emby API 响应非 200: %d", resp.StatusCode)
	}

	var sessions []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return 0, fmt.Errorf("解析 Sessions JSON 失败: %w", err)
	}

	activeCount := 0
	for _, session := range sessions {
		// 检查是否有 NowPlayingItem，表示正在播放
		if _, ok := session["NowPlayingItem"]; ok {
			// 如果有 PlayState 且 IsPaused 为 false，说明正在播放（也可以把暂停计作观看，看个人需求）
			// 这里我们简单起见，只要有 NowPlayingItem 就算在观看
			activeCount++
		}
	}

	return activeCount, nil
}

// GetTotalUsers 获取 Emby 系统中的总用户数量
func (ec *EmbyClient) GetTotalUsers() (int, error) {
	reqURL := fmt.Sprintf("%s/Users?api_key=%s", ec.URL, ec.APIKey)
	resp, err := ec.Client.Get(reqURL)
	if err != nil {
		return 0, fmt.Errorf("获取 Users 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Emby API 响应非 200: %d", resp.StatusCode)
	}

	var users []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return 0, fmt.Errorf("解析 Users JSON 失败: %w", err)
	}

	return len(users), nil
}

// GetServerName 从 Emby 获取服务器名称
func (ec *EmbyClient) GetServerName() (string, error) {
	reqURL := fmt.Sprintf("%s/System/Info?api_key=%s", ec.URL, ec.APIKey)
	resp, err := ec.Client.Get(reqURL)
	if err != nil {
		return "", fmt.Errorf("获取服务器信息失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Emby API 响应非 200: %d", resp.StatusCode)
	}

	var info map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("解析服务器信息失败: %w", err)
	}

	if name, ok := info["ServerName"].(string); ok && name != "" {
		return name, nil
	}
	return "EMBY", nil
}
