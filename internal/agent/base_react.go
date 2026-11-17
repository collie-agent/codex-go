package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/provider"
	mcpSdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const toolCallIDMetaKey = "call_id"

// BaseReactProcess 复用 ReAct 执行机制；具体 Process 决定配置、上下文和结果契约。
// MCP 会话归一次 Run 所有，结束时关闭；Process 不可并发运行或重复使用。
type BaseReactProcess[T LLMSchema] struct {
	fileProgressPolicy     FileProgressPolicy
	ctx                    context.Context
	maxTurn                int
	toolConfig             filemcp.Config
	mcpClientSess          *mcpSdk.ClientSession
	serverSess             *mcpSdk.ServerSession
	closeTools             func()
	checkpoint             func() error
	executionMu            sync.Mutex
	executionErr           error
	checkpointErr          error
	beforeResultCheckpoint func() error
	skills                 []Skill
	output                 provider.Outputer
}

// 最大轮数固定在代码中，避免任务通过配置无限延长。
const maxReactTurns = 60

func newBaseReactProcess[T LLMSchema](ctx context.Context, toolConfig filemcp.Config) (*BaseReactProcess[T], error) {
	clientTransport, serverTransport := mcpSdk.NewInMemoryTransports()
	return connectBaseReactProcess[T](ctx, toolConfig, clientTransport, serverTransport)
}

// newBaseReactProcessWithCommand connects to a separately built MCP server over stdio.
func newBaseReactProcessWithCommand[T LLMSchema](ctx context.Context, command *exec.Cmd) (*BaseReactProcess[T], error) {
	if ctx == nil {
		return nil, fmt.Errorf("agent context is required")
	}
	if command == nil {
		return nil, fmt.Errorf("MCP command is required")
	}
	if command.Cancel != nil {
		// CommandContext must allow MCP to reap its process sessions before
		// dying. WaitDelay still bounds shutdown if the server is unresponsive.
		command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
		if command.WaitDelay == 0 {
			command.WaitDelay = 8 * time.Second
		}
	}
	client := mcpSdk.NewClient(&mcpSdk.Implementation{Name: "codex-go-responses", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcpSdk.CommandTransport{Command: command}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to MCP command: %w", err)
	}
	return &BaseReactProcess[T]{ctx: ctx, maxTurn: maxReactTurns, mcpClientSess: session}, nil
}

func connectBaseReactProcess[T LLMSchema](ctx context.Context, toolConfig filemcp.Config, clientTransport, serverTransport mcpSdk.Transport) (*BaseReactProcess[T], error) {
	if ctx == nil {
		return nil, fmt.Errorf("agent context is required")
	}
	server := mcpSdk.NewServer(&mcpSdk.Implementation{Name: "codex-go-agent", Version: "0.1.0"}, nil)
	closeTools, err := filemcp.RegisterToolsWithCleanup(server, toolConfig)
	if err != nil {
		return nil, fmt.Errorf("register MCP tools: %w", err)
	}
	serverSess, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		closeTools()
		return nil, fmt.Errorf("connect MCP server: %w", err)
	}
	mcpClient := mcpSdk.NewClient(&mcpSdk.Implementation{Name: "codex-go-responses", Version: "0.1.0"}, nil)
	mcpClientSess, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSess.Close()
		closeTools()
		return nil, fmt.Errorf("connect MCP client: %w", err)
	}
	return &BaseReactProcess[T]{
		ctx:           ctx,
		maxTurn:       maxReactTurns,
		toolConfig:    toolConfig,
		mcpClientSess: mcpClientSess,
		serverSess:    serverSess,
		closeTools:    closeTools,
	}, nil
}

// SetCheckpoint 在每批工具执行前确认恢复状态可写；失败立即结束本次运行。
func (r *BaseReactProcess[T]) SetCheckpoint(save func() error) { r.checkpoint = save }

// SetSkills 保存启动时的技能快照，调用方后续修改切片不会影响运行。
func (r *BaseReactProcess[T]) SetSkills(skills []Skill) {
	r.skills = slices.Clone(skills)
}

// newContext 只添加两种模式共用的提示词和技能，不选择运行模式。
func (r *BaseReactProcess[T]) newContext(cfg *config.AgentConfig, durable *fileContext.DurableState) *fileContext.ReactContext {
	cfg.AllowTools[config.LoadSkillMCPToolName] = struct{}{}
	cfg.AllowTools[config.ReadToolResultMCPToolName] = struct{}{}
	rc := fileContext.NewReactContext(durable)
	rc.SetSysPrompt(cfg.SysPrompt)
	addSkillsToReactContext(rc, r.skills)
	return rc
}

