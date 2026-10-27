package agent

import (
	"context"
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

type ReactStatus string

const (
	CompletedReactStatus ReactStatus = "completed"
	FailedReactStatus    ReactStatus = "failed"
)

var reserveOnceResultTool = map[config.McpToolName]struct{}{
	config.ListDirMCPToolName: struct{}{},
	config.GlobMCPToolName:    struct{}{},
}

var reserveForeverResultTool = map[config.McpToolName]struct{}{
	config.ReadFileMCPToolName: struct{}{},
	config.GrepMCPToolName:     struct{}{},
}

type ReactResult struct {
	Status ReactStatus `json:"status" lineprotocol:"STATUS"`
	Answer string      `json:"answer" lineprotocol:"ANSWER"`
	// 计划步骤完成时必填；普通 ReAct 模式使用空列表。
	Observation        []string `json:"Observation" lineprotocol:"OBSERVATION"`
	CompletionEvidence []string `json:"CompletionEvidence" lineprotocol:"COMPLETION_EVIDENCE"`
}

type ReactProcess[T LLMSchema] struct {
	ctx                             context.Context
	maxTurn                         int
	toolConfig                      filemcp.Config
	mcpClientSess                   *mcpSdk.ClientSession
	serverSess                      *mcpSdk.ServerSession
	checkpoint                      func() error
	allowPlanSwitch                 bool
	multiAgentContext               string
	generateEntryCompletionCriteria bool
	skills                          []Skill
}

// 最大轮数固定在代码中，避免任务通过配置无限延长。
const maxReactTurns = 100

func NewReactProcess[T LLMSchema](ctx context.Context, toolConfig filemcp.Config) (*ReactProcess[T], error) {
	clientTransport, serverTransport := mcpSdk.NewInMemoryTransports()
	return newReactProcess[T](ctx, toolConfig, clientTransport, serverTransport)
}

// NewReactProcessWithCommand connects to a separately built MCP server over stdio.
func NewReactProcessWithCommand[T LLMSchema](ctx context.Context, command *exec.Cmd) (*ReactProcess[T], error) {
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
	return &ReactProcess[T]{ctx: ctx, maxTurn: maxReactTurns, mcpClientSess: session}, nil
}

func newReactProcess[T LLMSchema](ctx context.Context, toolConfig filemcp.Config, clientTransport, serverTransport mcpSdk.Transport) (*ReactProcess[T], error) {
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
	return &ReactProcess[T]{
		ctx:           ctx,
		maxTurn:       maxReactTurns,
		toolConfig:    toolConfig,
		mcpClientSess: mcpClientSess,
		serverSess:    serverSess,
	}, nil
}

// SetCheckpoint 在每批工具执行前确认恢复状态可写；失败立即结束本次运行。
func (r *ReactProcess[T]) SetCheckpoint(save func() error) { r.checkpoint = save }

// SetSkills 保存启动时的技能快照，调用方后续修改切片不会影响运行。
func (r *ReactProcess[T]) SetSkills(skills []Skill) {
	r.skills = slices.Clone(skills)
}

func (r *ReactProcess[T]) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (T, []AgentProcessorType, error) {
	var zero T
	if r == nil || r.ctx == nil || durableState == nil || agentConfig == nil || agentConfig.LLMConfig == nil {
		return zero, nil, fmt.Errorf("invalid ReAct process")
	}
	if r.mcpClientSess != nil {
		defer r.mcpClientSess.Close()
	}
	if r.serverSess != nil {
		defer r.serverSess.Close()
	}
	if _, multi := any(zero).(MultiAgentResult); !multi {
		agentConfig = agentConfig.ForAgent("react")
	} else {
		agentConfig = agentConfig.Copy()
	}
	// 技能加载是 ReAct 的内置能力，不依赖 YAML 的 allow_tools。
	agentConfig.AllowTools[config.LoadSkillMCPToolName] = struct{}{}
	// 控制流工具仅由支持交接的编排器开放，不依赖 YAML 工具权限。
	delete(agentConfig.AllowTools, config.SwitchToPlanExecuteMCPToolName)
	if r.allowPlanSwitch && len(durableState.StepExecutions) == 0 {
		agentConfig.AllowTools[config.SwitchToPlanExecuteMCPToolName] = struct{}{}
	}
	rc := fileContext.NewReactContext(durableState)
	// 先保留内置 ReAct 执行约束，再把 YAML 的长期约束追加到其后。
	rc.SetSysPrompt(agentConfig.SysPrompt)
	addSkillsToReactContext(rc, r.skills)
	var modelContext fileContext.Contexter = rc
	if _, multi := any(zero).(MultiAgentResult); multi {
		modelContext = fileContext.MultiAgentModelContext{ReactContext: rc, Delegation: r.multiAgentContext, GenerateEntryCompletionCriteria: r.generateEntryCompletionCriteria}
	}
	worker := NewWorker[T](r.ctx, agentConfig, r.mcpClientSess)
	formatErrors := 0
	toolCallErrors := 0
	for i := 1; i <= r.maxTurn; i++ {
		var result T
		var toolParams []*mcpSdk.CallToolParams
		// 格式纠错留在当前轮，避免最后一轮的可恢复错误直接耗尽执行预算。
		for {
			if err := r.ctx.Err(); err != nil {
				return zero, nil, err
			}
			var err error
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
				rc.AppendReportedErrors(fmt.Sprintf("LLM tool call error (%d/%d): %v. Retry using only the supplied native tools with their exact names and valid JSON arguments. If the task is complete, return the result as final text in the required output format; BEGIN RESULT is a text delimiter, not a tool name. The rejected batch was not executed.", toolCallErrors, maxToolCallErrors, err))
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
			rc.AppendReportedErrors(outputFormatCorrection(formatErrors, err, parseErr))
		}
		if len(toolParams) == 0 {
			if multi, ok := any(result).(MultiAgentResult); ok {
				// multi-agent 的 failed 是业务结果，必须保留原因交给 supervisor；
				// 普通 ReAct 的失败契约仍由下方分支处理。
				if multi.Result.Status != CompletedReactStatus && multi.Result.Status != FailedReactStatus {
					return zero, nil, fmt.Errorf("ReAct returned invalid status %q", multi.Result.Status)
				}
				if strings.TrimSpace(multi.Result.Answer) == "" {
					return zero, nil, fmt.Errorf("ReAct result requires a non-empty answer")
				}
			}
			if reactResult, ok := any(result).(ReactResult); ok {
				// 格式解析不校验业务约束：仅明确完成且有答案时才允许成功退出。
				switch reactResult.Status {
				case FailedReactStatus:
					return zero, nil, fmt.Errorf("ReAct reported failure: %s", reactResult.Answer)
				case CompletedReactStatus:
					// 计划步骤必须提供结构化观察与验收证据，不能用 answer 代替。
					if step := durableState.CurrentStep(); step != nil && step.Status == fileContext.RunningExecutionStatus {
						if !hasNonEmptyItems(reactResult.Observation) || !hasNonEmptyItems(reactResult.CompletionEvidence) {
							return zero, nil, fmt.Errorf("ReAct completed step requires non-empty observation and CompletionEvidence")
						}
					}
					if strings.TrimSpace(reactResult.Answer) == "" {
						return zero, nil, fmt.Errorf("ReAct completed result requires a non-empty answer")
					}
				default:
					return zero, nil, fmt.Errorf("ReAct returned invalid status %q", reactResult.Status)
				}
			}
			return result, []AgentProcessorType{EndAgentType}, nil
		}
		// 模型请求期间数据库也可能失效，必须在放行副作用前重新写 checkpoint。
		if r.checkpoint != nil {
			if err := r.checkpoint(); err != nil {
				return zero, nil, err
			}
		}
		if len(toolParams) == 1 && toolParams[0].Name == string(config.SwitchToPlanExecuteMCPToolName) {
			switched, err := r.switchToPlanExecute(toolParams[0], rc, durableState)
			if err != nil {
				return zero, nil, err
			}
			if switched {
				return zero, []AgentProcessorType{PlanAgentType}, nil
			}
			continue
		}
		r.batchToolCallings(toolParams, rc, agentConfig)
		if r.ctx.Err() != nil {
			return zero, nil, r.ctx.Err()
		}
	}
	return zero, nil, fmt.Errorf("ReAct exhausted maximum turns (%d)", r.maxTurn)
}

// switchToPlanExecute handles the MCP control tool and records the handoff context.
func (r *ReactProcess[T]) switchToPlanExecute(toolParam *mcpSdk.CallToolParams, rc *fileContext.ReactContext, durableState *fileContext.DurableState) (bool, error) {
	response, err := r.mcpClientSess.CallTool(r.ctx, toolParam)
	if err != nil {
		return false, fmt.Errorf("switch to plan-execute: %w", err)
	}
	if response == nil {
		return false, fmt.Errorf("switch to plan-execute: missing MCP response")
	}
	if response.IsError {
		encoded, _ := json.Marshal(response.Content)
		rc.AppendReportedErrors("switch_to_plan_execute rejected: " + string(encoded))
		return false, nil
	}
	// 保存原始决策上下文与模型交接摘要，避免规划阶段丢失已经完成的工作。
	args, _ := json.Marshal(toolParam.Arguments)
	handoff, _ := rc.FormatForModel()
	durableState.PlanningContext = handoff + "\n\n# Switch request\n" + string(args)
	log.Printf("task %s: [react] switching to plan-execute", durableState.RunId)
	return true, nil
}

func (r *ReactProcess[T]) observe(toolResult *fileContext.ToolResult, rc *fileContext.ReactContext, agentConfig *config.AgentConfig) {
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

func (r *ReactProcess[T]) batchToolCallings(toolParams []*mcpSdk.CallToolParams, rc *fileContext.ReactContext, agentConfig *config.AgentConfig) {
	// 执行前获取本批工具的实际 schema；校验失败也作为工具结果返回下一轮。
	schemas := make(map[string]any)
	var schemaErr error
	for tool, err := range r.mcpClientSess.Tools(r.ctx, nil) {
		if err != nil {
			schemaErr = fmt.Errorf("list tool schemas: %w", err)
			break
		}
		schemas[tool.Name] = tool.InputSchema
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
			callResult, err := r.mcpClientSess.CallTool(r.ctx, toolParam)
			r.updateContext(callResult, toolParam, rc, err, agentConfig)
		})
	}
	wg.Wait()
}

