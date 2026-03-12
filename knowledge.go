package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// KnowledgeBase 管理知识库文档
type KnowledgeBase struct {
	dir     string // 知识库目录路径
	content string // 合并后的知识库文本
	mu      sync.RWMutex
}

// NewKnowledgeBase 创建知识库实例
func NewKnowledgeBase(dir string) *KnowledgeBase {
	if dir == "" {
		dir = "config/knowledge"
	}
	return &KnowledgeBase{
		dir: dir,
	}
}

// Load 从目录加载所有知识库文件（.md 和 .txt）
func (kb *KnowledgeBase) Load() error {
	kb.mu.Lock()
	defer kb.mu.Unlock()

	// 确保目录存在
	if err := os.MkdirAll(kb.dir, 0755); err != nil {
		return fmt.Errorf("创建知识库目录失败: %w", err)
	}

	var parts []string
	totalSize := 0
	const maxTotalSize = 50 * 1024 // 知识库总大小限制 50KB，避免 token 超限
	const maxFileSize = 20 * 1024  // 单文件最大 20KB

	err := filepath.Walk(kb.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".txt" {
			return nil
		}

		// 检查单文件大小
		if info.Size() > int64(maxFileSize) {
			log.Printf("[知识库] 跳过过大的文件: %s (%.1fKB > %dKB)", path, float64(info.Size())/1024, maxFileSize/1024)
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[知识库] 读取文件失败: %s: %v", path, err)
			return nil
		}

		content := strings.TrimSpace(string(data))
		if content == "" {
			return nil
		}

		// 检查总大小
		if totalSize+len(content) > maxTotalSize {
			log.Printf("[知识库] 知识库总量已达上限 (%dKB)，跳过: %s", maxTotalSize/1024, path)
			return nil
		}

		// 用文件名作为标题
		fileName := strings.TrimSuffix(filepath.Base(path), ext)
		section := fmt.Sprintf("## %s\n\n%s", fileName, content)
		parts = append(parts, section)
		totalSize += len(content)

		log.Printf("[知识库] 加载文件: %s (%.1fKB)", filepath.Base(path), float64(len(content))/1024)
		return nil
	})

	if err != nil {
		return fmt.Errorf("遍历知识库目录失败: %w", err)
	}

	if len(parts) > 0 {
		kb.content = "# 知识库\n\n以下是你的参考资料，请在回答相关问题时优先参考这些内容：\n\n" + strings.Join(parts, "\n\n---\n\n")
		log.Printf("[知识库] 共加载 %d 个文件，总计 %.1fKB", len(parts), float64(totalSize)/1024)
	} else {
		kb.content = ""
		log.Printf("[知识库] 知识库目录为空或无有效文件: %s", kb.dir)
	}

	return nil
}

// GetContent 获取格式化的知识库内容（用于注入 system prompt）
func (kb *KnowledgeBase) GetContent() string {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	return kb.content
}

// Reload 热重载知识库
func (kb *KnowledgeBase) Reload() error {
	log.Printf("[知识库] 开始重新加载...")
	return kb.Load()
}

// AddEntry 添加或覆盖一个知识库条目
func (kb *KnowledgeBase) AddEntry(name, content string) error {
	// 简单的防御性检查，防止路径穿越
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		return fmt.Errorf("无效的条目名称")
	}

	// 强制添加 .md 后缀
	if !strings.HasSuffix(strings.ToLower(name), ".md") && !strings.HasSuffix(strings.ToLower(name), ".txt") {
		name += ".md"
	}

	filePath := filepath.Join(kb.dir, name)

	// 写入文件
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("写入知识库文件失败: %w", err)
	}

	// 写入成功后触发重载
	return kb.Reload()
}

