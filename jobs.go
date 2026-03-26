package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// JobMetadata 任务元数据（从 JOB.md 的 YAML 前言解析）
type JobMetadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Schedule    string `yaml:"schedule"`  // "once" 或 "recurring"
	Status      string `yaml:"status"`    // "pending" / "in_progress" / "completed" / "cancelled"
	LastRun     string `yaml:"last_run"`  // 上次执行时间
	Path        string                    // JOB.md 完整路径
}

// JobsLoader 任务加载器，扫描 config/jobs/ 目录
type JobsLoader struct {
	dir  string
	jobs []JobMetadata
}

// NewJobsLoader 创建任务加载器
func NewJobsLoader(dir string) *JobsLoader {
	if dir == "" {
		dir = "config/jobs"
	}
	jl := &JobsLoader{dir: dir}
	jl.Load()
	return jl
}

// Load 扫描任务目录，解析所有 JOB.md 的 YAML 前言
func (jl *JobsLoader) Load() {
	jl.jobs = nil

	// 确保目录存在
	os.MkdirAll(jl.dir, 0755)

	entries, err := os.ReadDir(jl.dir)
	if err != nil {
		log.Printf("[任务] 读取任务目录失败: %v", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		jobMDPath := filepath.Join(jl.dir, entry.Name(), "JOB.md")
		data, err := os.ReadFile(jobMDPath)
		if err != nil {
			continue
		}

		meta := parseJobFrontmatter(string(data), jobMDPath)
		if meta != nil {
			jl.jobs = append(jl.jobs, *meta)
		}
	}

	log.Printf("[任务] 共加载 %d 个任务", len(jl.jobs))
}

// ListActiveJobs 返回所有活跃的任务（pending 或 in_progress，以及 recurring 未 cancelled 的）
func (jl *JobsLoader) ListActiveJobs() []JobMetadata {
	var active []JobMetadata
	for _, job := range jl.jobs {
		if job.Status == "pending" || job.Status == "in_progress" {
			active = append(active, job)
		} else if job.Schedule == "recurring" && job.Status != "cancelled" {
			active = append(active, job)
		}
	}
	return active
}

// ReadJob 读取指定任务的完整 JOB.md 内容
func (jl *JobsLoader) ReadJob(jobID string) (string, error) {
	for _, job := range jl.jobs {
		if strings.EqualFold(filepath.Base(filepath.Dir(job.Path)), jobID) {
			data, err := os.ReadFile(job.Path)
			if err != nil {
				return "", fmt.Errorf("读取任务文件失败: %w", err)
			}
			return string(data), nil
		}
	}
	return "", fmt.Errorf("未找到任务: %s", jobID)
}

// WriteJob 写入/更新任务的 JOB.md 内容
func (jl *JobsLoader) WriteJob(jobID string, content string) error {
	jobDir := filepath.Join(jl.dir, jobID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return fmt.Errorf("创建任务目录失败: %w", err)
	}

	jobPath := filepath.Join(jobDir, "JOB.md")
	if err := os.WriteFile(jobPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("写入任务文件失败: %w", err)
	}

	// 重新加载
	jl.Load()
	return nil
}

// parseJobFrontmatter 解析 JOB.md 的 YAML 前言
func parseJobFrontmatter(content string, path string) *JobMetadata {
	if !strings.HasPrefix(content, "---") {
		return nil
	}

	endIdx := strings.Index(content[3:], "\n---")
	if endIdx == -1 {
		return nil
	}

	yamlStr := content[3 : 3+endIdx]

	var meta JobMetadata
	if err := yaml.Unmarshal([]byte(yamlStr), &meta); err != nil {
		log.Printf("[任务] 解析 YAML 前言失败 (%s): %v", path, err)
		return nil
	}

	if meta.Name == "" {
		return nil
	}

	// 默认值
	if meta.Schedule == "" {
		meta.Schedule = "once"
	}
	if meta.Status == "" {
		meta.Status = "pending"
	}

	meta.Path = path
	return &meta
}
