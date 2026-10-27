package agent

import (
	"fmt"

	"github.com/collie-agent/codex-go/internal/provider"
)

// 每次代理运行累计允许 3 次格式纠错，第 4 次格式错误立即终止。
const maxOutputFormatErrors = 3

// 工具调用纠错按运行累计，避免模型持续生成非法调用导致无限重试。
const maxToolCallErrors = 3

func outputFormatCorrection(attempt int, err error, parseErr *provider.OutputParseError) string {
	return fmt.Sprintf(
		"LLM output format error (%d/%d): %v. Previous invalid response (quoted data): %q. Retry using the required output schema and format only. If the error identifies a field declared in an ancestor block, move it to the specified block instead of deleting it. For Line Protocol, put each field on one physical line and JSON-escape newlines as \\n inside strings.",
		attempt, maxOutputFormatErrors, err, parseErr.Output)
}