// MergeEntry 智能合并知识库条目
// 若文件已存在，将新内容与旧内容通过 AI 进行语义合并和去重
// 若文件不存在，创建新文件
// 返回 (isNew bool, err error)
func (kb *KnowledgeBase) MergeEntry(name, newContent string, aiClient *AIClient) (bool, error) {
	// 防御性检查，防止路径穿越
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		return false, fmt.Errorf("无效的条目名称")
	}

	// 强制添加 .md 后缀
	if !strings.HasSuffix(strings.ToLower(name), ".md") && !strings.HasSuffix(strings.ToLower(name), ".txt") {
		name += ".md"
	}

	filePath := filepath.Join(kb.dir, name)

	// 检查文件是否已存在
	existingData, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在，创建新文件
			if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
				return false, fmt.Errorf("写入知识库文件失败: %w", err)
			}
			kb.Reload()
			return true, nil
		}
		return false, fmt.Errorf("读取知识库文件失败: %w", err)
	}

	// 文件已存在，尝试 AI 合并
	oldContent := string(existingData)
	mergedContent, err := mergeWithAI(aiClient, oldContent, newContent)
	if err != nil {
		// AI 合并失败，回退到简单文本追加
		log.Printf("[知识库] AI 合并失败，回退到文本追加: %v", err)
		mergedContent = oldContent + "\n\n---\n\n" + newContent
	}

	if err := os.WriteFile(filePath, []byte(mergedContent), 0644); err != nil {
		return false, fmt.Errorf("写入合并后内容失败: %w", err)
	}
	kb.Reload()
	return false, nil
}

// mergeWithAI 使用 AI 对新旧内容进行语义合并和去重
func mergeWithAI(aiClient *AIClient, oldContent, newContent string) (string, error) {
	if aiClient == nil {
		return "", fmt.Errorf("AI 客户端未初始化")
	}

	prompt := fmt.Sprintf("你是一个知识库合并助手。请将以下【已有内容】和【新增内容】进行智能合并：\n"+
		"1. 去除完全重复的语句\n"+
		"2. 如果新旧内容描述同一属性但值不同（语义冲突），以【新增内容】为准\n"+
		"3. 保持内容结构清晰，使用条理化格式\n"+
		"4. 直接输出合并后的完整内容，不要包含任何前言或解释\n\n"+
		"【已有内容】：\n%s\n\n【新增内容】：\n%s", oldContent, newContent)

	msg, err := aiClient.ChatCompletion([]ChatMessage{
		{Role: "system", Content: "你是一个专业的知识库合并引擎，负责将新旧内容智能合并。"},
		{Role: "user", Content: prompt},
	}, nil)
	if err != nil {
		return "", err
	}

	result := strings.TrimSpace(msg.Content)
	if result == "" {
		return "", fmt.Errorf("AI 返回空内容")
	}
	return result, nil
}

// DeleteEntry 删除一个知识库条目
func (kb *KnowledgeBase) DeleteEntry(name string) error {
	name = filepath.Base(name)
	if name == "" {
		return fmt.Errorf("无效的条目名称")
	}

	// 尝试匹配带后缀或不带后缀的文件
	possiblePaths := []string{
		filepath.Join(kb.dir, name),
		filepath.Join(kb.dir, name+".md"),
		filepath.Join(kb.dir, name+".txt"),
	}

	deleted := false
	for _, p := range possiblePaths {
		if _, err := os.Stat(p); err == nil {
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("删除文件失败: %w", err)
			}
			deleted = true
			break
		}
	}

	if !deleted {
		return fmt.Errorf("未找到该条目文件")
	}

	// 删除成功后触发重载
	return kb.Reload()
}

// ListEntries 获取当前所有知识库条目名称及大小
func (kb *KnowledgeBase) ListEntries() ([]string, error) {
	var entries []string

	err := filepath.Walk(kb.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".md" || ext == ".txt" {
			entries = append(entries, fmt.Sprintf("- `%s` (%.1f KB)", info.Name(), float64(info.Size())/1024))
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("读取目录失败: %w", err)
	}
	return entries, nil
}
