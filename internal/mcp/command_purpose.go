package mcp

import (
	"regexp"
	"strings"
)

// CommandPurpose uses a deliberately bounded grammar, never model declarations.
// Unknown commands, scripts and options remain investigation, even when they print test results.
func CommandPurpose(command string, args []string) string {
	if command == "git" && workspaceStatusArgs(args) {
		return "workspace_check"
	}
	if command == "sed" && len(args) >= 3 && args[0] == "-i" && regexp.MustCompile(`^s/[^/\\]*/[^/\\]*/g?$`).MatchString(args[1]) {
		for _, path := range args[2:] {
			if strings.HasPrefix(path, "-") {
				return "investigation"
			}
		}
		return "modification"
	}
	if command == "go" && len(args) > 0 && args[0] == "test" {
		for i := 1; i < len(args); i++ {
			arg := args[i]
			if !strings.HasPrefix(arg, "-") {
				continue
			}
			flag, _, assigned := strings.Cut(arg, "=")
			switch flag {
			case "-v", "-race", "-short", "-cover", "-json", "-failfast", "-benchmem", "-fullpath":
			case "-run", "-count", "-timeout", "-parallel", "-cpu", "-shuffle", "-bench", "-benchtime", "-coverprofile", "-covermode", "-coverpkg":
				if !assigned {
					i++
					if i >= len(args) {
						return "investigation"
					}
				}
			default:
				return "investigation"
			}
		}
		return "verification"
	}
	return "investigation"
}

// 工作区检查只接受有限的显示选项，不扩展到全局 git 参数、路径表达式或脚本。
// 未知参数一律回到调查，避免仅凭 status 字样绕过探索预算。
func workspaceStatusArgs(args []string) bool {
	if len(args) == 0 || args[0] != "status" {
		return false
	}
	for _, arg := range args[1:] {
		switch arg {
		case "-s", "--short", "-b", "--branch", "--long", "--show-stash", "-z", "--null",
			"--porcelain", "--porcelain=v1", "--porcelain=v2", "--porcelain=1", "--porcelain=2",
			"-uno", "-unormal", "-uall", "--untracked-files=no", "--untracked-files=normal", "--untracked-files=all":
		default:
			return false
		}
	}
	return true
}
