package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"

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
	ctx           context.Context
	maxTurn       int
	toolConfig    filemcp.Config
	mcpClientSess *mcpSdk.ClientSession
	serverSess    *mcpSdk.ServerSession
	checkpoint    func() error
	executionMu   sync.Mutex
	executionErr  error
	checkpointErr error
	skills        []Skill
	output        provider.Outputer
}

// 最大轮数固定在代码中，避免任务通过配置无限延长。
const maxReactTurns = 100

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
	if err := filemcp.RegisterTools(server, toolConfig); err != nil {
		return nil, fmt.Errorf("register MCP tools: %w", err)
	}
	serverSess, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect MCP server: %w", err)
	}
	mcpClient := mcpSdk.NewClient(&mcpSdk.Implementation{Name: "codex-go-responses", Version: "0.1.0"}, nil)
	mcpClientSess, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSess.Close()
		return nil, fmt.Errorf("connect MCP client: %w", err)
	}
	return &BaseReactProcess[T]{
		ctx:           ctx,
		maxTurn:       maxReactTurns,
		toolConfig:    toolConfig,
		mcpClientSess: mcpClientSess,
		serverSess:    serverSess,
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
	delete(cfg.AllowTools, config.SwitchToPlanExecuteMCPToolName)
	rc := fileContext.NewReactContext(durable)
	rc.SetSysPrompt(cfg.SysPrompt)
	addSkillsToReactContext(rc, r.skills)
	return rc
}

// run 是模板方法：公共循环统一维护重试预算、checkpoint 和工具副作用顺序。
// validate 与 control 分别提供结果校验和控制工具策略。
func (r *BaseReactProcess[T]) run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState, rc *fileContext.ReactContext, modelContext fileContext.Contexter, validate func(T) error, control func([]*mcpSdk.CallToolParams) ([]AgentProcessorType, bool, error)) (T, []AgentProcessorType, error) {
	var zero T
	if r.mcpClientSess != nil {
		defer r.mcpClientSess.Close()
	}
	if r.serverSess != nil {
		defer r.serverSess.Close()
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
	for _, call := range durableState.ToolExecutions {
		copy := *call
		rc.AppendToolResult(&copy)
	}
	worker := NewWorker[T](r.ctx, agentConfig, r.mcpClientSess)
	worker.output = r.output
	worker.localTools = []*mcpSdk.Tool{readToolResultDefinition()}
	// 每个执行实例独立去重；恢复时沿用已登记的调用标识。
	seenCalls := make(map[string]string)
	for _, call := range durableState.ToolExecutions {
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
	batchOffset := 0
	for _, call := range durableState.ToolExecutions {
		if call.Batch > batchOffset {
			batchOffset = call.Batch
		}
	}
	for i := 1; i <= r.maxTurn; i++ {
		// 同轮纠错不推进过期边界；恢复时最新一批仍供首次决策使用。
		transient.minimumBatch = batchOffset + i - 1
		var result T
		var toolParams []*mcpSdk.CallToolParams
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
			result, _, toolParams, err = worker.Run(modelContext)
			if err == nil {
				break
			}
			var callErr *toolCallError
			if errors.As(err, &callErr) {
				log.Printf("task %s: [react] tool calling error: %v", durableState.RunId, err)
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
		// 在登记和执行前移除重复项，也覆盖模型同一批返回的重复调用。
		pending := toolParams[:0]
		for _, param := range toolParams {
			// 摘要可能已替换此前回读的上下文，纯只读查询允许重新获取同一页。
			if param.Name == string(config.ReadToolResultMCPToolName) {
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
				message := fmt.Sprintf("Tool %q with these arguments was already called (previous call_id=%q). Duplicate call_id=%q was not executed. Refer to the previous call and its result instead of repeating it.", param.Name, original, id)
				worker.corrections = append(worker.corrections, message)
				r.publishToolResult(durableState.RunId, &fileContext.ToolResult{ToolName: param.Name, ToolCallId: id, Error: message})
				continue
			}
			seenCalls[key] = id
			pending = append(pending, param)
		}
		toolParams = pending
		if len(toolParams) == 0 {
			continue
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
		r.batchToolCallings(toolParams, rc, agentConfig)
		if r.executionErr != nil {
			return zero, nil, r.executionErr
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
	return zero, nil, fmt.Errorf("ReAct exhausted maximum turns (%d)", r.maxTurn)
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
			r.checkpointErr = r.checkpoint()
			r.executionErr = errors.Join(r.executionErr, r.checkpointErr)
		}
	}
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
		log.Printf("[react] tool calling error: %s", result.Error)
	}
	if agentConfig == nil || agentConfig.LogLevel.InfoEnabled() {
		log.Printf("[react] tool calling result: %+v", result)
	}
	if shouldObserveTool(toolParam) {
		// exec 的参数和原始结果随快照交给观察器，由 LLM 筛选影响后续决策的新增信息。
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

// publishToolResult 只发布执行结果，不改变上下文或触发工具重试。
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
		log.Printf("task %s: emit tool outcome: %v", taskID, err)
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