func (r *ReactProcess[T]) updateContext(callResult *mcpSdk.CallToolResult, toolParam *mcpSdk.CallToolParams, rc *fileContext.ReactContext, err error, agentConfig *config.AgentConfig) {
	if rc == nil || toolParam == nil {
		return
	}

	callID, _ := toolParam.Meta[toolCallIDMetaKey].(string)
	result := &fileContext.ToolResult{ToolCallId: callID, ToolName: toolParam.Name}
	callDescription := fmt.Sprintf("tool %q (call_id=%q)", toolParam.Name, callID)
	var errors []string
	if err != nil {
		errors = append(errors, fmt.Sprintf("%s: MCP call failed: %v", callDescription, err))
	}
	if toolParam.Arguments != nil {
		arguments, marshalErr := json.Marshal(toolParam.Arguments)
		if marshalErr != nil {
			errors = append(errors, fmt.Sprintf("%s: failed to encode arguments as JSON (type %T): %v", callDescription, toolParam.Arguments, marshalErr))
		} else {
			result.Arguments = string(arguments)
		}
	}
	if callResult == nil && err == nil {
		errors = append(errors, fmt.Sprintf("%s: MCP returned no result and no error; execution outcome is unknown; verify side effects before retrying", callDescription))
	}
	if callResult != nil {
		// 只保存 content，避免 structuredContent 的重复数据占用模型上下文。
		encoded, marshalErr := json.Marshal(callResult.Content)
		if marshalErr != nil {
			errors = append(errors, fmt.Sprintf("%s: failed to encode MCP result as JSON; result cannot be added to context: %v", callDescription, marshalErr))
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
			errors = append(errors, fmt.Sprintf("%s: tool execution failed (MCP isError=true): %s", callDescription, strings.Join(messages, "\n")))
		}
	}
	if len(errors) > 0 && result.Arguments != "" {
		errors = append(errors, "Tool arguments: "+result.Arguments)
	}
	result.Error = strings.Join(errors, "\n")
	result.Status = filemcp.ToolCallSuceessStatus
	if len(errors) > 0 {
		result.Status = filemcp.ToolCallFailedStatus
	}

	// Remove short-lived results and append the new result under one lock:
	// tool calls may complete concurrently.
	rc.UpdateToolResults(func(previous []*fileContext.ToolResult) []*fileContext.ToolResult {
		next := make([]*fileContext.ToolResult, 0, len(previous)+1)
		for _, ts := range previous {
			if ts == nil {
				continue
			}
			if _, ok := reserveOnceResultTool[config.McpToolName(ts.ToolName)]; ok {
				continue
			}
			next = append(next, ts)
		}
		return append(next, result)
	})
	// 错误独立于结果详情打印，确保 error 级别也能看到可恢复的工具失败。
	if result.Error != "" {
		log.Printf("[react] tool calling error: %s", result.Error)
	}
	if agentConfig == nil || agentConfig.LogLevel.InfoEnabled() {
		log.Printf("[react] tool calling result: %+v", result)
	}
	if config.McpToolName(toolParam.Name) == config.ApplyPatchMCPToolName || config.McpToolName(toolParam.Name) == config.ExecMCPToolName {
		// exec 的参数和原始结果随快照交给观察器，由 LLM 筛选影响后续决策的新增信息。
		// Observation is a separate processor and must use its own resolved
		// settings rather than inheriting ReAct's model or custom prompt.
		observerConfig := agentConfig.ForAgent("observe")
		r.observe(result, rc, observerConfig)
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
