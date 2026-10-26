package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	state "github.com/collie-agent/codex-go/internal/context"

	"gopkg.in/yaml.v3"
)

// Skill 是启动时注册的技能索引，不包含 SKILL.md 的执行指令正文。
type Skill struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
}

func addSkillsToReactContext(rc *state.ReactContext, skills []Skill) {
	if len(skills) == 0 {
		return
	}
	// JSON 引用元数据，避免多行 description 改变提示词的 Markdown 结构。
	metadata, _ := json.Marshal(skills)
	rc.SetSysPrompt("# Available skills\n\n以下是已注册技能的名称和用途索引：\n\n" + string(metadata))
}

// LoadSkills 按目录顺序扫描用户技能；注册结果由调用方持有，避免全局可变状态。
func LoadSkills() ([]Skill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate skills home: %w", err)
	}
	var skills []Skill
	root := filepath.Join(home, ".agents", "skills")
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// 未安装任何技能不应阻止 agent-server 启动。
			if path == root && os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read skill %s: %w", path, err)
		}
		// 仅解析 YAML front matter；技能正文可能含任意 Markdown，不能作为 YAML 解码。
		lines := bytes.Split(bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")), []byte("\n"))
		if len(lines) == 0 || string(lines[0]) != "---" {
			return fmt.Errorf("skill %s: missing YAML front matter", path)
		}
		end := 1
		for end < len(lines) && string(lines[end]) != "---" {
			end++
		}
		if end == len(lines) {
			return fmt.Errorf("skill %s: unterminated YAML front matter", path)
		}
		var skill Skill
		if err := yaml.Unmarshal(bytes.Join(lines[1:end], []byte("\n")), &skill); err != nil {
			return fmt.Errorf("parse skill %s: %w", path, err)
		}
		skill.Name = strings.TrimSpace(skill.Name)
		skill.Description = strings.TrimSpace(skill.Description)
		if skill.Name == "" || skill.Description == "" {
			return fmt.Errorf("skill %s: name and description are required", path)
		}
		skills = append(skills, skill)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	return skills, nil
}
