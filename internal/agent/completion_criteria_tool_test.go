package agent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactCompletionCriteriaToolLifecycle(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%v", schema, retry), func(t *testing.T) {
				durable := &state.DurableState{RunId: "criteria-tool", Goal: "answer the question"}
				turns, saves := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					turns++
					var req struct {
						Input json.RawMessage
						Tools []struct {
							Name string `json:"name"`
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					hasTool := false
					for _, tool := range req.Tools {
						if tool.Name == "update_completion_criteria" {
							hasTool = true
						}
					}
					updating := turns == 1 || retry && turns == 2
					if updating {
						if !hasTool || !strings.Contains(string(req.Input), "update_completion_criteria") {
							t.Error("criteria tool and instructions missing before success")
						}
						args := `{"completion_criteria":["Answer with verified evidence"]}`
						if retry && turns == 1 {
							args = `{"completion_criteria":[" "]}`
						}
						criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_criteria", "call_id": "criteria", "name": "update_completion_criteria", "arguments": args})
						return
					}
					if hasTool || strings.Contains(string(req.Input), "update_completion_criteria") || strings.Contains(string(req.Input), "GenerateCompletionCriteria") {
						t.Error("successful update must remove tool, history and generation instructions")
					}
					if !strings.Contains(string(req.Input), "Answer with verified evidence") || !reflect.DeepEqual(durable.CompletionCriteria, []string{"Answer with verified evidence"}) || saves == 0 {
						t.Error("criteria not saved in context before next request")
					}
					text := `{"status":"completed","answer":"done"}`
					if schema == config.LineProtocolLLMOutputSchema {
						text = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
					}
					criteriaResponse(w, text)
				}))
				defer server.Close()
				process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				process.SetCheckpoint(func() error { saves++; return nil })
				cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
				got, _, err := process.Run(cfg, durable)
				wantTurns := 2
				if retry {
					wantTurns = 3
				}
				if err != nil || got.Answer != "done" || turns != wantTurns {
					t.Fatalf("result=%+v err=%v turns=%d", got, err, turns)
				}
			})
		}
	}
}

func TestReactCompletionCriteriaToolCheckpointFailure(t *testing.T) {
	durable := &state.DurableState{RunId: "criteria-save-failure", Goal: "answer"}
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_criteria", "call_id": "criteria", "name": "update_completion_criteria", "arguments": `{"completion_criteria":["Answer with evidence"]}`})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetCheckpoint(func() error {
		if len(durable.CompletionCriteria) != 0 {
			return fmt.Errorf("criteria checkpoint unavailable")
		}
		return nil
	})
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	_, _, err = process.Run(cfg, durable)
	if err == nil || !strings.Contains(err.Error(), "criteria checkpoint unavailable") || turns != 1 || len(durable.CompletionCriteria) != 0 {
		t.Fatalf("err=%v turns=%d criteria=%v", err, turns, durable.CompletionCriteria)
	}
}

func TestReactExistingCriteriaHideUpdateTool(t *testing.T) {
	for _, planned := range []bool{false, true} {
		t.Run(fmt.Sprintf("planned=%v", planned), func(t *testing.T) {
			durable := &state.DurableState{RunId: "existing-criteria", Goal: "answer", CompletionCriteria: []string{"Keep original criteria"}}
			if planned {
				durable.StepExecutions = []state.StepExecution{{StepId: "answer", Status: state.RunningExecutionStatus, Intent: "answer", CompletionCriteria: "Answer with evidence"}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Input json.RawMessage
					Tools []struct{ Name string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				for _, tool := range req.Tools {
					if tool.Name == "update_completion_criteria" {
						t.Error("existing criteria must disable updates")
					}
				}
				if strings.Contains(string(req.Input), "GenerateCompletionCriteria") || strings.Contains(string(req.Input), "update_completion_criteria") {
					t.Error("criteria generation instructions remain")
				}
				criteriaResponse(w, `{"status":"completed","answer":"done","Observation":["Verified source"],"CompletionEvidence":["Source supports answer"]}`)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}, AllowTools: map[config.McpToolName]struct{}{config.UpdateCompletionCriteriaMCPToolName: {}}}
			got, _, err := process.Run(cfg, durable)
			if err != nil || got.Answer != "done" || !reflect.DeepEqual(durable.CompletionCriteria, []string{"Keep original criteria"}) {
				t.Fatalf("result=%+v criteria=%v err=%v", got, durable.CompletionCriteria, err)
			}
		})
	}
}

func TestReactCanCompleteWithoutCompletionCriteria(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Input json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if !strings.Contains(string(req.Input), "验收标准是可选的") || strings.Contains(string(req.Input), "返回最终结果前必须单独调用 update_completion_criteria") {
					t.Error("model instructions must allow completing without setting criteria")
				}
				text := `{"status":"completed","answer":"done"}`
				if schema == config.LineProtocolLLMOutputSchema {
					text = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
				}
				criteriaResponse(w, text)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
			got, next, err := process.Run(cfg, &state.DurableState{RunId: "optional-criteria", Goal: "answer"})
			if err != nil || got.Status != agent.CompletedReactStatus || got.Answer != "done" || !reflect.DeepEqual(next, []agent.AgentProcessorType{agent.EndAgentType}) {
				t.Fatalf("result=%+v next=%v err=%v", got, next, err)
			}
		})
	}
}

