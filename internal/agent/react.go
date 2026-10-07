package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
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
}

// support dag orchestrator
type ReactProcess[T LLMSchema] struct {
	ctx           context.Context
	maxTurn       int
	durableState  *fileContext.DurableState
	agentConfig   *config.AgentConfig
	toolConfig    filemcp.Config
	mcpClientSess *mcpSdk.ClientSession
	serverSess    *mcpSdk.ServerSession
}

// 最大轮数固定在代码中，避免任务通过配置无限延长。
const maxReactTurns = 100

func NewReactProcess[T LLMSchema](ctx context.Context, durableState *fileContext.DurableState, agentConfig *config.AgentConfig, toolConfig filemcp.Config) (*ReactProcess[T], error) {
	clientTransport, serverTransport := mcpSdk.NewInMemoryTransports()
	return newReactProcess[T](ctx, durableState, agentConfig, toolConfig, clientTransport, serverTransport)
}

// NewReactProcessWithCommand connects to a separately built MCP server over stdio.
func NewReactProcessWithCommand[T LLMSchema](ctx context.Context, durableState *fileContext.DurableState, agentConfig *config.AgentConfig, command *exec.Cmd) (*ReactProcess[T], error) {
	if command == nil {
		return nil, fmt.Errorf("MCP command is required")
	}
	client := mcpSdk.NewClient(&mcpSdk.Implementation{Name: "codex-go-responses", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcpSdk.CommandTransport{Command: command}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to MCP command: %w", err)
	}
	return &ReactProcess[T]{ctx: ctx, maxTurn: maxReactTurns, durableState: durableState,
		agentConfig: agentConfig, mcpClientSess: session}, nil
}

func newReactProcess[T LLMSchema](ctx context.Context, durableState *fileContext.DurableState, agentConfig *config.AgentConfig, toolConfig filemcp.Config, clientTransport, serverTransport mcpSdk.Transport) (*ReactProcess[T], error) {
	if ctx == nil {
		return nil, fmt.Errorf("agent context is required")
	}
	if durableState == nil {
		return nil, fmt.Errorf("durable state is required")
	}
	if agentConfig == nil || agentConfig.LLMConfig == nil {
		return nil, fmt.Errorf("agent config with LLM config is required")
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
		durableState:  durableState,
		agentConfig:   agentConfig,
		toolConfig:    toolConfig,
		mcpClientSess: mcpClientSess,
		serverSess:    serverSess,
	}, nil
}

func (r *ReactProcess[T]) Run() (T, error) {
	var zero T
	if r == nil || r.ctx == nil || r.durableState == nil || r.agentConfig == nil {
		return zero, fmt.Errorf("invalid ReAct process")
	}
	if r.mcpClientSess != nil {
		defer r.mcpClientSess.Close()
	}
	if r.serverSess != nil {
		defer r.serverSess.Close()
	}
	rc := fileContext.NewReactContext(r.durableState)
	// 先保留内置 ReAct 执行约束，再把 YAML 的长期约束追加到其后。
	rc.SetSysPrompt(r.agentConfig.SysPrompt)
	worker := NewWorker[T](r.ctx, r.agentConfig, r.mcpClientSess)
	for i := 1; i <= r.maxTurn; i++ {
		result, _, toolParams, err := worker.Run(rc)
		if err != nil {
			return zero, fmt.Errorf("ReAct turn %d: %w", i, err)
		}
		if len(toolParams) == 0 {
			if reactResult, ok := any(result).(ReactResult); ok {
				// 格式解析不校验业务约束：仅明确完成且有答案时才允许成功退出。
				switch reactResult.Status {
				case FailedReactStatus:
					return zero, fmt.Errorf("ReAct reported failure: %s", reactResult.Answer)
				case CompletedReactStatus:
					if strings.TrimSpace(reactResult.Answer) == "" {
						return zero, fmt.Errorf("ReAct completed result requires a non-empty answer")
					}
				default:
					return zero, fmt.Errorf("ReAct returned invalid status %q", reactResult.Status)
				}
			}
			return result, nil
		}
		r.batchToolCallings(toolParams, rc)
		if r.ctx.Err() != nil {
			return zero, r.ctx.Err()
		}
	}
	return zero, fmt.Errorf("ReAct exhausted maximum turns (%d)", r.maxTurn)
}

func (r *ReactProcess[T]) observe(toolResult *fileContext.ToolResult, rc *fileContext.ReactContext) {
	if r == nil || r.ctx == nil || rc == nil || toolResult == nil || r.ctx.Err() != nil {
		return
	}
	// Copy the inputs before starting the request: the next ReAct turn and
	// other observers may update the context while this request is running.
	observationContext := rc.SnapshotObservationContext(toolResult)
	go func() {
		observer := NewObserveProcess(r.ctx, r.agentConfig)
		result, err := observer.Run(observationContext)
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

func (r *ReactProcess[T]) batchToolCallings(toolParams []*mcpSdk.CallToolParams, rc *fileContext.ReactContext) {
	var wg sync.WaitGroup
	for _, toolParam := range toolParams {
		wg.Go(func() {
			callResult, err := r.mcpClientSess.CallTool(r.ctx, toolParam)
			r.updateContext(callResult, toolParam, rc, err)
		})
	}
	wg.Wait()
}

func (r *ReactProcess[T]) updateContext(callResult *mcpSdk.CallToolResult, toolParam *mcpSdk.CallToolParams, rc *fileContext.ReactContext, err error) {
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
	log.Printf("tool result: %+v\nReact context:\n%s", result, rc.Format())
	if config.McpToolName(toolParam.Name) == config.ApplyPatchMCPToolName {
		r.observe(result, rc)
	}
}
