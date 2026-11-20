package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

// 保存整体预算和阶段
type explorationState struct {
	Accounting            []explorationAccounting
	SessionPurposes       map[string]string                      //后台命令会话 ID 到用途的映射。exec_poll 和 exec_stop 据此沿用启动命令的用途
	SessionBaselines      map[string]sessionModificationBaseline //后台命令会话 ID 到修改基准的映射，保存启动批次和启动前的目标文件快照。命令结束时用它和结束快照比较，以确认跨批次的变更
	Extensions            int                                    //已获准的扩容次数
	Rejection             string                                 //最近保存的拒绝原因，例如扩容额度不足、决策无效或探索额度耗尽
	ModificationLeft      int                                    //当前实施窗口剩余的轮数
	ModificationConfirmed bool                                   //是否已确认目标文件发生实际变更。开始新一轮实施承诺时会重置
	ModificationEvidence  []modificationEvidence                 //已确认的变更记录，包含文件路径、变更前后版本、工具批次及对应的实施承诺编号
	Policy                config.ExplorationBudget
	TaskType              string                      //任务分类。空字符串表示尚未分类，analysis 表示分析任务，modification 表示修改任务
	Phase                 explorationPhase            //当前阶段。exploring 表示探索，implement 表示正在履行实施承诺，unable 表示决定停止并说明无法继续的原因。
	Used                  int                         //已消耗的探索额度
	Invalid               int                         //已计入决策纠错预算的无效尝试次数。超过 Policy.DecisionRetries 会触发停止
	Pending               bool                        //是否必须补交有效的探索决策
	VerificationRetryUsed bool                        //本次执行是否已获得混合批次拆分重试机会
	Handoff               *state.ExplorationDecision  //当前保存的阶段决策。实施时包含目标、依据、下一步操作和目标文件。选择 unable 时也会保存该决策。
	Handoffs              []state.ExplorationDecision //本次执行中所有获准的实施承诺。其位置用于将批次和变更证据关联到对应承诺
}

// decisionContext 仅在需要推进探索决策或实施时为原始请求追加提示。
func (e *explorationState) decisionContext(modelContext state.Contexter, schema config.LLMOutputSchema, executionID string, multiAgent bool) state.Contexter {
	required := e.required()
	if !required && e.Phase != phaseImplement {
		return modelContext
	}

	request := modelContext.ModelRequest()
	if required {
		request.LogLabel = "exploration budget"
		// 已确认目标修改后可继续验证，调查仍由批次准入处理。
		// 待纠错或首次交接必须在独立的无工具轮次完成。
		request.AllowTools = e.ModificationConfirmed && !e.Pending
		if request.AllowTools {
			request.TrailingContext += "\n已确认目标修改，探索额度已耗尽。请单独提交验证批次，例如 go build ./... 或 go test ./...。如果把搜索或读取源码与验证放在同一批，整批都不会执行。确需继续调查时，先依据已有证据单独提交 exploration.choice=extend，获批后再调用调查工具。验证完成后按正常协议收尾。"
		} else {
			request.TrailingContext += "\n" + explorationInstructions + "\n\n" + explorationFewShot(schema, multiAgent)
		}
		request.TrailingContext += fmt.Sprintf("\nExploration state: instance=%s task_type=%s phase=%s used=%d/%d decision_required=%t extensions=%d rejection=%q modification_left=%d handoff=%d", executionID, e.TaskType, e.Phase, e.Used, e.allowance(), required, e.Extensions, e.Rejection, e.ModificationLeft, len(e.Handoffs))
	}
	if e.Phase == phaseImplement {
		if e.ModificationLeft > 0 && e.TaskType != "analysis" && e.Used >= e.allowance() {
			request.AdvertiseTool = func(name config.McpToolName) bool {
				return isTargetLocationTool(name) || fixedExplorationPurpose(name) != "investigation"
			}
		}
		// 实施交接不在工具历史中；恢复也必须让模型看到既有证据和真实剩余窗口。
		handoff, _ := json.Marshal(e.Handoff)
		request.TrailingContext += fmt.Sprintf("\nImplementation state: instance=%s phase=%s used=%d/%d extensions=%d modification_left=%d handoff=%s", executionID, e.Phase, e.Used, e.allowance(), e.Extensions, e.ModificationLeft, handoff)
		if e.Rejection != "" {
			request.TrailingContext += "\n" + e.Rejection
		}
		// 每轮重建提示，并单独置于纠错消息之后。
		location := "如需准备 apply_patch，可用 grep 或 read_file 定位源码，每次显式指定 target_files 中的单个文件。定位批次消耗一轮修改窗口，整批失败时退回。"
		request.FinalUserMessage = fmt.Sprintf("已进入实施阶段，剩余修改窗口为 %d 轮。请依据已有证据，优先对 target_files 执行 next_operation，尽快产生实际目标变更。%s仅在修改确有需要时执行工作区检查。若存在阻止安全修改的信息缺口，请单独申请探索延期；无法继续时返回 unable。修改后执行验证。", e.ModificationLeft, location)
		if strings.HasPrefix(e.Rejection, "Operation rejected: exploration allowance exhausted") {
			request.FinalUserMessage = fmt.Sprintf("已进入实施阶段，剩余修改窗口为 %d 轮。请依据以上纠错提示推进 target_files 的 next_operation。%s工作区检查和探索延期以当前额度为准；无法继续时选择 unable。修改后执行验证。", e.ModificationLeft, location)
		}
	}
	return frozenCompactContext{Contexter: modelContext, request: request}
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
- implement 的 next_operation 必须描述接下来执行的具体修改；若判断无需进一步操作，将 skip_next_operation 设为 1 并留空 next_operation，不得在 next_operation 中填写否定声明。
- implement：确立修改任务，必须提供 target_files（工作区相对路径）、target（预期修改）、evidence 和 next_operation；也兼容在 target 中提供单个文件的旧格式。
- extend：必须提供 gap、blocking_reason、investigation 和 exit_condition。
- unable：必须提供 reason 和 execution_state。

## 额度与修改证据

- 实施必须在修改窗口内确认目标文件发生变化。调查仍消耗探索额度，修改窗口不提供免费探索。
- 仅当探索额度、延期次数和总决策预算允许时，运行时才批准延期。延期被拒绝后必须选择 implement 或 unable，不得重复申请已耗尽的额度。
- 运行时将使用受支持标准测试参数的 go test 识别为验证，将简单的 sed -i 替换及 apply_patch 识别为修改；实际修改后，git diff -- 加已确认目标的精确路径可识别为结果验证。
- 实施阶段可用 read_file 和 grep 定位交接目标文件的源码，计入修改窗口。其他源码读取与搜索、任意脚本或解释器以及不支持的命令选项均消耗探索额度。
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
- next_operation：implement 被接受后将执行的下一步修改操作；skip_next_operation 为 1 时留空。
- skip_next_operation：整数标志，1 表示判断无需执行下一步修改（应走正常完成路径），0 表示将正常执行 next_operation。
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