// run 是模板方法：公共循环统一维护重试预算、checkpoint 和工具副作用顺序。
// validate、control 与 progressResult 分别提供结果校验、控制工具和收尾结果适配策略。
func (r *BaseReactProcess[T]) run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState, rc *fileContext.ReactContext, modelContext fileContext.Contexter, validate func(T) error, control func([]*mcpSdk.CallToolParams) ([]AgentProcessorType, bool, error), progressResult func(*T) *ReactResult) (T, []AgentProcessorType, error) {
	var zero T
	if r.mcpClientSess != nil {
		defer r.mcpClientSess.Close()
	}
	if r.serverSess != nil {
		defer r.serverSess.Close()
	}
	if r.closeTools != nil {
		defer r.closeTools()
	}
	// 未确认的调用可能已经产生副作用，恢复必须在任何模型请求之前停止。
	for _, call := range durableState.ToolExecutions {
		if call != nil && call.Status == "" {
			return zero, nil, fmt.Errorf("unknown tool outcome: call_id=%q name=%q arguments=%s", call.ToolCallId, call.ToolName, call.Arguments)
		}
	}
	if len(durableState.ExecutionResult) > 0 {
		var saved T
		if err := json.Unmarshal(durableState.ExecutionResult, &saved); err != nil {
			return zero, nil, err
		}
		return saved, []AgentProcessorType{EndAgentType}, validate(saved)
	}
	if durableState.ExecutionID == "" && len(durableState.StepExecutions) == 0 {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return zero, nil, err
		}
		durableState.ExecutionID = hex.EncodeToString(id[:])
		if r.checkpoint != nil {
			if err := r.checkpoint(); err != nil {
				return zero, nil, err
			}
		}
	}
	var cleanup func()
	r.output, cleanup = agentOutput(r.ctx, agentConfig)
	defer cleanup()
	r.output = provider.AttributeOutput(r.ctx, r.output, durableState.RunId, agentConfig.Name)
	rc.RestoreToolResults(durableState.ToolExecutions)
	worker := NewWorker[T](r.ctx, agentConfig, r.mcpClientSess)
	worker.output = r.output
	worker.localTools = []*mcpSdk.Tool{readToolResultDefinition()}
	// 每个执行实例独立去重；恢复时沿用已登记的调用标识。
	seenCalls := make(map[string]string)
	lastWorkspaceChange := -1
	for _, call := range durableState.ToolExecutions {
		if changesWorkspace(call.ToolName) && call.Batch > lastWorkspaceChange {
			lastWorkspaceChange = call.Batch
		}
	}
	for _, call := range durableState.ToolExecutions {
		// 同批工具并行执行，读取可能早于写入；恢复时也不能复用这一批及更早的观察。
		if isWorkspaceObservation(call.ToolName) && call.Batch <= lastWorkspaceChange {
			continue
		}
		seenCalls[toolInvocationKey(call.ToolName, call.Arguments)] = call.ToolCallId
	}
	transient := &transientResultsContext{Contexter: modelContext}
	compacted := &compactModelContext{Contexter: transient}
	if len(durableState.ExecutionCompaction) > 0 {
		var saved ExecutionCompaction
		if err := json.Unmarshal(durableState.ExecutionCompaction, &saved); err != nil {
			return zero, nil, fmt.Errorf("restore execution compaction: %w", err)
		}
		if saved.ExecutionID == durableState.ExecutionID {
			compacted.compaction = &saved
		}
	}
	modelContext = compacted
	compaction := &asyncCompaction{}
	defer compaction.close()
	formatErrors := 0
	toolCallErrors := 0
	consecutiveDuplicates := 0
	progress := newFileProgressGuard(r.fileProgressPolicy)
	budget, err := restoreDecisionProgress(durableState, progress)
	if err != nil {
		return zero, nil, err
	}
	// 通过结果适配器统一普通、计划步骤与多代理的探索状态；观察器不启用预算。
	explorationEnabled := progressResult(&zero) != nil
	legacyExploration := budget.Exploration == nil && (len(durableState.ExecutionProgress) > 0 || len(durableState.ToolExecutions) > 0)
	if explorationEnabled && budget.Exploration == nil {
		policy := agentConfig.ExplorationBudget
		if policy.InitialTurns == 0 {
			policy = config.DefaultExplorationBudget()
		}
		budget.Exploration = &explorationState{Policy: policy, Phase: "exploring", Used: budget.UsedTurns}
	}

	if explorationEnabled {
		e := budget.Exploration
		log.Printf("[INFO] [exploration budget] instance=%s restore=%t legacy_exploration=%t initial_turns=%d extension_turns=%d max_extensions=%d decision_retries=%d modification_window_turns=%d task_type=%s phase=%s exploration=%d/%d extensions=%d invalid=%d modification_left=%d remaining_total=%d rejection=%q", durableState.ExecutionID, len(durableState.ExecutionProgress) > 0 || len(durableState.ToolExecutions) > 0, legacyExploration, e.Policy.InitialTurns, e.Policy.ExtensionTurns, e.Policy.MaxExtensions, e.Policy.DecisionRetries, e.Policy.ModificationWindowTurns, e.TaskType, e.Phase, e.Used, e.allowance(), e.Extensions, e.Invalid, e.ModificationLeft, r.maxTurn-budget.UsedTurns, e.Rejection)
		// 新 MCP 连接不继承旧进程身份；历史批次仍保留核验依据。
		budget.Exploration.SessionPurposes = nil
		budget.Exploration.SessionBaselines = nil
	}

	if budget.RecoveryActive && budget.Correction != "" {
		worker.corrections = append(worker.corrections, budget.Correction)
	}
	finish := func(reason string) (T, []AgentProcessorType, error) {
		if r.checkpointErr != nil {
			return zero, nil, r.checkpointErr
		}
		if e := budget.Exploration; explorationEnabled && e != nil {
			log.Printf("[INFO] [exploration budget] instance=%s task_type=%s phase=%s exploration=%d/%d extensions=%d modification_left=%d stop=%q", durableState.ExecutionID, e.TaskType, e.Phase, e.Used, e.allowance(), e.Extensions, e.ModificationLeft, reason)
		}
		return r.finishProgress(worker, modelContext, durableState, budget, reason, progressResult)
	}
	observeProgress := func(batch BatchEvidence, remaining int) error {
		correction, err := progress.observe(batch, durableState, r.maxTurn-budget.UsedTurns-1)
		budget.capture(progress)
		if !progress.recoveryActive {
			budget.Correction = ""
			worker.corrections = slices.DeleteFunc(worker.corrections, func(value string) bool { return strings.HasPrefix(value, "File investigation recovery (") })
		}
		for _, call := range batch.Completed {
			if call.Batch > budget.ObservedThroughBatch {
				budget.ObservedThroughBatch = call.Batch
			}
		}
		if correction != "" {
			budget.Correction = correction
		}
		if saveErr := saveDecisionProgress(durableState, budget, r.checkpoint); saveErr != nil {
			r.checkpointErr = saveErr
			return saveErr
		}
		if correction != "" {
			worker.corrections = append(worker.corrections, correction)
		}
		return err
	}
	// Confirmed batches may have reached storage just before a crash, while the
	// progress checkpoint did not. Reconcile them before another model decision.
	failedExecs := make(failedCommands)
	for start := 0; start < len(durableState.ToolExecutions); {
		end := start + 1
		for end < len(durableState.ToolExecutions) && durableState.ToolExecutions[end].Batch == durableState.ToolExecutions[start].Batch {
			end++
		}
		batch := durableState.ToolExecutions[start:end]
		if explorationEnabled {
			budget.Exploration.reconcile(batch)
			if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
				return zero, nil, err
			}
		}
		if batch[0].Batch > budget.ObservedThroughBatch { //resume时只处理未observe progress的批次tool execution
			if err := observeProgress(BatchEvidence{Completed: batch}, r.maxTurn-budget.UsedTurns-1); err != nil {
				return finish(err.Error())
			}
		}
		// 已分析过进展的批次也要恢复失败证据，并按登记批次恢复失效边界。
		failedExecs.observe(batch)
		start = end
	}
	var cachedDuplicates []*fileContext.ToolResult
	batchOffset := 0
	for _, call := range durableState.ToolExecutions {
		if call.Batch > batchOffset {
			batchOffset = call.Batch
		}
	}
	for i := 1; i <= r.maxTurn; i++ {
		if budget.Finalizing || budget.UsedTurns >= r.maxTurn-1 {
			return finish("maximum decision budget exhausted")
		}
		if explorationEnabled && budget.Exploration.Pending && budget.Exploration.Invalid > budget.Exploration.Policy.DecisionRetries {
			return finish("exploration decision corrections exhausted: " + budget.Exploration.Rejection)
		}
		if explorationEnabled && budget.Exploration.Phase == "unable" {
			reason := "saved unable decision"
			if handoff := budget.Exploration.Handoff; handoff != nil {
				reason = "unable: " + handoff.Reason + "; confirmed execution state: " + handoff.ExecutionState
			}
			return finish(reason)
		}
		if explorationEnabled && budget.Exploration.Phase == "implement" && budget.Exploration.ModificationLeft <= 0 {
			return finish("modification window exhausted without confirmed target change")
		}
		// 同轮纠错不推进过期边界；恢复时保留最近三批目录检索结果。
		transient.minimumBatch = batchOffset + i - discoveryResultDecisionTurns
		explorationCharged := false
		var result T
		var toolParams []*mcpSdk.CallToolParams
		repairingFormat := false
		// 格式纠错留在当前轮，避免最后一轮的可恢复错误直接耗尽执行预算。
		for {
			if err := r.ctx.Err(); err != nil {
				return zero, nil, err
			}
			var err error
			compacted.corrections = worker.corrections
			if applied, err := r.prepareCompaction(compaction, agentConfig, durableState, compacted, worker.inputTokens); err != nil {
				return zero, nil, err
			} else if applied {
				worker.inputTokens = 0 // 旧模型视图的 usage 不再用于触发压缩。
			}
			decisionContext := modelContext
			if explorationEnabled && budget.Exploration.required() {
				request := modelContext.ModelRequest()
				request.LogLabel = "exploration budget"
				e := budget.Exploration
				// 已确认目标修改后仍需执行验证；返回的调查批次仍由下面的阶段门禁整批拒绝。
				// 待纠错决策不能借此执行工具，首次交接也仍是独立的无工具轮次。
				request.AllowTools = e.ModificationConfirmed && !e.Pending
				// 仅在阈值决策或纠错待处理时追加预算提示，避免普通轮次重复占用上下文。
				_, multiAgent := any(result).(MultiAgentResult)
				if request.AllowTools {
					request.TrailingContext += "\n已确认目标修改，但探索额度已耗尽。可以执行受支持的验证操作或按正常协议完成任务。继续调查前须依据已有证据单独提交 exploration 阶段决策，不得混入工具调用。"
				} else {
					request.TrailingContext += "\n" + explorationInstructions + "\n\n" + explorationFewShot(agentConfig.LLMConfig.OutputSchema, multiAgent)
				}
				request.TrailingContext += fmt.Sprintf("\nExploration state: instance=%s task_type=%s phase=%s used=%d/%d decision_required=%t extensions=%d rejection=%q modification_left=%d handoff=%d", durableState.ExecutionID, e.TaskType, e.Phase, e.Used, e.allowance(), e.required(), e.Extensions, e.Rejection, e.ModificationLeft, len(e.Handoffs))
				decisionContext = frozenCompactContext{Contexter: modelContext, request: request}
			}
			if explorationEnabled && budget.Exploration.Phase == "implement" {
				e := budget.Exploration
				request := decisionContext.ModelRequest()
				// 实施交接不在工具历史中；恢复也必须让模型看到既有证据和真实剩余窗口。
				handoff, _ := json.Marshal(e.Handoff)
				request.TrailingContext += fmt.Sprintf("\nImplementation state: instance=%s phase=%s used=%d/%d extensions=%d modification_left=%d handoff=%s", durableState.ExecutionID, e.Phase, e.Used, e.allowance(), e.Extensions, e.ModificationLeft, handoff)
				if e.Rejection != "" {
					request.TrailingContext += "\n" + e.Rejection
				}
				decisionContext = frozenCompactContext{Contexter: decisionContext, request: request}
			}
			if len(cachedDuplicates) > 0 {
				// 缓存回传属于上一轮的协议响应，放在裁剪和压缩之后，且不写入实际执行历史。
				request := decisionContext.ModelRequest()
				request.ToolResults = append(slices.Clone(request.ToolResults), cachedDuplicates...)
				decisionContext = frozenCompactContext{Contexter: modelContext, request: request}
			}
			if budget.Finalizing || budget.UsedTurns >= r.maxTurn-1 {
				return finish("maximum decision budget exhausted")
			}
			if explorationEnabled {
				e := budget.Exploration
				explorationCharged = e.TaskType != "analysis" && !e.required() && e.Phase != "implement"
				if explorationCharged {
					e.Used++
				}

				log.Printf("[INFO] [exploration budget] instance=%s task_type=%s phase=%s exploration=%d/%d extensions=%d rejection=%q modification_left=%d handoff=%d", durableState.ExecutionID, e.TaskType, e.Phase, e.Used, e.allowance(), e.Extensions, e.Rejection, e.ModificationLeft, len(e.Handoffs))
			}
			budget.UsedTurns++
			if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
				return zero, nil, err
			}
			if repairingFormat {
				// 格式纠错只重写已有响应，保留工具证据但禁止新增远程和本地工具调用。
				request := decisionContext.ModelRequest()
				request.AllowTools = false
				decisionContext = frozenCompactContext{Contexter: decisionContext, request: request}
			}
			result, _, toolParams, err = worker.Run(decisionContext)
			if err == nil {
				break
			}
			var formatErr *provider.OutputParseError
			if errors.As(err, &formatErr) {
				repairingFormat = true
			}
			// 阈值处的协议错误也属于阶段纠错，不能借通用格式重试扩大额度。
			if explorationEnabled && !explorationCharged && budget.Exploration.required() {
				var parseErr *provider.OutputParseError
				var toolErr *toolCallError
				if errors.As(err, &parseErr) || errors.As(err, &toolErr) {
					e := budget.Exploration
					e.Invalid++
					e.Pending = true
					e.Rejection = err.Error()
					if saveErr := saveDecisionProgress(durableState, budget, r.checkpoint); saveErr != nil {
						return zero, nil, saveErr
					}
					if e.Invalid > e.Policy.DecisionRetries {
						return finish("exploration decision corrections exhausted: " + err.Error())
					}
					worker.corrections = append(worker.corrections, "Exploration decision rejected: "+err.Error())
					continue
				}
			}
			var callErr *toolCallError
			if errors.As(err, &callErr) {
				log.Printf("[ERROR] task %s: [react] tool calling error: %v", durableState.RunId, err)
				// 非法调用整批拒绝，反馈原始错误后在当前轮重试，不放行任何工具副作用。
				toolCallErrors++
				if toolCallErrors > maxToolCallErrors {
					return zero, nil, fmt.Errorf("ReAct turn %d: tool call errors exceeded limit (%d): %w", i, maxToolCallErrors, err)
				}
				worker.corrections = append(worker.corrections, fmt.Sprintf("LLM tool call error (%d/%d): %v. Retry using only the supplied native tools with their exact names and valid JSON arguments. If the task is complete, return the result as final text in the required output format; BEGIN RESULT is a text delimiter, not a tool name. The rejected batch was not executed.", toolCallErrors, maxToolCallErrors, err))
				continue
			}
			var parseErr *provider.OutputParseError
			if !errors.As(err, &parseErr) {
				return zero, nil, fmt.Errorf("ReAct turn %d: %w", i, err)
			}
			formatErrors++
			if formatErrors > maxOutputFormatErrors {
				return zero, nil, fmt.Errorf("ReAct turn %d: output format errors exceeded limit (%d): %w", i, maxOutputFormatErrors, err)
			}
			// 纠错仅属于本次运行的消息历史，不混入工具观察的 ReportedErrors。
			worker.corrections = append(worker.corrections, outputFormatCorrection(durableState.RunId, "react", formatErrors, err, parseErr))
		}
		cachedDuplicates = nil
		if explorationEnabled {
			e := budget.Exploration
			value := progressResult(&result)
			// 本轮预扣的最后一个探索额度仍允许执行；下一轮才要求独立决策。
			batchPurpose, _ := e.batchPurpose(toolParams)
			// 已确认修改后的测试及完成不申请新的探索；普通调查仍受阈值门禁。
			verification := len(toolParams) > 0 && batchPurpose == "verification"
			completion := len(toolParams) == 0 && value.Status == CompletedReactStatus && e.ModificationConfirmed
			required := e.required() && !explorationCharged && !(!e.Pending && (verification || completion))
			if value.Exploration != nil || required {
				// 校验前记录模型决策，保留被拒绝或缺失 exploration 的返回，便于定位阶段协议问题。
				response, err := json.Marshal(result)
				if err != nil {
					log.Printf("[INFO] [exploration budget] instance=%s llm_result=%+v", durableState.ExecutionID, result)
				} else {
					log.Printf("[INFO] [exploration budget] instance=%s llm_result=%s", durableState.ExecutionID, response)
				}
				if value.Exploration != nil && explorationCharged {
					e.Used--
				}
				candidate := *e
				reason, decisionErr := candidate.decide(value.Exploration, r.maxTurn-budget.UsedTurns-1)
				if len(toolParams) > 0 && e.Phase == "implement" {
					decisionErr = fmt.Errorf("phase decision must be standalone; mixed tool batch was not executed")
				}
				// 实施中的无效决策与违规调查共用剩余窗口，不授予独立阶段纠错额度。
				inModificationWindow := e.Phase == "implement" && !e.ModificationConfirmed
				if decisionErr == nil {
					*e = candidate
					e.Pending = false
				}
				if decisionErr != nil {
					e.Rejection = decisionErr.Error()
					if inModificationWindow {
						e.ModificationLeft--
						e.Pending = false
						e.Rejection = fmt.Sprintf("Exploration decision rejected: %s; apply a target modification, submit a valid standalone extend decision or choose unable. modification_left=%d", decisionErr, e.ModificationLeft)
						log.Printf("[INFO] [exploration budget] instance=%s operation=rejected correction_allowed=%t modification_left=%d", durableState.ExecutionID, e.ModificationLeft > 0, e.ModificationLeft)
					} else {
						e.Invalid++
						e.Pending = true
						worker.corrections = append(worker.corrections, "Exploration decision rejected: "+decisionErr.Error())
					}
					r.publishRejectedToolBatch(durableState.RunId, toolParams, e.Rejection)
				}
				log.Printf("[INFO] [exploration budget] instance=%s task_type=%s phase=%s exploration=%d/%d extensions=%d rejection=%q modification_left=%d handoff=%d decision_error=%v stop=%s", durableState.ExecutionID, e.TaskType, e.Phase, e.Used, e.allowance(), e.Extensions, e.Rejection, e.ModificationLeft, len(e.Handoffs), decisionErr, reason)
				if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
					return zero, nil, err
				}
				if decisionErr != nil && !inModificationWindow && e.Invalid > e.Policy.DecisionRetries {
					return finish("exploration decision corrections exhausted: " + decisionErr.Error())
				}
				if decisionErr == nil && reason != "" {
					return finish(reason)
				}
				continue
			}
		}
		batchPurpose, batchCalls := "investigation", map[string]string(nil)
		if explorationEnabled {
			batchPurpose, batchCalls = budget.Exploration.batchPurpose(toolParams)
		}
		if explorationEnabled && explorationCharged && batchPurpose != "investigation" {
			budget.Exploration.Used--
			explorationCharged = false
			if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
				return zero, nil, err
			}
		}
		if explorationEnabled && budget.Exploration.Phase == "implement" && !budget.Exploration.ModificationConfirmed && len(toolParams) == 0 {
			return finish("implementation handoff has no confirmed target modification")
		}
		if len(toolParams) == 0 {
			validationErr := validate(result)
			terminal := validationErr == nil
			if value, ok := any(result).(ReactResult); ok && value.Status == FailedReactStatus {
				terminal = true
			}
			if terminal && durableState.ExecutionID != "" {
				encoded, err := json.Marshal(result)
				if err != nil {
					return zero, nil, err
				}
				previousResult := durableState.ExecutionResult
				durableState.ExecutionResult = encoded
				if r.checkpoint != nil {
					if err := r.checkpoint(); err != nil {
						durableState.ExecutionResult = previousResult
						return zero, nil, err
					}
				}
			}
			if validationErr != nil {
				return result, nil, validationErr
			}
			return result, []AgentProcessorType{EndAgentType}, nil
		}
		if explorationEnabled {
			e := budget.Exploration
			if e.Phase == "implement" && batchPurpose == "investigation" && e.TaskType != "analysis" {
				if e.Used >= e.allowance() {
					log.Printf("[INFO] [exploration budget] instance=%s purpose=%s admission=denied exploration=%d/%d modification_left=%d", durableState.ExecutionID, batchPurpose, e.Used, e.allowance(), e.ModificationLeft)
					// 策略拒绝不是工具失败：不登记执行或预扣探索，直接消耗业务轮窗口，不能退款。
					e.ModificationLeft--
					e.Rejection = fmt.Sprintf("Operation rejected: exploration allowance exhausted; batch was not executed. Apply a target modification using existing evidence, submit a standalone exploration extend decision with gap, blocking_reason, investigation and exit_condition, or choose unable. modification_left=%d", e.ModificationLeft)
					r.publishRejectedToolBatch(durableState.RunId, toolParams, e.Rejection)
					if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
						return zero, nil, err
					}
					log.Printf("[INFO] [exploration budget] instance=%s operation=rejected correction_allowed=%t modification_left=%d", durableState.ExecutionID, e.ModificationLeft > 0, e.ModificationLeft)
					if e.ModificationLeft <= 0 {
						return finish("modification window exhausted without confirmed target change; exploration allowance exhausted")
					}
					continue
				}
				e.Used++
			}
			e.Rejection = "" // 已接受纠正操作，避免后续模型继续收到过期的拒绝及窗口余量。
			e.Accounting = append(e.Accounting, explorationAccounting{Batch: batchOffset + i, Handoff: len(e.Handoffs), Purpose: batchPurpose, Calls: batchCalls, Charged: batchPurpose == "investigation" && e.TaskType != "analysis", Outcome: "pending"})
			if len(e.Accounting) > 256 {
				e.Accounting = e.Accounting[len(e.Accounting)-256:]
			}
			if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
				return zero, nil, err
			}
		}
		if explorationEnabled && budget.Exploration.Phase == "implement" {
			// 工作区检查不赠送探索额度，也不证明修改；整个业务批次只扣一次修改窗口。
			budget.Exploration.ModificationLeft--
			budget.Exploration.Accounting[len(budget.Exploration.Accounting)-1].ModificationCharged = true
			if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
				return zero, nil, err
			}
		}
		if explorationEnabled {
			e := budget.Exploration
			log.Printf("[INFO] [exploration budget] instance=%s purpose=%s admission=allowed exploration=%d/%d modification_left=%d", durableState.ExecutionID, batchPurpose, e.Used, e.allowance(), e.ModificationLeft)
		}
		// 在登记和执行前移除重复项，也覆盖模型同一批返回的重复调用。
		pending := toolParams[:0]
		for _, param := range toolParams {
			if param.Name == string(config.ExecMCPToolName) {
				args, err := json.Marshal(param.Arguments)
				if err != nil {
					return zero, nil, err
				}
				key := toolInvocationKey(param.Name, string(args))
				if failure, exists := failedExecs[key]; exists && (!failure.transient || failure.attempts >= 2) {
					id, _ := param.Meta[toolCallIDMetaKey].(string)
					message := fmt.Sprintf("Repeated failed command was not executed (previous call_id=%q). No intervening repair or state-changing tool call was observed. Inspect the cached error and repair the source, dependencies or environment before retrying. Transient failures allow only one retry.", failure.call.ToolCallId)
					cached := r.cachedDuplicateResult(durableState, failure.call.ToolCallId, id, param.Name, string(args))
					cached.Error = strings.TrimSpace(message + "\n" + cached.Error)
					cachedDuplicates = append(cachedDuplicates, cached)
					r.publishToolResult(durableState.RunId, &fileContext.ToolResult{ToolName: param.Name, ToolCallId: id, Error: message})
					continue
				}
			}
			// exec 同样依赖实时工作区和网络状态；修复文件后必须允许重跑
			// 相同命令（例如 go mod tidy），不能复用此前失败的结果。
			// 进程会话操作依赖实时状态：修复后可重启同一命令，也可重复
			// 轮询和停止。恢复后的新连接不继承旧进程，不能沿用命令去重。
			if param.Name == string(config.ExecMCPToolName) || param.Name == string(config.ReadToolResultMCPToolName) || param.Name == string(config.ExecStartMCPToolName) || param.Name == string(config.ExecPollMCPToolName) || param.Name == string(config.ExecStopMCPToolName) {
				pending = append(pending, param)
				continue
			}
			args, err := json.Marshal(param.Arguments)
			if err != nil {
				return zero, nil, err
			}
			if raw, ok := param.Meta["raw_arguments"].(string); ok {
				args = []byte(raw)
			}
			key := toolInvocationKey(param.Name, string(args))
			id, _ := param.Meta[toolCallIDMetaKey].(string)
			if original, exists := seenCalls[key]; exists {
				message := fmt.Sprintf("Tool %q with these arguments was already called (previous call_id=%q). Duplicate call_id=%q was not executed; its native tool response contains the cached result. Use that result to choose your next action instead of repeating the original tool.", param.Name, original, id)
				cached := r.cachedDuplicateResult(durableState, original, id, param.Name, string(args))
				cached.Error = strings.TrimSpace(message + "\n" + cached.Error)
				cachedDuplicates = append(cachedDuplicates, cached)
				r.publishToolResult(durableState.RunId, &fileContext.ToolResult{ToolName: param.Name, ToolCallId: id, Error: message})
				continue
			}
			seenCalls[key] = id
			pending = append(pending, param)
		}
		toolParams = pending
		if len(toolParams) == 0 {
			if explorationEnabled {
				e := budget.Exploration
				e.Accounting[len(e.Accounting)-1].Outcome = "cached"
				if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
					return zero, nil, err
				}
			}
			hasFile := false
			for _, call := range cachedDuplicates {
				if call.ToolName == string(config.ReadFileMCPToolName) || call.ToolName == string(config.ReadToolResultMCPToolName) || call.ToolName == string(config.GrepMCPToolName) || call.ToolName == string(config.GlobMCPToolName) || isCommandProgressTool(call.ToolName) {
					hasFile = true
				}
			}
			if hasFile {
				if err := observeProgress(BatchEvidence{Cached: cachedDuplicates}, r.maxTurn-i); err != nil {
					return finish(err.Error())
				}
				continue
			}
			// Preserve the existing duplicate guard for non-file tools.
			consecutiveDuplicates++
			if consecutiveDuplicates >= 3 {
				return zero, nil, fmt.Errorf("ReAct stopped after %d consecutive duplicate tool batches", consecutiveDuplicates)
			}
			continue
		}
		consecutiveDuplicates = 0
		var modificationBefore map[string]targetSnapshot
		if explorationEnabled && budget.Exploration.Phase == "implement" {
			e := budget.Exploration
			modificationBefore = snapshotModificationTargets(r.toolConfig.Workspace, e)
			e.Accounting[len(e.Accounting)-1].Before = modificationBefore
			if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
				return zero, nil, err
			}
		}
		if durableState.ExecutionID != "" {
			for position, param := range toolParams {
				args, err := json.Marshal(param.Arguments)
				if err != nil {
					return zero, nil, err
				}
				id, _ := param.Meta[toolCallIDMetaKey].(string)
				if raw, ok := param.Meta["raw_arguments"].(string); ok {
					args = []byte(raw)
				}
				durableState.ToolExecutions = append(durableState.ToolExecutions, &fileContext.ToolResult{ToolCallId: id, ToolName: param.Name, Arguments: string(args), Batch: batchOffset + i, Position: position})
			}
		}
		// 模型请求期间数据库也可能失效，必须在放行副作用前重新写 checkpoint。
		if r.checkpoint != nil {
			if err := r.checkpoint(); err != nil {
				return zero, nil, err
			}
		}
		if control != nil {
			next, handled, err := control(toolParams)
			if err != nil {
				return zero, nil, err
			}
			if r.executionErr != nil {
				return zero, nil, r.executionErr
			}
			if handled {
				if durableState.ExecutionID != "" {
					for _, param := range toolParams {
						id, _ := param.Meta[toolCallIDMetaKey].(string)
						for _, call := range durableState.ToolExecutions {
							if call.ToolCallId == id && call.Status == "" {
								call.Status = filemcp.ToolCallSuceessStatus
								call.Result = "[]"
							}
						}
					}
					if r.checkpoint != nil {
						if err := r.checkpoint(); err != nil {
							return zero, nil, err
						}
					}
				}
				if len(next) > 0 {
					return zero, next, nil
				}
				continue
			}
		}
		historyOffset := len(durableState.ToolExecutions)
		if explorationEnabled {
			// 最后一个兄弟调用的结果与批次快照一起落盘；阶段确认可以随后补偿。
			r.beforeResultCheckpoint = func() error {
				e := budget.Exploration
				record := &e.Accounting[len(e.Accounting)-1]
				var completed []*fileContext.ToolResult
				for _, call := range durableState.ToolExecutions {
					if call.Batch != record.Batch {
						continue
					}
					if call.Status == "" {
						return nil
					}
					completed = append(completed, call)
				}
				if len(completed) == 0 {
					return nil
				}
				e.captureSessionBaseline(completed, record)
				if record.Before != nil {
					record.After = snapshotModificationTargets(r.toolConfig.Workspace, e)
				}
				e.settleFailedInvestigation(record, completed)
				record.updateOutcome(completed)
				return saveDecisionProgress(durableState, budget, nil)
			}
		}
		r.batchToolCallings(toolParams, rc, agentConfig)
		r.beforeResultCheckpoint = nil
		// 有执行实例时，调用已预登记；否则结果由上下文直接追加。
		var completed []*fileContext.ToolResult
		ids := make(map[string]bool)
		for _, param := range toolParams {
			id, _ := param.Meta[toolCallIDMetaKey].(string)
			ids[id] = true
		}
		if durableState.ExecutionID != "" {
			historyOffset = 0
		}
		for _, call := range durableState.ToolExecutions[historyOffset:] {
			if ids[call.ToolCallId] {
				completed = append(completed, call)
			}
		}
		if explorationEnabled && r.executionErr == nil {
			e := budget.Exploration
			e.recordCommandSessions(completed, batchCalls, modificationBefore, batchOffset+i)
			record := &e.Accounting[len(e.Accounting)-1]
			if modificationBefore != nil && record.After == nil {
				record.After = snapshotModificationTargets(r.toolConfig.Workspace, e)
			}
			e.captureSessionBaseline(completed, record)
			e.settleFailedInvestigation(record, completed)
			record.updateOutcome(completed)
			if record.hasVerificationCommand(completed) {
				// 测试与仍在运行的修改进程可能重叠，旧启动基准不再能区分变更来源。
				e.SessionBaselines = nil
			}
			if modificationBefore != nil && record.Outcome == "known" {
				if modificationBatchCandidate(completed) {
					e.confirmModification(modificationBefore, record.After, batchOffset+i)
				}
				e.confirmSessionModification(completed, record.After, record)
			}
			log.Printf("[INFO] [exploration budget] instance=%s exploration=%d/%d batch=%d since_batch=%d purpose=%s calls=%v outcome=%s confirmed=%t evidence=%v", durableState.ExecutionID, e.Used, e.allowance(), record.Batch, record.SinceBatch, record.Purpose, record.Calls, record.Outcome, e.ModificationConfirmed, e.ModificationEvidence)
			record.Applied = true
			if err := saveDecisionProgress(durableState, budget, r.checkpoint); err != nil {
				return zero, nil, err
			}
		}

		failedExecs.observe(completed)
		if r.executionErr == nil {
			if err := observeProgress(BatchEvidence{Completed: completed, Cached: cachedDuplicates}, r.maxTurn-i); err != nil {
				return finish(err.Error())
			}
		}
		for _, param := range toolParams {
			if changesWorkspace(param.Name) {
				// 命令或补丁失败也可能留下部分修改；进程轮询期间同样可能产生文件变更。
				for key := range seenCalls {
					name, _, _ := strings.Cut(key, "\x00")
					if isWorkspaceObservation(name) {
						delete(seenCalls, key)
					}
				}
				break
			}
		}
		// 服务启动或停止改变了命令观察的外部状态，旧验证结果不可复用。
		// 命令验证缓存另按成功的生命周期调用失效，文件观察已在上方失效。
		for _, param := range toolParams {
			if param.Name != string(config.ExecStartMCPToolName) && param.Name != string(config.ExecStopMCPToolName) {
				continue
			}
			id, _ := param.Meta[toolCallIDMetaKey].(string)
			for _, call := range durableState.ToolExecutions {
				if call.ToolCallId == id && call.Status == filemcp.ToolCallSuceessStatus {
					for key := range seenCalls {
						if strings.HasPrefix(key, string(config.ExecMCPToolName)+"\x00") {
							delete(seenCalls, key)
						}
					}
					break
				}
			}
		}
		if r.executionErr != nil {
			return zero, nil, r.executionErr
		}
		if explorationEnabled && budget.Exploration.Phase == "implement" && budget.Exploration.ModificationLeft == 0 {
			return finish("modification window exhausted without confirmed target change")
		}
		if durableState.ExecutionID != "" {
			rc.UpdateToolResults(func(_ []*fileContext.ToolResult) []*fileContext.ToolResult {
				return slices.Clone(durableState.ToolExecutions)
			})
		}
		if r.ctx.Err() != nil {
			return zero, nil, r.ctx.Err()
		}
	}
	return finish(fmt.Sprintf("maximum decision budget exhausted (%d)", r.maxTurn))
}

