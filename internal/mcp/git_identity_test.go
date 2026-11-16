package mcp_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/mcp"
)

func TestCommandExecutorUsesHostGitIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_AUTHOR_NAME", "Ignored host environment")
	config := "[user]\n name = Config User\n email = config@example.invalid\n[include]\n path = extra.config\n[credential]\n helper = secret-helper\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "extra.config"), []byte("[user]\n name = Included User\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import os,pathlib,subprocess
def git(*args):
    return subprocess.check_output(['git', *args]).decode().strip()
git('init', '--quiet')
git('config', 'user.name', 'Repository User')
git('config', 'user.email', 'repository@example.invalid')
git('-c', 'commit.gpgsign=false', 'commit', '--allow-empty', '--quiet', '-m', 'identity test')
assert git('log', '-1', '--format=%an|%ae|%cn|%ce') == 'Config User|config@example.invalid|Config User|config@example.invalid'
assert not pathlib.Path(os.environ['HOME'], '.gitconfig').exists()
assert subprocess.run(['git', 'config', '--get', 'credential.helper'], capture_output=True).returncode == 1
`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorWithoutHostGitConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import os
assert not any(k in os.environ for k in ('GIT_AUTHOR_NAME', 'GIT_AUTHOR_EMAIL', 'GIT_COMMITTER_NAME', 'GIT_COMMITTER_EMAIL'))`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
