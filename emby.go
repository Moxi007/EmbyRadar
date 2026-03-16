package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

// EmbyMediaStream 表示 Emby 媒体流信息（视频、音频等）
type EmbyMediaStream struct {
	Type         string `json:"Type"`         // 流类型，如 "Video"、"Audio"
	DisplayTitle string `json:"DisplayTitle"` // 显示标题，如 "4K HEVC"
	Width        int    `json:"Width"`        // 视频宽度
	Height       int    `json:"Height"`       // 视频高度
}

// EmbyMediaSource 表示 Emby 媒体源信息
type EmbyMediaSource struct {
	Name         string            `json:"Name"`
	Path         string            `json:"Path"`
	Bitrate      int               `json:"Bitrate"`
	Container    string            `json:"Container"`
	MediaStreams []EmbyMediaStream `json:"MediaStreams"`
}

// EmbyMediaItem 表示 Emby 媒体库中的一个条目
type EmbyMediaItem struct {
	Name         string            `json:"Name"`
	Year         int               `json:"ProductionYear"`
	Type         string            `json:"Type"` // "Movie" 或 "Series"
	MediaSources []EmbyMediaSource `json:"MediaSources"`
}

// embyItemsResponse 用于反序列化 Emby /Items 端点的响应
type embyItemsResponse struct {
	Items []EmbyMediaItem `json:"Items"`
}

// GetResolution 从 MediaStreams 中提取视频分辨率描述
// 遍历所有 MediaSource 的 MediaStream，找到第一个 Video 类型的流并根据宽高返回分辨率
// 无视频流时返回 "未知"
func (item *EmbyMediaItem) GetResolution() string {
	for _, source := range item.MediaSources {
		for _, stream := range source.MediaStreams {
			if stream.Type == "Video" {
				return resolveResolution(stream.Width, stream.Height)
			}
		}
	}
	return "未知"
}

// resolveResolution 根据视频宽高返回分辨率描述
func resolveResolution(width, height int) string {
	switch {
	case width >= 3840 || height >= 2160:
		return "4K"
	case width >= 1920 || height >= 1080:
		return "1080p"
	case width >= 1280 || height >= 720:
		return "720p"
	case width >= 720 || height >= 480:
		return "480p"
	case width > 0 || height > 0:
		return fmt.Sprintf("%dx%d", width, height)
	default:
		return "未知"
	}
}

// FormatMediaInfo 将 Emby 媒体条目格式化为用户可读的文本
// 包含名称、年份和分辨率信息
func (item *EmbyMediaItem) FormatMediaInfo() string {
	resolution := item.GetResolution()
	if item.Year > 0 {
		return fmt.Sprintf("%s（%d）[%s]", item.Name, item.Year, resolution)
	}
	return fmt.Sprintf("%s [%s]", item.Name, resolution)
}

// SearchMedia 在 Emby 媒体库中搜索影视资源
// 调用 /Items 端点，通过 SearchTerm 参数搜索电影和电视剧
// 返回匹配的条目列表，包含标题、年份、分辨率等信息
func (ec *EmbyClient) SearchMedia(query string) ([]EmbyMediaItem, error) {
	reqURL := fmt.Sprintf("%s/Items?SearchTerm=%s&IncludeItemTypes=Movie,Series&Recursive=true&Fields=MediaSources&api_key=%s",
		ec.URL, url.QueryEscape(query), ec.APIKey)

	resp, err := ec.Client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("搜索 Emby 媒体库失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Emby API 响应非 200: %d", resp.StatusCode)
	}

	var result embyItemsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 Emby 搜索结果失败: %w", err)
	}

	return result.Items, nil
}

// SearchByProviderID 通过 TMDB ID 和媒体类型在 Emby 媒体库中精确查找资源
// 使用 AnyProviderIdEquals 参数按 ProviderID 精确匹配，避免标题模糊搜索的误判
// mediaType 为 "movie" 或 "tv"，对应 Emby 的 Movie 和 Series 类型
func (ec *EmbyClient) SearchByProviderID(tmdbID int, mediaType string) ([]EmbyMediaItem, error) {
	// 将求片的 mediaType 映射为 Emby 的 IncludeItemTypes
	embyType := "Movie"
	if mediaType == "tv" {
		embyType = "Series"
	}

	reqURL := fmt.Sprintf("%s/Items?AnyProviderIdEquals=Tmdb.%d&IncludeItemTypes=%s&Recursive=true&Fields=MediaSources,ProviderIds&api_key=%s",
		ec.URL, tmdbID, embyType, ec.APIKey)

	resp, err := ec.Client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("通过 TMDB ID 搜索 Emby 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Emby API 响应非 200: %d", resp.StatusCode)
	}

	var result embyItemsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 Emby 搜索结果失败: %w", err)
	}

	return result.Items, nil
}

