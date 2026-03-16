package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TMDBClient 封装 TMDB API v3 调用，提供影视搜索能力
type TMDBClient struct {
	APIKey     string
	BaseURL    string       // 默认 "https://api.themoviedb.org/3"
	HTTPClient *http.Client // 10 秒超时
}

// TMDBSearchResult 单条 TMDB 搜索结果
type TMDBSearchResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`          // 电影标题（movie 类型）
	Name          string  `json:"name"`           // 电视剧标题（tv 类型）
	OriginalTitle string  `json:"original_title"` // 电影原始标题
	OriginalName  string  `json:"original_name"`  // 电视剧原始标题
	Overview      string  `json:"overview"`       // 简介
	ReleaseDate   string  `json:"release_date"`   // 上映日期（movie）
	FirstAirDate  string  `json:"first_air_date"` // 首播日期（tv）
	VoteAverage   float64 `json:"vote_average"`   // 评分
	PosterPath    string  `json:"poster_path"`    // 海报路径
	MediaType     string  `json:"media_type"`     // "movie" 或 "tv"
}

// tmdbSearchResponse 用于反序列化 TMDB 搜索 API 的响应
type tmdbSearchResponse struct {
	Results []TMDBSearchResult `json:"results"`
}

// NewTMDBClient 创建 TMDB 客户端实例，使用默认的 BaseURL 和 10 秒超时
func NewTMDBClient(apiKey string) *TMDBClient {
	return &TMDBClient{
		APIKey:  apiKey,
		BaseURL: "https://api.themoviedb.org/3",
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetDisplayTitle 返回统一的显示标题
// 电影使用 Title 字段，电视剧使用 Name 字段
func (r *TMDBSearchResult) GetDisplayTitle() string {
	if r.MediaType == "tv" {
		return r.Name
	}
	// 默认返回 Title（movie 类型或 multi 搜索中的电影结果）
	if r.Title != "" {
		return r.Title
	}
	return r.Name
}

// GetYear 从日期字段提取前 4 位年份
// 电影取 ReleaseDate，电视剧取 FirstAirDate
func (r *TMDBSearchResult) GetYear() string {
	date := r.ReleaseDate
	if r.MediaType == "tv" {
		date = r.FirstAirDate
	}
	// 日期为空或长度不足时回退到另一个字段
	if len(date) < 4 {
		if r.MediaType == "tv" {
			date = r.ReleaseDate
		} else {
			date = r.FirstAirDate
		}
	}
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

// GetTMDBURL 返回 TMDB 页面链接
// 电影返回 /movie/{id}，电视剧返回 /tv/{id}
func (r *TMDBSearchResult) GetTMDBURL() string {
	mediaPath := "movie"
	if r.MediaType == "tv" {
		mediaPath = "tv"
	}
	return fmt.Sprintf("https://www.themoviedb.org/%s/%d", mediaPath, r.ID)
}

// SearchMulti 搜索电影和电视剧，返回最多 5 条结果
// mediaType 可选 "movie"、"tv" 或空字符串（使用 /search/multi 搜索全部）
func (tc *TMDBClient) SearchMulti(query, mediaType string) ([]TMDBSearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("搜索关键词不能为空")
	}

	// 根据 mediaType 选择对应的搜索端点
	var endpoint string
	switch mediaType {
	case "movie":
		endpoint = "/search/movie"
	case "tv":
		endpoint = "/search/tv"
	default:
		endpoint = "/search/multi"
	}

	reqURL := fmt.Sprintf("%s%s?api_key=%s&query=%s&language=zh-CN",
		tc.BaseURL, endpoint, tc.APIKey, url.QueryEscape(query))

	resp, err := tc.HTTPClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("TMDB 搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB API 响应非 200: %d", resp.StatusCode)
	}

	var searchResp tmdbSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("解析 TMDB 搜索结果失败: %w", err)
	}

	results := searchResp.Results

	// 使用特定类型端点搜索时，手动设置 MediaType 字段
	// 因为 /search/movie 和 /search/tv 的响应中不包含 media_type 字段
	if mediaType == "movie" || mediaType == "tv" {
		for i := range results {
			results[i].MediaType = mediaType
		}
	}

	// 限制最多返回 5 条结果
	if len(results) > 5 {
		results = results[:5]
	}

	return results, nil
}

// GetByID 通过 TMDB ID 和媒体类型直接获取影视详情
// mediaType 必须为 "movie" 或 "tv"
func (tc *TMDBClient) GetByID(id int, mediaType string) (*TMDBSearchResult, error) {
	if mediaType != "movie" && mediaType != "tv" {
		return nil, fmt.Errorf("无效的媒体类型: %s，仅支持 movie 或 tv", mediaType)
	}

	reqURL := fmt.Sprintf("%s/%s/%d?api_key=%s&language=zh-CN",
		tc.BaseURL, mediaType, id, tc.APIKey)

	resp, err := tc.HTTPClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("TMDB 详情请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB API 响应非 200: %d", resp.StatusCode)
	}

	var result TMDBSearchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 TMDB 详情失败: %w", err)
	}

	// 手动设置 MediaType，因为 details 端点响应中不含此字段
	result.MediaType = mediaType
	return &result, nil
}

// 预编译 TMDB 和豆瓣链接匹配正则
var (
	// 匹配 TMDB 链接：https://www.themoviedb.org/movie/12345 或 /tv/12345
	reTMDBURL = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?themoviedb\.org/(movie|tv)/(\d+)`)
	// 匹配豆瓣链接：https://movie.douban.com/subject/12345/ 或 douban.com/subject/12345
	reDoubanURL = regexp.MustCompile(`(?i)(?:https?://)?(?:movie\.)?douban\.com/subject/(\d+)`)
)

