package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type resultReaderFunc func(context.Context, string, string, string) (*state.ToolResult, error)

func (f resultReaderFunc) ToolResult(ctx context.Context, run, execution, call string) (*state.ToolResult, error) {
	return f(ctx, run, execution, call)
}

func TestReadToolResultUsesStore(t *testing.T) {
	for _, name := range []string{"glob", "list_dir"} {
		t.Run(name, func(t *testing.T) {
			reader := resultReaderFunc(func(_ context.Context, run, execution, call string) (*state.ToolResult, error) {
				if run != "run" || execution != "execution" || call != "call" {
					t.Fatalf("wrong scope: %s/%s/%s", run, execution, call)
				}
				return &state.ToolResult{ToolCallId: call, ToolName: name, Status: "success", Result: "甲乙丙"}, nil
			})
			process := &BaseReactProcess[ReactResult]{ctx: WithToolResultReader(t.Context(), reader)}
			durable := &state.DurableState{RunId: "run", ExecutionID: "execution"}
			params := &sdk.CallToolParams{Arguments: map[string]any{"call_id": "call", "offset": 1, "limit": 1}}
			result := process.readToolResult(params, durable)
			if result.IsError || !strings.Contains(result.Content[0].(*sdk.TextContent).Text, `"content":"乙"`) {
				t.Fatalf("result=%+v", result)
			}
			process.ctx = WithToolResultReader(t.Context(), resultReaderFunc(func(context.Context, string, string, string) (*state.ToolResult, error) {
				return nil, errors.New("database unavailable")
			}))
			durable.ToolExecutions = []*state.ToolResult{{ToolCallId: "call", Status: "success", Result: "stale"}}
			if result := process.readToolResult(params, durable); !result.IsError {
				t.Fatal("database failure silently used memory")
			}
		})
	}
}

func TestCachedDuplicateResultPreservesOutputAndPagination(t *testing.T) {
	for _, tc := range []struct {
		name, output, failure string
		truncated             bool
	}{
		{name: "success", output: `[{"type":"text","text":"main.go"}]`},
		{name: "failed", failure: "permission denied"},
		{name: "unicode preview", output: strings.Repeat("甲", duplicateResultPreviewLimit+1), truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			process := &BaseReactProcess[ReactResult]{ctx: t.Context()}
			durable := &state.DurableState{ToolExecutions: []*state.ToolResult{{ToolCallId: "original", ToolName: "list_dir", Status: "success", Result: tc.output, Error: tc.failure}}}
			result := process.cachedDuplicateResult(durable, "original", "duplicate", "list_dir", `{"path":"."}`)
			if result.ToolCallId != "duplicate" || result.Error != tc.failure {
				t.Fatalf("result=%+v", result)
			}
			if tc.truncated {
				if !strings.HasPrefix(result.Result, strings.Repeat("甲", duplicateResultPreviewLimit)+"\n\n") || !strings.Contains(result.Result, `call_id="original", offset=4096, limit=4096`) {
					t.Fatal("missing Unicode-safe preview or pagination instructions")
				}
			} else if result.Result != tc.output {
				t.Fatalf("output=%q, want %q", result.Result, tc.output)
			}
			if len(durable.ToolExecutions) != 1 {
				t.Fatal("cached response was persisted as an execution")
			}
		})
	}
}
