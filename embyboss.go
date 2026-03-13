package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
		Tg     json.Number `json:"tg"`     // TG ID，可能是数字或字符串
		Iv     int         `json:"iv"`     // 积分/货币余额
		Name   string      `json:"name"`   // Emby 用户名
		EmbyID string      `json:"embyid"` // Emby 账号 ID
		Lv     string      `json:"lv"`     // 等级/状态, 如 'c' 代表封禁
		Cr     string      `json:"cr"`     // 创建时间等
		Ex     string      `json:"ex"`     // 过期时间
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
	log.Printf("[EmbyBoss] 正在请求: %s/user/user_info?tg=%d&token=***", c.BaseURL, tgID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		log.Printf("[EmbyBoss] 网络请求失败: %v", err)
		return nil, fmt.Errorf("请求 EmbyBoss 失败 (请检查服务是否启动及端口配置): %w", err)
	}
	defer resp.Body.Close()

	log.Printf("[EmbyBoss] HTTP 状态码: %d", resp.StatusCode)

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	log.Printf("[EmbyBoss] 原始响应: %s", string(bodyBytes))

	// 如果 HTTP 状态码本身不是 200，说明鉴权失败或路由错
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("EmbyBoss HTTP 错误 (状态码 %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var userInfo UserInfoResponse
	if err := json.Unmarshal(bodyBytes, &userInfo); err != nil {
		return nil, fmt.Errorf("解析 EmbyBoss 响应失败: %w", err)
	}

	if userInfo.Code != 200 {
		log.Printf("[EmbyBoss] 业务层返回非200: code=%d, message=%s", userInfo.Code, userInfo.Message)
		return nil, fmt.Errorf("EmbyBoss API 返回错误或无该用户数据: %s", userInfo.Message)
	}

	log.Printf("[EmbyBoss] 成功获取用户数据: name=%s, iv=%d, lv=%s", userInfo.Data.Name, userInfo.Data.Iv, userInfo.Data.Lv)
	return &userInfo, nil
}

// DeductCoins 扣除指定用户的货币
// 调用 EmbyBoss 的 /user/deduct 接口，参数通过 query string 传递
func (c *EmbyBossClient) DeductCoins(tgID int64, amount int, reason string) error {
	reqURL := fmt.Sprintf("%s/user/deduct?tg=%d&amount=%d&reason=%s&token=%s",
		c.BaseURL, tgID, amount, url.QueryEscape(reason), c.APIToken)
	log.Printf("[EmbyBoss] 正在扣除货币: tg=%d, amount=%d, reason=%s", tgID, amount, reason)

	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		return fmt.Errorf("创建扣币请求失败: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		log.Printf("[EmbyBoss] 扣币网络请求失败: %v", err)
		return fmt.Errorf("请求 EmbyBoss 扣币接口失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取扣币响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("EmbyBoss 扣币接口 HTTP 错误 (状态码 %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return fmt.Errorf("解析扣币响应失败: %w", err)
	}

	if result.Code != 200 {
		return fmt.Errorf("EmbyBoss 扣币失败: %s", result.Message)
	}

	log.Printf("[EmbyBoss] 成功扣除货币: tg=%d, amount=%d", tgID, amount)
	return nil
}

// RefundCoins 退还指定用户的货币
// 调用 EmbyBoss 的 /user/refund 接口，参数通过 query string 传递
func (c *EmbyBossClient) RefundCoins(tgID int64, amount int, reason string) error {
	reqURL := fmt.Sprintf("%s/user/refund?tg=%d&amount=%d&reason=%s&token=%s",
		c.BaseURL, tgID, amount, url.QueryEscape(reason), c.APIToken)
	log.Printf("[EmbyBoss] 正在退还货币: tg=%d, amount=%d, reason=%s", tgID, amount, reason)

	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		return fmt.Errorf("创建退币请求失败: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		log.Printf("[EmbyBoss] 退币网络请求失败: %v", err)
		return fmt.Errorf("请求 EmbyBoss 退币接口失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取退币响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("EmbyBoss 退币接口 HTTP 错误 (状态码 %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return fmt.Errorf("解析退币响应失败: %w", err)
	}

	if result.Code != 200 {
		return fmt.Errorf("EmbyBoss 退币失败: %s", result.Message)
	}

	log.Printf("[EmbyBoss] 成功退还货币: tg=%d, amount=%d", tgID, amount)
	return nil
}

// ConfigResponse 对应 EmbyBoss /bot/config 接口的返回结构
type ConfigResponse struct {
	Code         int    `json:"code"`
	Message      string `json:"message"`
	CurrencyName string `json:"currency_name"` // 服务端配置的货币名称
}

// GetCurrencyName 从 EmbyBoss 服务端动态获取货币名称。
// 调用 /bot/config 端点，解析返回的 currency_name 字段。
// 请求失败或返回异常时返回 error，供调用方 fallback 到本地配置。
func (c *EmbyBossClient) GetCurrencyName() (string, error) {
	url := fmt.Sprintf("%s/bot/config?token=%s", c.BaseURL, c.APIToken)
	log.Printf("[EmbyBoss] 正在请求货币名称配置: %s/bot/config?token=***", c.BaseURL)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		log.Printf("[EmbyBoss] 获取货币名称失败（网络错误）: %v", err)
		return "", fmt.Errorf("请求 EmbyBoss 配置接口失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("EmbyBoss 配置接口 HTTP 错误 (状态码 %d): %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取配置响应失败: %w", err)
	}

	var configResp ConfigResponse
	if err := json.Unmarshal(bodyBytes, &configResp); err != nil {
		return "", fmt.Errorf("解析配置响应失败: %w", err)
	}

	if configResp.Code != 200 {
		return "", fmt.Errorf("EmbyBoss 配置接口返回错误: %s", configResp.Message)
	}

	if configResp.CurrencyName == "" {
		return "", fmt.Errorf("EmbyBoss 配置接口返回的货币名称为空")
	}

	log.Printf("[EmbyBoss] 成功获取服务端货币名称: %s", configResp.CurrencyName)
	return configResp.CurrencyName, nil
}

// FormatForAI 将用户数据格式化为适合 AI 理解的自然语言
func (r *UserInfoResponse) FormatForAI(currencyName string) string {
	status := "正常"
	if r.Data.Lv == "c" {
		status = "封禁状态(被禁止登录)"
	}

	return fmt.Sprintf(
		"【内部系统数据查询结果】：当前对话者的系统绑定账号名为「%s」，目前的可用资产余额为 %d %s。其账号当前所处状态判定为「%s」，此账号的过期时间戳记录为 %s。请在回答时根据这些准确数据为其解答疑问，不可凭空捏造数据。注意：货币单位必须严格使用「%s」，禁止自行替换为其他名称。",
		r.Data.Name,
		r.Data.Iv,
		currencyName,
		status,
		r.Data.Ex,
		currencyName,
	)
}