func (r *BaseReactProcess[T]) observe(toolResult *fileContext.ToolResult, rc *fileContext.ReactContext, agentConfig *config.AgentConfig) {
	if r == nil || r.ctx == nil || rc == nil || toolResult == nil || r.ctx.Err() != nil {
		return
	}
	// Copy the inputs before starting the request: the next ReAct turn and
	// other observers may update the context while this request is running.
	observationContext := rc.SnapshotObservationContext(toolResult)
	// 配置沿调用链传入并由本次闭包捕获，无需保存在 Process 上。
	go func() {
		observer := NewObserveProcess[ObserveResult](r.ctx, observationContext)
		result, _, err := observer.Run(agentConfig, observationContext.DurableState)
		if r.ctx.Err() != nil {
			return
		}
		if err != nil {
			tr := observationContext.ToolResult
			rc.AppendReportedErrors(fmt.Sprintf("tool %q (call_id=%q): observation failed: %v", tr.ToolName, tr.ToolCallId, err))
			return
		}
		for _, fact := range result.ConfirmedFacts {
			rc.AppendConfirmedFacts(fact)
		}
		for _, reportedError := range result.ReportedErrors {
			rc.AppendReportedErrors(reportedError)
		}
	}()
}

func (r *BaseReactProcess[T]) batchToolCallings(toolParams []*mcpSdk.CallToolParams, rc *fileContext.ReactContext, agentConfig *config.AgentConfig) {
	// 执行前获取本批工具的实际 schema；校验失败也作为工具结果返回下一轮。
	schemas := make(map[string]any)
	var schemaErr error
	schemas[string(config.ReadToolResultMCPToolName)] = readToolResultDefinition().InputSchema
	if r.mcpClientSess != nil {
		for tool, err := range r.mcpClientSess.Tools(r.ctx, nil) {
			if err != nil {
				schemaErr = fmt.Errorf("list tool schemas: %w", err)
				break
			}
			schemas[tool.Name] = tool.InputSchema
		}
	}
	var wg sync.WaitGroup
	for _, toolParam := range toolParams {
		wg.Go(func() {
			err := schemaErr
			if err == nil {
				schema, ok := schemas[toolParam.Name]
				if !ok {
					names := make([]string, 0, len(schemas))
					for name := range schemas {
						names = append(names, name)
					}
					sort.Strings(names)
					err = fmt.Errorf("unknown tool %q; available tools: %s; the tool was not executed", toolParam.Name, strings.Join(names, ", "))
				} else {
					err = validateToolArguments(schema, toolParam.Arguments)
				}
			}
			if err != nil {
				r.updateContext(&mcpSdk.CallToolResult{IsError: true, Content: []mcpSdk.Content{&mcpSdk.TextContent{Text: err.Error()}}}, toolParam, rc, nil, agentConfig)
				return
			}
			if toolParam.Name == string(config.ReadToolResultMCPToolName) {
				callResult := r.readToolResult(toolParam, rc.DurableState)
				r.updateContext(callResult, toolParam, rc, nil, agentConfig)
				return
			}
			callResult, err := r.mcpClientSess.CallTool(r.ctx, toolParam)
			r.updateContext(callResult, toolParam, rc, err, agentConfig)
		})
	}
	wg.Wait()
}

