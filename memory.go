package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// MemoryResult 表示一条检索出的记忆
type MemoryResult struct {
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	UserName  string `json:"user_name"`
	Score     float32
}

// MemoryStore 向量记忆存储，封装 Qdrant 和自定义 Embedding API 调用
type MemoryStore struct {
	qdrantURL       string
	embeddingAPIURL string
	embeddingAPIKey string
	embeddingModel  string
	collectionName  string
	dimension       int
	httpClient      *http.Client
}

// NewMemoryStore 创建记忆存储客户端
func NewMemoryStore(cfg *GlobalConfig) *MemoryStore {
	return &MemoryStore{
		qdrantURL:       cfg.QdrantURL,
		embeddingAPIURL: cfg.EmbeddingAPIURL,
		embeddingAPIKey: cfg.EmbeddingAPIKey,
		embeddingModel:  cfg.EmbeddingModel,
		collectionName:  "embyradar_memory",
		dimension:       1024, // 默认 1024，实际会通过测试一次请求动态获取或固定（如 bge-m3 是 1024）
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// EnsureCollection 确保 Qdrant 中的集合存在
func (ms *MemoryStore) EnsureCollection() error {
	// 如果配置了，先获取一次嵌入维度
	if ms.embeddingAPIURL != "" {
		vec, err := ms.embedText("test")
		if err == nil && len(vec) > 0 {
			ms.dimension = len(vec)
			log.Printf("[记忆] 成功测算出 Embedding 维度为: %d", ms.dimension)
		} else {
			log.Printf("[记忆] 提示：无法连接 Embedding API 测算维度，将使用默认维度 1024: %v", err)
		}
	}

	url := fmt.Sprintf("%s/collections/%s", ms.qdrantURL, ms.collectionName)
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := ms.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("检查 Qdrant 集合失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		return nil // 集合已存在
	}

	// 否则创建结合
	createReq := map[string]any{
		"vectors": map[string]any{
			"size":     ms.dimension,
			"distance": "Cosine",
		},
	}
	body, _ := json.Marshal(createReq)
	req, _ = http.NewRequest("PUT", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err = ms.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("创建 Qdrant 集合请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("创建 Qdrant 集合失败(Status=%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// embedText 调用配置的 Embedding API (兼容 OpenAI /v1/embeddings 格式)
func (ms *MemoryStore) embedText(text string) ([]float32, error) {
	if ms.embeddingAPIURL == "" {
		return nil, fmt.Errorf("未配置 Embedding API URL")
	}

	// 构造 OpenAI 兼容的 Embedding 请求
	reqData := map[string]any{
		"input": text,
		"model": ms.embeddingModel,
	}
	body, _ := json.Marshal(reqData)

	url := fmt.Sprintf("%s/embeddings", ms.embeddingAPIURL)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if ms.embeddingAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ms.embeddingAPIKey)
	}

	resp, err := ms.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Embedding API 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Embedding API 响应错误(HTTP %d): %s", resp.StatusCode, string(b))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 Embedding 响应失败: %w", err)
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("Embedding 响应数据为空")
	}
	return result.Data[0].Embedding, nil
}

// Store 将文本存储到 Qdrant 
func (ms *MemoryStore) Store(chatID int64, text string, metadata map[string]any) error {
	vec, err := ms.embedText(text)
	if err != nil {
		return fmt.Errorf("生成向量失败: %w", err)
	}

	// 生成唯一 ID（简单用时间戳+chatID 的哈希）或者直接随机 UUID（这里使用自增的 timestamp 以简化）
	pointID := time.Now().UnixNano()

	payload := map[string]any{
		"text":    text,
		"chat_id": chatID,
	}
	// 合并额外的元数据
	for k, v := range metadata {
		payload[k] = v
	}

	qdrantReq := map[string]any{
		"points": []map[string]any{
			{
				"id":      pointID,
				"vector":  vec,
				"payload": payload,
			},
		},
	}

	body, _ := json.Marshal(qdrantReq)
	url := fmt.Sprintf("%s/collections/%s/points?wait=true", ms.qdrantURL, ms.collectionName)
	req, _ := http.NewRequest("PUT", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := ms.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("写入 Qdrant 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("写入 Qdrant 失败(HTTP %d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// Search 检索相似片段
func (ms *MemoryStore) Search(chatID int64, queryText string, topK int) ([]MemoryResult, error) {
	vec, err := ms.embedText(queryText)
	if err != nil {
		return nil, fmt.Errorf("检索生成向量失败: %w", err)
	}

	searchReq := map[string]any{
		"vector": vec,
		"limit":  topK,
		"with_payload": true,
		"filter": map[string]any{
			"must": []map[string]any{
				{
					"key": "chat_id",
					"match": map[string]any{
						"value": chatID,
					},
				},
			},
		},
	}

	body, _ := json.Marshal(searchReq)
	url := fmt.Sprintf("%s/collections/%s/points/search", ms.qdrantURL, ms.collectionName)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := ms.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Qdrant 搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Qdrant 搜索失败(HTTP %d): %s", resp.StatusCode, string(b))
	}

	var qdrantResp struct {
		Result []struct {
			Score   float32        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&qdrantResp); err != nil {
		return nil, fmt.Errorf("解析 Qdrant 搜索响应失败: %w", err)
	}

	var results []MemoryResult
	// 简单的分数过滤机制，低于某个相似度则不要
	const threshold float32 = 0.5

	for _, hit := range qdrantResp.Result {
		if hit.Score < threshold {
			continue
		}
		
		text, _ := hit.Payload["text"].(string)
		ts, _ := hit.Payload["timestamp"].(string)
		uName, _ := hit.Payload["user_name"].(string)
		
		results = append(results, MemoryResult{
			Text:      text,
			Timestamp: ts,
			UserName:  uName,
			Score:     hit.Score,
		})
	}

	return results, nil
}
