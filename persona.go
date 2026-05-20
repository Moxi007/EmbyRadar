package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultPersonaDir = "config/personas"

// PersonaProfile 表示一个可被群组选择的人格包。
// 人格只负责表达风格与角色背景，不承载工具 SOP、权限策略或事实知识。
type PersonaProfile struct {
	ID          string
	Name        string
	Description string
	Content     string
	SourcePath  string
}

// PersonaStore 从本地目录加载人格包，供 buildMessages 注入 system prompt。
type PersonaStore struct {
	dir      string
	profiles map[string]PersonaProfile
	mu       sync.RWMutex
}

var personaDirectoryFileOrder = []string{"IDENTITY.md", "USER.md", "SOUL.md"}

// NewPersonaStore 创建人格仓库实例。
func NewPersonaStore(dir string) *PersonaStore {
	if strings.TrimSpace(dir) == "" {
		dir = defaultPersonaDir
	}
	return &PersonaStore{
		dir:      dir,
		profiles: make(map[string]PersonaProfile),
	}
}

// Load 从目录加载 .md/.txt 人格文件。
func (ps *PersonaStore) Load() error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if err := os.MkdirAll(ps.dir, 0755); err != nil {
		return fmt.Errorf("创建人格目录失败: %w", err)
	}

	entries, err := os.ReadDir(ps.dir)
	if err != nil {
		return fmt.Errorf("遍历人格目录失败: %w", err)
	}

	nextProfiles := make(map[string]PersonaProfile)
	for _, entry := range entries {
		if entry.IsDir() {
			profile, ok := ps.loadDirectoryProfile(entry.Name())
			if ok {
				nextProfiles[profile.ID] = profile
				log.Printf("[人格] 加载目录人格包: %s (%s)", profile.ID, profile.Name)
			}
			continue
		}
		if strings.EqualFold(entry.Name(), "README.md") || strings.EqualFold(entry.Name(), "README.txt") {
			continue
		}

		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".md" && ext != ".txt" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			log.Printf("[人格] 获取文件信息失败: %s: %v", entry.Name(), err)
			continue
		}
		const maxPersonaFileSize = 32 * 1024
		if info.Size() > maxPersonaFileSize {
			log.Printf("[人格] 跳过过大的人格文件: %s (%.1fKB > %dKB)", entry.Name(), float64(info.Size())/1024, maxPersonaFileSize/1024)
			continue
		}

		path := filepath.Join(ps.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[人格] 读取人格文件失败: %s: %v", path, err)
			continue
		}

		id := strings.TrimSuffix(entry.Name(), ext)
		profile := parsePersonaProfile(id, path, string(data))
		if profile.Content == "" {
			log.Printf("[人格] 跳过空人格文件: %s", entry.Name())
			continue
		}
		nextProfiles[id] = profile
		log.Printf("[人格] 加载人格包: %s (%s)", profile.ID, profile.Name)
	}

	ps.profiles = nextProfiles
	if len(ps.profiles) == 0 {
		log.Printf("[人格] 人格目录为空或无有效文件: %s", ps.dir)
	} else {
		log.Printf("[人格] 共加载 %d 个人格包", len(ps.profiles))
	}
	return nil
}

func (ps *PersonaStore) loadDirectoryProfile(dirName string) (PersonaProfile, bool) {
	if filepath.Base(dirName) != dirName {
		return PersonaProfile{}, false
	}

	profileDir := filepath.Join(ps.dir, dirName)
	profile := PersonaProfile{
		ID:         dirName,
		Name:       dirName,
		SourcePath: profileDir,
	}

	var sections []string
	for _, fileName := range personaDirectoryFileOrder {
		path := filepath.Join(profileDir, fileName)
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[人格] 读取目录人格文件失败: %s: %v", path, err)
			}
			continue
		}

		part := parsePersonaProfile(dirName, path, string(data))
		if strings.TrimSpace(part.Name) != "" && part.Name != dirName {
			profile.Name = part.Name
		}
		if strings.TrimSpace(part.Description) != "" {
			profile.Description = part.Description
		}
		if strings.TrimSpace(part.Content) == "" {
			continue
		}

		sectionName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
		sections = append(sections, fmt.Sprintf("# %s\n\n%s", sectionName, strings.TrimSpace(part.Content)))
	}

	if len(sections) == 0 {
		return PersonaProfile{}, false
	}

	profile.Content = strings.Join(sections, "\n\n")
	return profile, true
}

// GetPrompt 获取指定人格的 system prompt 片段。
func (ps *PersonaStore) GetPrompt(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || filepath.Base(id) != id {
		return ""
	}

	ps.mu.RLock()
	defer ps.mu.RUnlock()

	profile, ok := ps.profiles[id]
	if !ok {
		return ""
	}
	return profile.FormatPrompt()
}

// HasProfile 检查指定人格包是否已加载。
func (ps *PersonaStore) HasProfile(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || filepath.Base(id) != id {
		return false
	}

	ps.mu.RLock()
	defer ps.mu.RUnlock()

	_, ok := ps.profiles[id]
	return ok
}

// FormatPrompt 将人格包格式化为明确边界的 prompt 片段。
func (p PersonaProfile) FormatPrompt() string {
	var parts []string
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = p.ID
	}
	parts = append(parts, fmt.Sprintf("[人格包: %s]", name))
	if strings.TrimSpace(p.Description) != "" {
		parts = append(parts, "简介："+strings.TrimSpace(p.Description))
	}
	parts = append(parts, strings.TrimSpace(p.Content))
	return strings.Join(parts, "\n\n")
}

func parsePersonaProfile(id, sourcePath, raw string) PersonaProfile {
	content := strings.TrimSpace(raw)
	profile := PersonaProfile{
		ID:         id,
		Name:       id,
		Content:    content,
		SourcePath: sourcePath,
	}
	if !strings.HasPrefix(content, "---\n") {
		return profile
	}

	rest := strings.TrimPrefix(content, "---\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return profile
	}

	metadata := rest[:end]
	body := strings.TrimSpace(rest[end+len("\n---"):])
	for _, line := range strings.Split(metadata, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(strings.ToLower(key)) {
		case "name":
			if strings.TrimSpace(value) != "" {
				profile.Name = strings.TrimSpace(value)
			}
		case "description":
			profile.Description = strings.TrimSpace(value)
		}
	}
	profile.Content = body
	return profile
}
