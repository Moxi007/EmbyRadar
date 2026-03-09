package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EmbyBossClient 用于与本地 EmbyBoss FastAPI 交互
type EmbyBossClient struct {
	BaseURL    string
	APIToken   string // EmbyBoss 的鉴权 Token（即 EmbyBoss 的 bot_token）
	HTTPClient *http.Client
}

// UserInfoResponse 对应 EmbyBoss /user_info 接口的返回结构
type UserInfoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Tg     string `json:"tg"`
		Iv     int    `json:"iv"`     // 积分/花币
		Name   string `json:"name"`   // Emby 用户名
		EmbyID string `json:"embyid"` // Emby 账号 ID
		Lv     string `json:"lv"`     // 等级/状态, 如 'c' 代表封禁
		Cr     string `json:"cr"`     // 创建时间等
		Ex     string `json:"ex"`     // 过期时间
	} `json:"data"`
}

// NewEmbyBossClient 创建一个新的 EmbyBoss 客户端实例
func NewEmbyBossClient(baseURL string, apiToken string) *EmbyBossClient {
	return &EmbyBossClient{
		BaseURL:  baseURL,
		APIToken: apiToken,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second, // 内网请求，5秒足够
		},
	}
}

// GetUserInfo 根据 TG ID 查询用户在 EmbyBoss 中的信息
func (c *EmbyBossClient) GetUserInfo(tgID int64) (*UserInfoResponse, error) {
	url := fmt.Sprintf("%s/user/user_info?tg=%d&token=%s", c.BaseURL, tgID, c.APIToken)
	
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 EmbyBoss 失败 (请检查服务是否启动及端口配置): %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var userInfo UserInfoResponse
	if err := json.Unmarshal(bodyBytes, &userInfo); err != nil {
		return nil, fmt.Errorf("解析 EmbyBoss 响应失败: %w", err)
	}

	if userInfo.Code != 200 {
		return nil, fmt.Errorf("EmbyBoss API 返回错误或无该用户数据: %s", userInfo.Message)
	}

	return &userInfo, nil
}

// FormatUserInfo 将用户数据格式化为适合 AI 理解的自然语言
func (r *UserInfoResponse) FormatForAI() string {
	status := "正常"
	if r.Data.Lv == "c" {
		status = "封禁状态(被禁止登录)"
	}
	
	return fmt.Sprintf(
		"【内部系统数据查询结果】：当前对话者的系统绑定账号名为“%s”，目前的可用资产余额为 %d 花币/积分。其账号当前所处状态判定为“%s”，此账号的过期时间戳记录为 %s。请在回答时根据这些准确数据为其解答疑问，不可凭空捏造数据。",
		r.Data.Name,
		r.Data.Iv,
		status,
		r.Data.Ex,
	)
}
