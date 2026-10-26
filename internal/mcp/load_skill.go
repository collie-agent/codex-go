package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

type loadSkillArgs struct {
	SkillName string `json:"skill_name" jsonschema:"exact name from the available skills index, not a file path"`
}

func registerLoadSkillTool(server *sdk.Server) {
	sdk.AddTool(server, &sdk.Tool{
		Name:        string(config.LoadSkillMCPToolName),
		Description: `Load the complete SKILL.md instructions for a skill from ~/.agents/skills/. Select the skill using its description in Available skills, then call this tool with its exact name before proceeding with matching work. Do not assume skill instructions before loading. Example arguments: {"skill_name":"tdd"}. The returned content remains available in the ReAct tool history.`,
		InputSchema: toolInputSchema[loadSkillArgs](),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, args loadSkillArgs) (*sdk.CallToolResult, any, error) {
		content, err := loadSkill(ctx, args.SkillName)
		if err != nil {
			return nil, nil, err
		}
		result, err := toolResponse(nil, content, false)
		return result, nil, err
	})
}

func loadSkill(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", fmt.Errorf("skill_name must be a non-empty skill name, not a path")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate skills home: %w", err)
	}
	var found string
	// skill_name 是元数据标识，不拼入文件路径；目录名可以与声明的 name 不同。
	err = filepath.WalkDir(filepath.Join(home, ".agents", "skills"), func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
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
		var metadata struct {
			Name string `yaml:"name"`
		}
		if err := yaml.Unmarshal(bytes.Join(lines[1:end], []byte("\n")), &metadata); err != nil {
			return fmt.Errorf("parse skill %s: %w", path, err)
		}
		if strings.TrimSpace(metadata.Name) == name {
			found = string(content)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("load skill %q: %w", name, err)
	}
	if found == "" {
		return "", fmt.Errorf("unknown skill %q", name)
	}
	return found, nil
}
