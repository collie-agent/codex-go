package mcp

import (
	"regexp"
	"strings"
)

// CommandPurpose uses a deliberately bounded grammar, never model declarations.
// Unknown commands, scripts and options remain investigation, even when they print test results.
func CommandPurpose(command string, args []string) string {
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
