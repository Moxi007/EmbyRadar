package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillMetadata 技能元数据（从 SKILL.md 的 YAML 前言解析）
type SkillMetadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Path        string // SKILL.md 完整路径
}

// SkillsLoader 技能加载器，扫描 config/skills/ 目录
type SkillsLoader struct {
	dir    string
	skills []SkillMetadata
}

// NewSkillsLoader 创建技能加载器
func NewSkillsLoader(dir string) *SkillsLoader {
	if dir == "" {
		dir = "config/skills"
	}
	sl := &SkillsLoader{dir: dir}
	sl.Load()
	return sl
}

// Load 扫描技能目录，解析所有 SKILL.md 的 YAML 前言
func (sl *SkillsLoader) Load() {
	sl.skills = nil

	// 确保目录存在
	os.MkdirAll(sl.dir, 0755)

	entries, err := os.ReadDir(sl.dir)
	if err != nil {
		log.Printf("[技能] 读取技能目录失败: %v", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillMDPath := filepath.Join(sl.dir, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(skillMDPath)
		if err != nil {
			continue // 没有 SKILL.md 的目录跳过
		}

		meta := parseSkillFrontmatter(string(data), skillMDPath)
		if meta != nil {
			sl.skills = append(sl.skills, *meta)
			log.Printf("[技能] 已加载: %s - %s", meta.Name, meta.Description)
		}
	}

	log.Printf("[技能] 共加载 %d 个技能", len(sl.skills))
}

// ListSkills 返回所有已加载的技能摘要
func (sl *SkillsLoader) ListSkills() []SkillMetadata {
	return sl.skills
}

// ReadSkill 读取指定技能的完整 SKILL.md 内容
func (sl *SkillsLoader) ReadSkill(skillName string) (string, error) {
	for _, skill := range sl.skills {
		if skill.Name == skillName || strings.EqualFold(filepath.Base(filepath.Dir(skill.Path)), skillName) {
			data, err := os.ReadFile(skill.Path)
			if err != nil {
				return "", fmt.Errorf("读取技能文件失败: %w", err)
			}
			return string(data), nil
		}
	}
	return "", fmt.Errorf("未找到技能: %s", skillName)
}

// parseSkillFrontmatter 解析 SKILL.md 的 YAML 前言
func parseSkillFrontmatter(content string, path string) *SkillMetadata {
	// 匹配 --- 分隔的 YAML 前言
	if !strings.HasPrefix(content, "---") {
		return nil
	}

	endIdx := strings.Index(content[3:], "\n---")
	if endIdx == -1 {
		return nil
	}

	yamlStr := content[3 : 3+endIdx]

	var meta SkillMetadata
	if err := yaml.Unmarshal([]byte(yamlStr), &meta); err != nil {
		log.Printf("[技能] 解析 YAML 前言失败 (%s): %v", path, err)
		return nil
	}

	if meta.Name == "" || meta.Description == "" {
		log.Printf("[技能] 跳过 %s: 缺少 name 或 description", path)
		return nil
	}

	meta.Path = path
	return &meta
}
