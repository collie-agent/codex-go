package mcp_test

import (
	"strings"
	"testing"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestCommandPurposeRecognizesBoundedWorkspaceChecks(t *testing.T) {
	for _, args := range [][]string{
		{"status"}, {"status", "-s"}, {"status", "--short"},
		{"status", "--porcelain"}, {"status", "--porcelain=v1"}, {"status", "--porcelain=v2"},
		{"status", "--porcelain=1"}, {"status", "--porcelain=2"},
		{"status", "--short", "--branch"}, {"status", "-s", "-b", "-z"},
		{"status", "--long", "--show-stash", "--null"},
		{"status", "--untracked-files=no"}, {"status", "--untracked-files=normal"}, {"status", "--untracked-files=all"},
		{"status", "-uno"}, {"status", "-unormal"}, {"status", "-uall"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if got := filemcp.CommandPurpose("git", args); got != "workspace_check" {
				t.Fatalf("purpose=%s", got)
			}
		})
	}
}

func TestCommandPurposeKeepsUnsupportedWorkspaceCommandsAsInvestigation(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"git", nil}, {"git", []string{"status", "--unknown"}}, {"git", []string{"status", "--porcelain=v3"}},
		{"git", []string{"status", "--untracked-files=unexpected"}}, {"git", []string{"status", "--", "main.go"}},
		{"git", []string{"-C", ".", "status"}}, {"git", []string{"-c", "alias.st=status", "st"}},
		{"git status", nil}, {"git status && go test", nil}, {"sh", []string{"-c", "git status"}},
		{"python3", []string{"-c", "print('git status')"}}, {"./status.sh", nil},
		{"git", []string{"status", ";", "go", "test"}},
		{"git", []string{"diff"}}, {"git", []string{"diff", "HEAD~1"}}, {"git", []string{"diff", "--", "main.go"}},
	} {
		t.Run(tc.command+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			if got := filemcp.CommandPurpose(tc.command, tc.args); got != "investigation" {
				t.Fatalf("purpose=%s", got)
			}
		})
	}
}
