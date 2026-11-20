package mcp

import (
	"regexp"
	"slices"
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
	if buildOrTestCommand(command, args) {
		return "verification"
	}
	return "investigation"
}

// Match direct invocations and conventional npm script names. Script contents
// are workspace-defined, so other npm scripts keep the default classification.
func buildOrTestCommand(command string, args []string) bool {
	switch command {
	case "go":
		return len(args) > 0 && args[0] == "build" &&
			verificationArgs(args[1:], "-v -race -trimpath -modcacherw -buildvcs", "-o -tags -mod -buildmode -ldflags -gcflags")
	case "npm":
		return slices.Equal(args, []string{"run", "build"}) ||
			slices.Equal(args, []string{"test"}) ||
			slices.Equal(args, []string{"run", "test"})
	case "npx":
		// Only recognize named tools, with no npx package or shell options.
		return len(args) > 0 && (args[0] == "tsc" || args[0] == "jest" || args[0] == "vitest") &&
			buildOrTestCommand(args[0], args[1:])
	case "tsc":
		return verificationArgs(args, "-b --build --noEmit --pretty --incremental --strict --force --verbose", "-p --project --outDir --target --module")
	case "jest":
		return verificationArgs(args, "--ci --runInBand --coverage --silent --verbose", "-t --testNamePattern")
	case "vitest":
		return len(args) > 0 && args[0] == "run" &&
			verificationArgs(args[1:], "--coverage --silent", "-t --testNamePattern")
	case "pytest":
		return verificationArgs(args, "-q -v -vv -x --disable-warnings", "-k -m --maxfail")
	case "python", "python3":
		if len(args) < 2 || args[0] != "-m" {
			return false
		}
		switch args[1] {
		case "build":
			return verificationArgs(args[2:], "-s --sdist -w --wheel -n --no-isolation", "-o --outdir")
		case "pytest":
			return buildOrTestCommand("pytest", args[2:])
		case "unittest":
			return verificationArgs(args[2:], "-v --verbose -q --quiet -f --failfast -b --buffer", "-s --start-directory -p --pattern -t --top-level-directory -k")
		}
	}
	return false
}

func verificationArgs(args []string, switches, valueOptions string) bool {
	switchFlags := strings.Fields(switches)
	valueFlags := strings.Fields(valueOptions)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flag, value, assigned := strings.Cut(arg, "=")
		switch {
		case slices.Contains(switchFlags, flag):
			if assigned && value != "true" && value != "false" {
				return false
			}
		case slices.Contains(valueFlags, flag):
			if !assigned {
				i++
				if i >= len(args) || strings.HasPrefix(args[i], "-") {
					return false
				}
			} else if value == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
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
