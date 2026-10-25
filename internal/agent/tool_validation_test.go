package agent

import (
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	mcpSdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolArgumentValidationFeedbackAndRetry(t *testing.T) {
	r, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.mcpClientSess.Close()
	defer r.serverSess.Close()
	rc := fileContext.NewReactContext(nil)
	call := &mcpSdk.CallToolParams{Name: "list_dir", Meta: mcpSdk.Meta{toolCallIDMetaKey: "call_invalid"},
		Arguments: map[string]any{"depth": "1", "limit": "200", "offset": 0, "path": "."}}
	r.batchToolCallings([]*mcpSdk.CallToolParams{call}, rc, &config.AgentConfig{})
	_, results := rc.FormatForModel()
	if len(results) != 1 {
		t.Fatalf("expected validation feedback, got %+v", results)
	}
	for _, want := range []string{"depth must be a JSON integer", "limit must be a JSON integer", "not a quoted string", "tool was not executed"} {
		if !strings.Contains(results[0].Error, want) {
			t.Errorf("feedback missing %q: %s", want, results[0].Error)
		}
	}
	if results[0].Status != filemcp.ToolCallFailedStatus || results[0].ToolCallId != "call_invalid" || !strings.Contains(results[0].Arguments, `"depth":"1"`) {
		t.Fatalf("invalid call was not retained for the next model turn: %+v", results[0])
	}
	call.Arguments = map[string]any{"depth": 1, "limit": 200, "offset": 0, "path": "."}
	call.Meta[toolCallIDMetaKey] = "call_corrected"
	r.batchToolCallings([]*mcpSdk.CallToolParams{call}, rc, &config.AgentConfig{})
	_, results = rc.FormatForModel()
	if len(results) != 1 || results[0].Status != filemcp.ToolCallSuceessStatus || results[0].Error != "" || results[0].Result == "" {
		t.Fatalf("corrected call failed: %+v", results)
	}
}

func TestValidateToolArgumentsUsesFullSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object", "required": []string{"count"}, "additionalProperties": false,
		"properties": map[string]any{"count": map[string]any{"type": []string{"null", "integer"}, "minimum": 1}},
	}
	for _, args := range []any{map[string]any{}, map[string]any{"count": 0}, map[string]any{"count": 1.5}, map[string]any{"count": 1, "extra": true}} {
		if err := validateToolArguments(schema, args); err == nil {
			t.Errorf("invalid arguments accepted: %+v", args)
		}
	}
	for _, args := range []any{map[string]any{"count": nil}, map[string]any{"count": 2}} {
		if err := validateToolArguments(schema, args); err != nil {
			t.Errorf("valid arguments rejected: %v", err)
		}
	}
}

func TestAllToolSchemasRejectWrongArgumentTypes(t *testing.T) {
	r, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.mcpClientSess.Close()
	defer r.serverSess.Close()
	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"switch_to_plan_execute": {map[string]any{"reason": 123, "summary": "remaining work"}, "invalid arguments"},
		"list_dir":               {map[string]any{"path": ".", "depth": "1"}, "depth must be a JSON integer"},
		"read_file":              {map[string]any{"path": "README.md", "offset": "1", "limit": "200"}, "limit must be a JSON integer"},
		"glob":                   {map[string]any{"pattern": "**/*.go", "include_ignored": "false"}, "include_ignored must be a JSON boolean"},
		"grep":                   {map[string]any{"pattern": "TODO", "literal": "true", "limit": "200"}, "literal must be a JSON boolean"},
		"apply_patch":            {map[string]any{"patch": 123}, "invalid arguments"},
		"exec":                   {map[string]any{"command": "git", "args": "status"}, "invalid arguments"},
	}
	for tool, err := range r.mcpClientSess.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		tc, ok := cases[tool.Name]
		if !ok {
			t.Fatalf("tool %s has no validation case", tool.Name)
		}
		t.Run(tool.Name, func(t *testing.T) {
			if err := validateToolArguments(tool.InputSchema, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "resubmit") {
				t.Fatalf("expected actionable validation error containing %q, got %v", tc.want, err)
			}
			if !strings.Contains(tool.Description, "Example arguments:") {
				t.Error("tool description missing typed argument example")
			}
		})
		delete(cases, tool.Name)
	}
	if len(cases) != 0 {
		t.Fatalf("missing tools: %v", cases)
	}
}