func (r *BaseReactProcess[T]) updateContext(callResult *mcpSdk.CallToolResult, toolParam *mcpSdk.CallToolParams, rc *fileContext.ReactContext, err error, agentConfig *config.AgentConfig) {
	var summaryErr error
	if rc != nil && rc.DurableState != nil {
		// Summarize before entering the persistence lock; raw exec output is never checkpointed.
		callResult, summaryErr = r.summarizeExecResult(callResult, toolParam, agentConfig, rc.DurableState)
	}

	r.executionMu.Lock()
	defer r.executionMu.Unlock()
	if rc == nil || toolParam == nil {
		return
	}

	callID, _ := toolParam.Meta[toolCallIDMetaKey].(string)
	result := &fileContext.ToolResult{ToolCallId: callID, ToolName: toolParam.Name}
	callDescription := fmt.Sprintf("tool %q (call_id=%q)", toolParam.Name, callID)
	var executionErrors []string
	if err != nil {
		executionErrors = append(executionErrors, fmt.Sprintf("%s: MCP call failed: %v", callDescription, err))
	}
	if toolParam.Arguments != nil {
		arguments, marshalErr := json.Marshal(toolParam.Arguments)
		if marshalErr != nil {
			executionErrors = append(executionErrors, fmt.Sprintf("%s: failed to encode arguments as JSON (type %T): %v", callDescription, toolParam.Arguments, marshalErr))
		} else {
			result.Arguments = string(arguments)
			if raw, ok := toolParam.Meta["raw_arguments"].(string); ok {
				result.Arguments = raw
			}
		}
	}
	if callResult == nil && err == nil {
		executionErrors = append(executionErrors, fmt.Sprintf("%s: MCP returned no result and no error; execution outcome is unknown; verify side effects before retrying", callDescription))
	}
	if callResult != nil {
		if !callResult.IsError && toolParam.Name == string(config.ReadFileMCPToolName) {
			result.FileEvidence = evidenceFromResult(callResult.StructuredContent)
		}
		if !callResult.IsError && (toolParam.Name == string(config.GrepMCPToolName) || toolParam.Name == string(config.GlobMCPToolName)) {
			result.SearchEvidence = searchEvidenceFromResult(callResult.StructuredContent)
		}
		if !callResult.IsError && toolParam.Name == string(config.ReadToolResultMCPToolName) {
			result.HistoryEvidence = historyEvidenceFromResult(callResult.StructuredContent)
		}
		// 只保存 content，避免 structuredContent 的重复数据占用模型上下文。
		encoded, marshalErr := json.Marshal(callResult.Content)
		if marshalErr != nil {
			executionErrors = append(executionErrors, fmt.Sprintf("%s: failed to encode MCP result as JSON; result cannot be added to context: %v", callDescription, marshalErr))
		} else {
			result.Result = string(encoded)
		}
		if callResult.IsError {
			var messages []string
			for _, content := range callResult.Content {
				if content, ok := content.(*mcpSdk.TextContent); ok && content != nil && content.Text != "" {
					messages = append(messages, content.Text)
				}
			}
			// 部分 MCP 服务仅返回结构化错误；无文本时保留它，避免丢失修正依据。
			if len(messages) == 0 && callResult.StructuredContent != nil {
				structured, encodeErr := json.Marshal(callResult.StructuredContent)
				if encodeErr != nil {
					messages = append(messages, fmt.Sprintf("failed to encode structured MCP error: %v", encodeErr))
				} else {
					messages = append(messages, "MCP structured error: "+string(structured))
				}
			}
			if len(messages) == 0 {
				messages = append(messages, "MCP returned isError=true without a non-empty text error message; inspect the result for details")
			}
			executionErrors = append(executionErrors, fmt.Sprintf("%s: tool execution failed (MCP isError=true): %s", callDescription, strings.Join(messages, "\n")))
		}
	}
	if len(executionErrors) > 0 && result.Arguments != "" {
		executionErrors = append(executionErrors, "Tool arguments: "+result.Arguments)
	}
	result.Error = strings.Join(executionErrors, "\n")
	result.Status = filemcp.ToolCallSuceessStatus
	if len(executionErrors) > 0 {
		result.Status = filemcp.ToolCallFailedStatus
	}

	// 逐个提交已确认结果；写入失败保留数据库中的未知状态并停止后续模型调度。
	if rc.DurableState != nil && rc.DurableState.ExecutionID != "" {
		for i := len(rc.DurableState.ToolExecutions) - 1; i >= 0; i-- {
			call := rc.DurableState.ToolExecutions[i]
			if call.ToolCallId == callID && call.Status == "" {
				result.Batch = call.Batch
				result.Position = call.Position
				rc.DurableState.ToolExecutions[i] = result
				break
			}
		}
		if callResult == nil {
			result.Status = ""
			r.executionErr = errors.Join(r.executionErr, fmt.Errorf("unknown tool outcome: call_id=%q name=%q arguments=%s", callID, toolParam.Name, result.Arguments))
		}
		// Unknown outcomes stop scheduling, but must not suppress confirmed sibling
		// results. A persistence failure, however, must never be retried here.
		if callResult != nil && r.checkpoint != nil && r.checkpointErr == nil {
			if r.beforeResultCheckpoint != nil {
				r.checkpointErr = r.beforeResultCheckpoint()
			}
			if r.checkpointErr == nil {
				r.checkpointErr = r.checkpoint()
			}
			r.executionErr = errors.Join(r.executionErr, r.checkpointErr)
		}
	}
	r.executionErr = errors.Join(r.executionErr, summaryErr)
	if r.executionErr != nil {
		return
	}
	// 并发完成时先追加，整批结束后按登记顺序重建模型历史。
	rc.AppendToolResult(result)
	// 工具实际执行完成后发布一次结果，不能在模型历史重放时重复发送。
	if rc.DurableState != nil && toolParam.Name != string(config.UpdateCompletionCriteriaMCPToolName) && toolParam.Name != string(config.SwitchToPlanExecuteMCPToolName) {
		r.publishToolResult(rc.DurableState.RunId, result)
	}

	// 错误独立于结果详情打印，确保 error 级别也能看到可恢复的工具失败。
	if result.Error != "" {
		log.Printf("[ERROR] [react] tool calling error: %s", result.Error)
	}
	if agentConfig == nil || agentConfig.LogLevel.InfoEnabled() {
		log.Printf("[INFO] [react] tool calling result: %+v", result)
	}
	if shouldObserveTool(toolParam) {
		// exec 的参数和已记录结果随快照交给观察器，由 LLM 筛选影响后续决策的新增信息。
		// Observation is a separate processor and must use its own resolved
		// settings rather than inheriting ReAct's model or custom prompt.
		observerConfig := agentConfig.ForAgent("observe")
		r.observe(result, rc, observerConfig)
	}
}