// GetLatestMedia 获取最新入库的影视资源
// 通过限制 Limit 并按 DateCreated 降序排，获取最新的电视剧和电影
func (ec *EmbyClient) GetLatestMedia(limit int) ([]EmbyMediaItem, error) {
	if limit <= 0 {
		limit = 10
	}
	reqURL := fmt.Sprintf("%s/Items?SortBy=DateCreated&SortOrder=Descending&Limit=%d&IncludeItemTypes=Movie,Series&Recursive=true&Fields=MediaSources&api_key=%s",
		ec.URL, limit, ec.APIKey)

	resp, err := ec.Client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("获取最新入库资源失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Emby API 响应非 200: %d", resp.StatusCode)
	}

	var result embyItemsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析最新资源结果失败: %w", err)
	}

	return result.Items, nil
}

// GetUserPlayback 获取指定用户最近观看的媒体
// embyUserID 为用户在 Emby 内的 UUID（可通过 EmbyBoss 获取）
func (ec *EmbyClient) GetUserPlayback(embyUserID string, limit int) ([]EmbyMediaItem, error) {
	if limit <= 0 {
		limit = 10
	}
	// 按最新播放时间排序（无论是否看完）
	reqURL := fmt.Sprintf("%s/Users/%s/Items?SortBy=DatePlayed&SortOrder=Descending&Limit=%d&IncludeItemTypes=Movie,Series&Recursive=true&Fields=MediaSources&api_key=%s",
		ec.URL, embyUserID, limit, ec.APIKey)

	resp, err := ec.Client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("获取用户观看历史失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Emby API 响应非 200: %d", resp.StatusCode)
	}

	var result embyItemsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析用户观影历史失败: %w", err)
	}

	return result.Items, nil
}

// UserIPDeviceStats 记录去重后的统计信息
type UserIPDeviceStats struct {
	UniqueIPs      int
	UniqueDevices  int
	IPList         []string
	DeviceList     []string
}

type playbackReportingCustomQueryReq struct {
	CustomQueryString string `json:"CustomQueryString"`
	ReplaceUserId     bool   `json:"ReplaceUserId"`
}

type playbackReportingQueryResult struct {
	Columns []string   `json:"columns"`
	Colums  []string   `json:"colums"` // 兼容拼写错误
	Results [][]string `json:"results"`
}

// GetUserIPAndDeviceStats 使用 Playback Reporting 插件直接发送 SQL 拉取对应天数内的播放活动数据以统计 IP/设备数
func (ec *EmbyClient) GetUserIPAndDeviceStats(embyUserID string, days int) (*UserIPDeviceStats, error) {
	if days <= 0 {
		days = 1 // 默认至少查 1 天（最近 24 小时）
	}
	
	// 构建原生的 SQLite 查询注入
	queryStr := fmt.Sprintf("SELECT IpAddress, DeviceName FROM PlaybackActivity WHERE UserId = '%s' AND DateCreated >= datetime('now', '-%d days')", embyUserID, days)
	
	payload := playbackReportingCustomQueryReq{
		CustomQueryString: queryStr,
		ReplaceUserId:     false,
	}
	
	bodyData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("构建查询 Payload 失败: %w", err)
	}
	
	reqURL := fmt.Sprintf("%s/user_usage_stats/submit_custom_query?api_key=%s", ec.URL, ec.APIKey)
	req, err := http.NewRequest("POST", reqURL, bytes.NewBuffer(bodyData))
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := ec.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Playback Reporting 接口失败: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Playback Reporting API 错误，可能未安装该插件: %d", resp.StatusCode)
	}
	
	var pbResult playbackReportingQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&pbResult); err != nil {
		return nil, fmt.Errorf("解析结果失败: %w", err)
	}
	
	ipMap := make(map[string]bool)
	deviceMap := make(map[string]bool)
	
	for _, row := range pbResult.Results {
		if len(row) >= 2 {
			ip := row[0]
			device := row[1]
			if ip != "" {
				ipMap[ip] = true
			}
			if device != "" {
				deviceMap[device] = true
			}
		}
	}
	
	stats := &UserIPDeviceStats{
		UniqueIPs:     len(ipMap),
		UniqueDevices: len(deviceMap),
		IPList:        make([]string, 0, len(ipMap)),
		DeviceList:    make([]string, 0, len(deviceMap)),
	}
	
	for ip := range ipMap {
		stats.IPList = append(stats.IPList, ip)
	}
	for dev := range deviceMap {
		stats.DeviceList = append(stats.DeviceList, dev)
	}
	
	return stats, nil
}