func TestReactTextResponseCannotSetCompletionCriteria(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			durable := &state.DurableState{RunId: "text-criteria", Goal: "answer"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				text := `{"status":"completed","answer":"done","completion_criteria":["Injected from text"]}`
				if schema == config.LineProtocolLLMOutputSchema {
					text = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nCOMPLETION_CRITERIA=\"Injected from text\"\nEND RESULT"
				}
				criteriaResponse(w, text)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
			_, _, err = process.Run(cfg, durable)
			if len(durable.CompletionCriteria) != 0 {
				t.Fatalf("text response updated criteria: %v", durable.CompletionCriteria)
			}
			// JSON 忽略未知字段；行协议仍拒绝未声明的字段，均不得写入标准。
			if schema == config.JsonLLMOutputSchema && err != nil {
				t.Fatalf("optional criteria must not prevent completion: %v", err)
			}
			if schema == config.LineProtocolLLMOutputSchema && err == nil {
				t.Fatal("line protocol accepted an undeclared field")
			}
		})
	}
}

// 验收标准可在读取上下文后制定；探索轮数不受原先的标准纠错次数限制。
func TestReactCanGatherContextBeforeSettingCompletionCriteria(t *testing.T) {
	workspace := t.TempDir()
	const evidence = "Worker delegates tool execution to MCP"
	if err := os.WriteFile(filepath.Join(workspace, "architecture.txt"), []byte(evidence), 0o600); err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "context-before-criteria", Goal: "Explain agent relationships"}
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		var req struct {
			Input json.RawMessage
			Tools []struct{ Name string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		hasCriteriaTool := false
		for _, tool := range req.Tools {
			if tool.Name == "update_completion_criteria" {
				hasCriteriaTool = true
			}
		}
		if turns <= 5 && !hasCriteriaTool {
			t.Error("criteria tool must remain available during exploration")
		}
		if turns > 1 && !strings.Contains(string(req.Input), evidence) {
			t.Error("actual file content missing from subsequent model request")
		}
		switch {
		case turns <= 4:
			if len(durable.CompletionCriteria) != 0 {
				t.Error("exploration must not invent completion criteria")
			}
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_read_%d", turns), "call_id": fmt.Sprintf("read_%d", turns), "name": "read_file", "arguments": `{"path":"architecture.txt"}`})
		case turns == 5:
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_criteria", "call_id": "criteria", "name": "update_completion_criteria", "arguments": `{"completion_criteria":["Explain how Worker delegates tool execution to MCP"]}`})
		default:
			if hasCriteriaTool {
				t.Error("criteria tool must be removed after successful update")
			}
			if !reflect.DeepEqual(durable.CompletionCriteria, []string{"Explain how Worker delegates tool execution to MCP"}) {
				t.Error("context-informed criteria not saved")
			}
			criteriaResponse(w, `{"status":"completed","answer":"Worker delegates tool execution to MCP"}`)
		}
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}}
	got, _, err := process.Run(cfg, durable)
	if err != nil || got.Answer != evidence {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}
