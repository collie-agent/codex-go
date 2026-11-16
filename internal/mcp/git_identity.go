package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// readGitIdentity 使用 Git 自身的解析器，只读取 ~/.gitconfig 中的直接配置。
// 禁用 include，避免为提交身份读取额外宿主文件；未配置的字段保持 Git 默认行为。
func readGitIdentity() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil // 未提供 HOME 的服务仍可执行不依赖宿主身份的命令。
	}
	file := filepath.Join(home, ".gitconfig")
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read Git identity: %w", err)
	}
	// 与沙箱一样使用可信系统 Git，不从宿主 PATH 解析程序。
	if _, err := os.Stat("/usr/bin/git"); os.IsNotExist(err) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var identity []string
	for _, field := range []struct{ key, suffix string }{{"user.name", "NAME"}, {"user.email", "EMAIL"}} {
		cmd := exec.CommandContext(ctx, "/usr/bin/git", "config", "--file", file, "--no-includes", "--null", "--get", field.key)
		cmd.Env = []string{}
		output, err := cmd.Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue // Git 用退出码 1 表示该字段未配置。
			}
			return nil, fmt.Errorf("read Git identity %s: %w", field.key, err)
		}
		value := strings.TrimSuffix(string(output), "\x00")
		identity = append(identity, "GIT_AUTHOR_"+field.suffix+"="+value, "GIT_COMMITTER_"+field.suffix+"="+value)
	}
	return identity, nil
}