// TMDBLinkInfo 从 TMDB 链接中解析出的信息
type TMDBLinkInfo struct {
	ID        int    // TMDB ID
	MediaType string // "movie" 或 "tv"
}

// ParseTMDBURL 从文本中解析 TMDB 链接，提取媒体类型和 ID
// 返回 nil 表示文本中不含有效的 TMDB 链接
func ParseTMDBURL(text string) *TMDBLinkInfo {
	matches := reTMDBURL.FindStringSubmatch(text)
	if len(matches) != 3 {
		return nil
	}
	id, err := strconv.Atoi(matches[2])
	if err != nil {
		return nil
	}
	return &TMDBLinkInfo{
		ID:        id,
		MediaType: strings.ToLower(matches[1]),
	}
}

// ParseDoubanURL 从文本中解析豆瓣链接，提取 subject ID
// 返回 0 表示文本中不含有效的豆瓣链接
func ParseDoubanURL(text string) int {
	matches := reDoubanURL.FindStringSubmatch(text)
	if len(matches) != 2 {
		return 0
	}
	id, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	return id
}

// FetchDoubanTitle 通过 HTTP 抓取豆瓣影视页面的标题
// 从 <title> 标签中提取影视名称，用于后续 TMDB 搜索
func FetchDoubanTitle(doubanID int) (string, error) {
	pageURL := fmt.Sprintf("https://movie.douban.com/subject/%d/", doubanID)

	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建豆瓣请求失败: %w", err)
	}
	// 设置 User-Agent 避免被反爬拦截
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求豆瓣页面失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("豆瓣页面响应非 200: %d", resp.StatusCode)
	}

	// 只读取前 64KB，避免全量加载页面
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("读取豆瓣页面失败: %w", err)
	}

	// 从 HTML 中提取 <title> 标签内容
	// 豆瓣 title 格式通常为："影视名称 (豆瓣)" 或 "影视名称 第X季 (豆瓣)"
	reTitle := regexp.MustCompile(`<title[^>]*>([^<]+)</title>`)
	titleMatch := reTitle.FindSubmatch(body)
	if len(titleMatch) < 2 {
		return "", fmt.Errorf("未找到豆瓣页面标题")
	}

	title := strings.TrimSpace(string(titleMatch[1]))
	// 移除 " (豆瓣)" 后缀
	title = strings.TrimSuffix(title, " (豆瓣)")
	title = strings.TrimSpace(title)

	if title == "" {
		return "", fmt.Errorf("豆瓣页面标题为空")
	}

	return title, nil
}

// FormatTMDBResultsForAI 将搜索结果格式化为 AI 可读的文本
// 每条结果包含序号、标题、年份、评分、简介和 TMDB 链接
func FormatTMDBResultsForAI(results []TMDBSearchResult) string {
	if len(results) == 0 {
		return "未找到相关影视信息。"
	}

	var sb strings.Builder
	// 在开头加入强制指令，防止 AI 篡改链接
	sb.WriteString("⚠️ 重要：回复中引用影视信息时，必须直接复制粘贴下方提供的完整 TMDB 链接，严禁自行拼凑或修改链接中的任何部分！\n\n")

	for i, r := range results {
		title := r.GetDisplayTitle()
		year := r.GetYear()
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, title))
		if year != "" {
			sb.WriteString(fmt.Sprintf("（%s）", year))
		}
		sb.WriteString(fmt.Sprintf(" - TMDB评分: %.1f", r.VoteAverage))
		if r.Overview != "" {
			// 简介过长时截断，避免占用过多 token
			overview := r.Overview
			if len([]rune(overview)) > 100 {
				overview = string([]rune(overview)[:100]) + "..."
			}
			sb.WriteString(fmt.Sprintf("\n   简介: %s", overview))
		}
		sb.WriteString(fmt.Sprintf("\n   TMDB链接(请原样使用): %s", r.GetTMDBURL()))
		if i < len(results)-1 {
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString("\n\n📌 注意：以上评分为 TMDB 评分。如果需要向用户展示评分，请优先通过搜索工具获取豆瓣评分后展示给用户。")

	return sb.String()
}
