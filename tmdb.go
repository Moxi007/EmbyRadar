package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

// FormatTMDBResultsForAI 将搜索结果格式化为 AI 可读的文本
// 每条结果包含序号、标题、年份、评分、简介
func FormatTMDBResultsForAI(results []TMDBSearchResult) string {
	if len(results) == 0 {
		return "未找到相关影视信息。"
	}

	var sb strings.Builder
	for i, r := range results {
		title := r.GetDisplayTitle()
		year := r.GetYear()
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, title))
		if year != "" {
			sb.WriteString(fmt.Sprintf("（%s）", year))
		}
		sb.WriteString(fmt.Sprintf(" - 评分: %.1f", r.VoteAverage))
		if r.Overview != "" {
			// 简介过长时截断，避免占用过多 token
			overview := r.Overview
			if len([]rune(overview)) > 100 {
				overview = string([]rune(overview)[:100]) + "..."
			}
			sb.WriteString(fmt.Sprintf("\n   简介: %s", overview))
		}
		sb.WriteString(fmt.Sprintf("\n   TMDB: %s", r.GetTMDBURL()))
		if i < len(results)-1 {
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}
