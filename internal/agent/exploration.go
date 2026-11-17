package agent

import (
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

type explorationState struct {
	Accounting            []explorationAccounting
	SessionPurposes       map[string]string
	SessionBaselines      map[string]sessionModificationBaseline
	Extensions            int
	Rejection             string
	ModificationLeft      int
	ModificationConfirmed bool
	ModificationEvidence  []modificationEvidence
	Policy                config.ExplorationBudget
	TaskType              string
	Phase                 string
	Used                  int
	Invalid               int
	Pending               bool
	Handoff               *state.ExplorationDecision
	Handoffs              []state.ExplorationDecision
}

const explorationInstructions = `# 探索预算与阶段决策

## 本轮强制限制

- 当前轮仅用于探索预算的分类与阶段决策，所有 MCP 工具和本地工具均已禁用。不得发起任何工具调用，包括搜索、读取文件、列目录、执行命令或修改文件。
- 上下文中的 function_call、function_call_output 及工具名称、参数和结果均为历史记录，仅供判断已有证据，不代表本轮可用工具；不得重放、续接或模仿这些调用。
- 只能依据已有证据，按当前输出协议返回包含 exploration 的独立阶段决策；不得附带工具调用，也不得用工具调用代替决策。纯分析任务应返回 task_type=analysis 及 basis，分类被接受后再回答。
- implement 的 next_operation 和 extend 的 investigation 仅描述获批后计划执行的操作，本轮不得执行。信息不足时申请 extend；无法继续且不能延期时选择 unable，不得先调用工具补充信息。

## 任务分类

- 获取上下文后对任务分类，最迟不得超过探索额度耗尽时。
- analysis 必须解释原始目标为何无需修改代码，不能舍弃用户的修改目标。若调查证明无需修改，完成前必须在 exploration.evidence 中提供具体证据。

## 阶段选择

达到阈值时，返回 exploration，choice 为 implement、extend 或 unable：

- 已有本实例确认的修改且随后验证成功时，可以按正常协议返回 completed，无须创建新的实施交接。
- implement 的 next_operation 必须描述接下来执行的具体修改，不能填写“无需操作”“直接结束”或已完成声明。
- implement：确立修改任务，必须提供 target_files（工作区相对路径）、target（预期修改）、evidence 和 next_operation；也兼容在 target 中提供单个文件的旧格式。
- extend：必须提供 gap、blocking_reason、investigation 和 exit_condition。
- unable：必须提供 reason 和 execution_state。

## 额度与修改证据

- 实施必须在修改窗口内确认目标文件发生变化。调查仍消耗探索额度，修改窗口不提供免费探索。
- 仅当探索额度、延期次数和总决策预算允许时，运行时才批准延期。延期被拒绝后必须选择 implement 或 unable，不得重复申请已耗尽的额度。
- 运行时将使用受支持标准测试参数的 go test 识别为验证，将简单的 sed -i 替换及 apply_patch 识别为修改；实际修改后，git diff -- 加已确认目标的精确路径可识别为结果验证。
- 源码读取与搜索、任意脚本或解释器、不支持的命令选项均保守地消耗探索额度；声称用于验证不能豁免。
- 包含调查的混合批次消耗一轮探索。进程轮询、停止及已存结果读取继承原始命令或调用的用途；未知标识仍视为调查。
- 验证命令与修改操作必须分开执行；混合批次中的修改归属不明确。
- 只有已确认的目标文件变化能够证明实施；workspace_version、测试输出、退出状态或口头声明均不能证明。

## 输出字段

以下字段均位于 exploration 对象中。

- task_type：任务类型，modification 表示需要修改代码，analysis 表示仅需分析说明。
- basis：任务分类的依据，说明用户目标为何需要或无需修改代码。
- choice：阶段决策，implement 表示开始实施，extend 表示申请继续调查，unable 表示无法继续；仅做任务分类时可省略。
- target_files：implement 的目标文件，使用工作区相对路径；JSON 中为字符串数组，行协议中单个文件的写法见示例。
- target：implement 预期完成的具体修改，说明要改变的行为或逻辑。
- evidence：支持实施决策或无需修改结论的具体观察证据，应来自实际查看的代码或调查结果。
- next_operation：implement 被接受后将执行的下一步修改操作。
- gap：extend 尚需补齐的信息缺口。
- blocking_reason：该信息缺口为何阻碍安全、正确地实施修改。
- investigation：extend 期间为补齐缺口计划执行的具体调查。
- exit_condition：结束延期调查的可判断条件，说明何时信息足以作出下一步决策。
- reason：unable 无法继续完成任务的具体原因。
- execution_state：unable 时已确认的执行状态，包括已完成的调查、是否修改文件及是否执行验证。

`

// explorationFewShot 按实际输出协议和响应层级构造示例，避免模型混用 JSON 与行协议。
func explorationFewShot(schema config.LLMOutputSchema, multiAgent bool) string {
	if schema != config.JsonLLMOutputSchema && schema != config.LineProtocolLLMOutputSchema {
		return ""
	}
	examples := []struct {
		context string
		json    string
		line    string
	}{
		{`### 示例 1：原因和目标明确时开始实施
上下文：用户要求修复空输入问题。已阅读 internal/parser/parser.go 和 internal/parser/parser_test.go，确认 Parse 在检查 input 前先解引用，且缺少空输入测试；探索额度已耗尽。`, `{"exploration":{"task_type":"modification","basis":"用户要求修复空输入的代码问题。","choice":"implement","target_files":["internal/parser/parser.go","internal/parser/parser_test.go"],"target":"在 Parse 解引用空输入之前返回错误。","evidence":"已查看的 Parse 实现在空值检查前解引用 input。","next_operation":"应用补丁，将空值检查移到解引用之前，并添加空输入回归测试。"}}`, `BEGIN EXPLORATION
TASK_TYPE="modification"
BASIS="用户要求修复空输入的代码问题。"
CHOICE="implement"
TARGET_FILES="internal/parser/parser.go"
TARGET_FILES="internal/parser/parser_test.go"
TARGET="在 Parse 解引用空输入之前返回错误。"
EVIDENCE="已查看的 Parse 实现在空值检查前解引用 input。"
NEXT_OPERATION="应用补丁，将空值检查移到解引用之前，并添加空输入回归测试。"
END EXPLORATION`},
		{`### 示例 2：针对明确的阻塞缺口申请有限延期
上下文：用户要求修复超时问题。超时分支已明确，但尚未阅读调用方的取消约定。探索额度已耗尽，仍可申请延期。`, `{"exploration":{"task_type":"modification","basis":"所要求的超时修复需要修改取消处理。","choice":"extend","gap":"尚不清楚调用方是否负责取消。","blocking_reason":"未明确取消约定就修改取消逻辑，可能取消共享任务。","investigation":"阅读直接调用方及其取消测试。","exit_condition":"确认取消职责的归属，并选定需要修改的具体分支。"}}`, `BEGIN EXPLORATION
TASK_TYPE="modification"
BASIS="所要求的超时修复需要修改取消处理。"
CHOICE="extend"
GAP="尚不清楚调用方是否负责取消。"
BLOCKING_REASON="未明确取消约定就修改取消逻辑，可能取消共享任务。"
INVESTIGATION="阅读直接调用方及其取消测试。"
EXIT_CONDITION="确认取消职责的归属，并选定需要修改的具体分支。"
END EXPLORATION`},
		{`### 示例 3：必要依赖不可用且延期被拒绝时停止
上下文：用户要求的 API 迁移依赖工作区中缺失的私有 schema。访问失败，运行时拒绝继续延期。尚未修改文件。`, `{"exploration":{"task_type":"modification","basis":"用户要求的 API 迁移需要修改代码。","choice":"unable","reason":"必要的私有 schema 不可用，且不允许继续延期。","execution_state":"已确认 schema 缺失且访问失败；未修改文件，也未运行迁移验证。"}}`, `BEGIN EXPLORATION
TASK_TYPE="modification"
BASIS="用户要求的 API 迁移需要修改代码。"
CHOICE="unable"
REASON="必要的私有 schema 不可用，且不允许继续延期。"
EXECUTION_STATE="已确认 schema 缺失且访问失败；未修改文件，也未运行迁移验证。"
END EXPLORATION`},
		{`### 示例 4：将纯分析目标归类为分析任务
上下文：用户仅询问 Parse 如何处理空输入。阅读函数后已明确答案；用户未要求修改代码。`, `{"exploration":{"task_type":"analysis","basis":"目标是解释现有的空输入处理，可根据已查看的函数回答，无需修改代码。"}}`, `BEGIN EXPLORATION
TASK_TYPE="analysis"
BASIS="目标是解释现有的空输入处理，可根据已查看的函数回答，无需修改代码。"
END EXPLORATION`},
	}
	var prompt strings.Builder
	prompt.WriteString("# 探索决策示例（根据实际观察调整路径和证据，不得编造证据）\n以下均为不附带工具调用的独立阶段决策；仅在运行时接受后执行下一步操作。\n")
	// 只注入当前协议的说明和示例，避免另一种协议的语法干扰输出。
	if schema == config.LineProtocolLLMOutputSchema {
		prompt.WriteString("多个目标文件必须重复 TARGET_FILES 行，每行只写一个字符串；不得使用数组字面量。\n")
	}
	for _, example := range examples {
		response := example.json
		if schema == config.LineProtocolLLMOutputSchema {
			response = "BEGIN RESULT\n" + example.line + "\nEND RESULT"
			if multiAgent {
				response = "BEGIN RESULT\n" + response + "\nEND RESULT"
			}
		} else if multiAgent {
			response = `{"result":` + response + `,"next":[]}`
		}
		prompt.WriteString("\n" + example.context + "\n响应：\n" + response + "\n")
	}
	prompt.WriteString("## 分类后的回答\n\nanalysis 分类被接受后，在后续响应中使用正常回答协议提供解释。")
	return prompt.String()
}

func (e *explorationState) allowance() int {
	return e.Policy.InitialTurns + e.Extensions*e.Policy.ExtensionTurns
}

func (e *explorationState) required() bool {
	return e.Pending || (e.TaskType != "analysis" && e.Used >= e.allowance() && !(e.Phase == "implement" && e.ModificationLeft > 0))
}
func (e *explorationState) decide(d *state.ExplorationDecision, remaining int) (string, error) {
	if d == nil {
		return "", fmt.Errorf("exploration threshold requires classification and phase decision")
	}
	nonempty := func(values ...string) bool {
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return false
			}
		}
		return true
	}
	switch d.TaskType {
	case "analysis":
		if !nonempty(d.Basis) {
			return "", fmt.Errorf("analysis requires basis")
		}
		// 一旦确认修改目标，不允许仅重命名任务类型；无须改动必须交付证据。
		if e.TaskType == "modification" && !nonempty(d.Evidence) {
			return "", fmt.Errorf("modification goal requires concrete no-change evidence")
		}
	case "modification":
		if !nonempty(d.Basis) {
			return "", fmt.Errorf("modification classification requires basis")
		}
	case "":
		if e.TaskType == "" {
			return "", fmt.Errorf("task_type required")
		}
	default:
		return "", fmt.Errorf("invalid task_type")
	}
	if d.TaskType != "" {
		e.TaskType = d.TaskType
	}
	if d.Choice == "" {
		if e.TaskType != "analysis" && e.Used >= e.allowance() {
			return "", fmt.Errorf("phase choice required")
		}
		return "", nil
	}
	switch d.Choice {
	case "implement":
		if !nonempty(d.Target, d.Evidence, d.NextOperation) {
			return "", fmt.Errorf("implement requires target, evidence, next_operation")
		}
		// 否定操作或结束声明不构成新的实施承诺，应走已有证据完成路径。
		operation := strings.ToLower(strings.TrimSpace(d.NextOperation))
		for _, phrase := range []string{"无需", "无须", "不需要", "直接结束", "返回最终", "no further", "no operation", "nothing to", "already complete", "return final", "no-op", "noop"} {
			if strings.Contains(operation, phrase) {
				return "", fmt.Errorf("implement next_operation must describe a concrete modification; use normal completion for verified existing changes")
			}
		}
		if err := e.beginModification(d); err != nil {
			return "", err
		}
		// 实施承诺确认了修改目标，不能继续享有分析任务的探索豁免。
		e.TaskType = "modification"
		return "", nil
	case "extend":
		if !nonempty(d.Gap, d.BlockingReason, d.Investigation, d.ExitCondition) {
			return "", fmt.Errorf("extend requires gap, blocking_reason, investigation, exit_condition")
		}
		// 审批使用扣除当前决策与失败收尾预留后的额度，不增加总预算。
		if e.Extensions >= e.Policy.MaxExtensions {
			e.Rejection = "extension limit exhausted; choose implement or unable"
			return "", fmt.Errorf("%s", e.Rejection)
		}
		if remaining < e.Policy.ExtensionTurns {
			e.Rejection = "insufficient total decision budget for extension; choose implement or unable"
			return "", fmt.Errorf("%s", e.Rejection)
		}
		e.Extensions++
		e.Rejection = ""
		if e.Phase != "implement" {
			e.Phase = "exploring"
		}
		return "", nil
	case "unable":
		if !nonempty(d.Reason, d.ExecutionState) {
			return "", fmt.Errorf("unable requires reason and execution_state")
		}
	default:
		return "", fmt.Errorf("invalid phase choice")
	}
	e.Handoff = d
	e.Phase = d.Choice
	if d.Choice == "unable" {
		return "unable: " + d.Reason + "; confirmed execution state: " + d.ExecutionState, nil
	}
	return "", nil
}
