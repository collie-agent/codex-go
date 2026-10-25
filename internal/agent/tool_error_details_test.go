package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStructuredToolErrorReachesModel(t *testing.T) {
	r := &ReactProcess[ReactResult]{}
	rc := fileContext.NewReactContext(nil)
	r.updateContext(&sdk.CallToolResult{IsError: true, StructuredContent: map[string]any{"error": "invalid glob: unclosed bracket"}}, &sdk.CallToolParams{Name: "glob", Arguments: map[string]any{"pattern": "["}}, rc, nil, nil)
	_, results := rc.FormatForModel()
	if len(results) != 1 || results[0].Status != filemcp.ToolCallFailedStatus || !strings.Contains(results[0].Error, "invalid glob: unclosed bracket") {
		t.Fatalf("structured diagnostic lost: %+v", results)
	}
}

func TestMissingToolResultIsFailure(t *testing.T) {
	r := &ReactProcess[ReactResult]{}
	rc := fileContext.NewReactContext(nil)
	r.updateContext(nil, &sdk.CallToolParams{Name: "read_file"}, rc, nil, nil)
	if got := rc.ToolResults[0]; got.Status != filemcp.ToolCallFailedStatus || !strings.Contains(got.Error, "no result") {
		t.Fatalf("empty response treated as success: %+v", got)
	}
}

func TestValidationPreservesSchemaErrorAlongsideHints(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{"path": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"}}}
	err := validateToolArguments(schema, map[string]any{"limit": "200"})
	if err == nil || !strings.Contains(err.Error(), "/properties/limit") || !strings.Contains(err.Error(), "limit must be a JSON integer") {
		t.Fatalf("schema details lost: %v", err)
	}
}

func TestWorkerInvalidCallDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, tool, args, want string }{
		{"syntax", "read_file", `{"path":`, "decode tool"},
		{"null", "read_file", "null", "must be a JSON object"},
		{"disallowed", "unknown", `{}`, "allowed tools: [read_file]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_invalid", "name": tc.tool, "arguments": tc.args}})
			}))
			defer server.Close()
			worker := NewWorker[ReactResult](t.Context(), &config.AgentConfig{
				AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}},
				LLMConfig:  &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
			}, nil)
			_, _, calls, err := worker.Run(workerTestContext{request: fileContext.ModelRequest{RunID: "diagnostics", AllowTools: true}})
			var callErr *toolCallError
			if !errors.As(err, &callErr) || len(calls) != 0 {
				t.Fatalf("expected recoverable rejection: calls=%v err=%v", calls, err)
			}
			for _, want := range []string{tc.want, `call_id="call_invalid"`, "raw arguments: " + tc.args} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("missing %q: %v", want, err)
				}
			}
		})
	}
}