// shouldObserveTool 按入口命令筛选，避免搜索参数中出现 npm 等词时误触发观察器。
func shouldObserveTool(params *mcpSdk.CallToolParams) bool {
	if config.McpToolName(params.Name) == config.ApplyPatchMCPToolName {
		return true
	}
	if config.McpToolName(params.Name) != config.ExecMCPToolName {
		return false
	}
	arguments, err := json.Marshal(params.Arguments)
	if err != nil {
		return false
	}
	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(arguments, &input) != nil {
		return false
	}
	switch input.Command {
	case "git", "npm", "poetry", "python", "python3", "ts-node", "tsx":
		return true
	default:
		return false
	}
}

// hasNonEmptyItems 拒绝缺失列表和空白条目，避免无内容的结果通过步骤验收。
func hasNonEmptyItems(items []string) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			return false
		}
	}
	return true
}

// 拒绝发生前已发布 tool_call；即使随后保存失败，也须逐个发布终态。
// 这里只发送事件，不把未执行调用写入执行记录或参与工具失败退款。
func (r *BaseReactProcess[T]) publishRejectedToolBatch(taskID string, params []*mcpSdk.CallToolParams, reason string) {
	for _, param := range params {
		id, _ := param.Meta[toolCallIDMetaKey].(string)
		r.publishToolResult(taskID, &fileContext.ToolResult{ToolName: param.Name, ToolCallId: id, Error: "tool batch was not executed: " + reason})
	}
}

