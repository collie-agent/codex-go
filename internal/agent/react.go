package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	Status ReactStatus
	Answer string
}

/*
for循环worker
处理worker的返回:
针对不同的tool,更新react context
需要observation的,异步请求
tool calling error的,将error message加回到llm context
*/
// support dag orchestrator
type ReactProcess[T LLMSchema] struct {
	ctx           context.Context
	maxTurn       int
	durableState  *fileContext.DurableState
	agentConfig   *config.AgentConfig
	toolConfig    filemcp.Config
	mcpClientSess *mcpSdk.ClientSession
}

func NewReactProcess[T LLMSchema](ctx context.Context, durableState *fileContext.DurableState, agentConfig *config.AgentConfig, toolConfig filemcp.Config) (*ReactProcess[T], error) {
	clientTransport, _ := mcpSdk.NewInMemoryTransports()
	mcpClient := mcpSdk.NewClient(&mcpSdk.Implementation{Name: "codex-go-responses", Version: "0.1.0"}, nil)
	mcpClientSess, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect MCP client: %w", err)
	}
	return &ReactProcess[T]{
		ctx:           ctx,
		mcpClientSess: mcpClientSess,
	}, nil
}

func (r *ReactProcess[T]) Run() {
	rc := fileContext.NewReactContext(r.durableState)
	worker := NewWorker[T](r.ctx, r.agentConfig, r.mcpClientSess)
	for i := 1; i <= r.maxTurn; i++ {
		result, reasoning, toolParams, err := worker.Run(rc)
		fmt.Printf("worker推理结果:%+v,%+v,err:%+v\n", result, reasoning, err)
		if err != nil {
			return
		}
		r.batchToolCallings(toolParams, rc)
		if r.ctx.Err() != nil {
			return
		}
	}
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
	if config.McpToolName(toolParam.Name) == config.ApplyPatchMCPToolName {
		r.observe(result, rc)
	}
}
