package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
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
	ctx           context.Context
	maxTurn       int
	toolConfig    filemcp.Config
	mcpClientSess *mcpSdk.ClientSession
	serverSess    *mcpSdk.ServerSession
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
	rc := fileContext.NewReactContext(durableState)
	// 先保留内置 ReAct 执行约束，再把 YAML 的长期约束追加到其后。
	rc.SetSysPrompt(agentConfig.SysPrompt)
	worker := NewWorker[T](r.ctx, agentConfig, r.mcpClientSess)
	formatErrors := 0
	for i := 1; i <= r.maxTurn; i++ {
		var result T
		var toolParams []*mcpSdk.CallToolParams
		// 格式纠错留在当前轮，避免最后一轮的可恢复错误直接耗尽执行预算。
		for {
			if err := r.ctx.Err(); err != nil {
				return zero, nil, err
			}
			var err error
			result, _, toolParams, err = worker.Run(rc)
			if err == nil {
				break
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
		r.batchToolCallings(toolParams, rc, agentConfig)
		if r.ctx.Err() != nil {
			return zero, nil, r.ctx.Err()
		}
	}
	return zero, nil, fmt.Errorf("ReAct exhausted maximum turns (%d)", r.maxTurn)
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
					err = fmt.Errorf("unknown tool %q; choose an available tool", toolParam.Name)
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
	if callResult != nil {
		encoded, marshalErr := json.Marshal(callResult)
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
	// Log the recorded result and the resulting ReAct context together so
	// concurrent tool calls remain distinguishable in the process output.
	log.Printf("tool result: %+v\nReact context:\n%s", result, rc.FormatForLog())
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