// publishToolResult 只发布工具终态，不改变上下文或触发工具重试。
func (r *BaseReactProcess[T]) publishToolResult(taskID string, result *fileContext.ToolResult) {
	if r.output == nil {
		return
	}
	kind := provider.OutputToolResult
	if result.Error != "" {
		kind = provider.OutputToolError
	}
	data, err := json.Marshal(provider.ToolResultOutput{Name: result.ToolName, CallID: result.ToolCallId, Result: json.RawMessage(result.Result), Error: result.Error})
	if err == nil {
		err = r.output.Emit(r.ctx, provider.OutputEvent{TaskID: taskID, Label: "react", Kind: kind, Data: data})
	}
	if err != nil {
		log.Printf("[INFO] task %s: emit tool outcome: %v", taskID, err)
	}
}

// 控制工具不会保留普通工具历史，因此在控制流分支中单独发布结果。
func (r *BaseReactProcess[T]) publishControlToolResult(param *mcpSdk.CallToolParams, rc *fileContext.ReactContext, response *mcpSdk.CallToolResult, callErr error) {
	if r.output == nil || rc.DurableState == nil {
		return
	}
	callID, _ := param.Meta[toolCallIDMetaKey].(string)
	result := &fileContext.ToolResult{ToolName: param.Name, ToolCallId: callID}
	if callErr != nil {
		result.Error = callErr.Error()
	}
	if response == nil && callErr == nil {
		result.Error = "MCP returned no result; execution outcome is unknown"
	}
	if response != nil {
		content, err := json.Marshal(response.Content)
		if err != nil {
			result.Error += "failed to encode tool result: " + err.Error()
		} else {
			result.Result = string(content)
		}
		if response.IsError {
			result.Error += "MCP tool execution failed: " + result.Result
		}
	}
	r.publishToolResult(rc.DurableState.RunId, result)
}

// toolInvocationKey 规范化 JSON 对象键顺序与空白，保留数字精度。
func isWorkspaceObservation(name string) bool {
	switch config.McpToolName(name) {
	case config.ReadFileMCPToolName, config.ListDirMCPToolName, config.GlobMCPToolName, config.GrepMCPToolName:
		return true
	default:
		return false
	}
}

// 无法从命令参数可靠判断写入范围，因此保守地让文件观察整体失效。
func changesWorkspace(name string) bool {
	switch config.McpToolName(name) {
	case config.ApplyPatchMCPToolName, config.ExecMCPToolName, config.ExecStartMCPToolName, config.ExecPollMCPToolName, config.ExecStopMCPToolName:
		return true
	default:
		return false
	}
}

func toolInvocationKey(name, arguments string) string {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			arguments = string(canonical)
		}
	}
	return name + "\x00" + arguments
}
