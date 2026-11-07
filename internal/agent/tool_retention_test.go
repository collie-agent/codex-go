package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactExpiresDiscoveryResultsAndRetainsOtherToolHistory(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("first\nsecond\nthird\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		turn++
		var request struct {
			Input []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			} `json:"input"`
		}
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		for _, id := range []string{"discovery_glob", "discovery_list_dir"} {
			calls, outputs := 0, 0
			for _, item := range request.Input {
				if item.CallID == id {
					switch item.Type {
					case "function_call":
						calls++
					case "function_call_output":
						outputs++
						if item.Output == "" {
							t.Error("empty discovery result")
						}
					}
				}
			}
			want := 0
			if turn == 2 {
				want = 1
			}
			if calls != want || outputs != want {
				t.Errorf("turn %d: %s call/result counts = %d/%d, want %d/%d", turn, id, calls, outputs, want, want)
			}
		}
		// 非目录检索工具的调用及结果持续保留在模型视图。
		for readTurn := 2; readTurn < turn; readTurn++ {
			id := fmt.Sprintf("read_%d", readTurn)
			calls, outputs := 0, 0
			for _, item := range request.Input {
				if item.CallID == id {
					switch item.Type {
					case "function_call":
						calls++
					case "function_call_output":
						outputs++
					}
				}
			}
			if calls != 1 || outputs != 1 {
				t.Errorf("turn %d: %s call/result counts = %d/%d, want 1/1", turn, id, calls, outputs)
			}
		}
		call := func(id, name, args string) any {
			return map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": args}
		}
		switch turn {
		case 1:
			writeObservationResponse(w, []any{
				call("discovery_glob", "glob", `{"pattern":"*.txt"}`),
				call("discovery_list_dir", "list_dir", `{"path":"."}`),
			})
		case 2, 3, 4:
			writeObservationResponse(w, []any{call(fmt.Sprintf("read_%d", turn), "read_file", fmt.Sprintf(`{"path":"sample.txt","offset":%d,"limit":1}`, turn-1))})
		default:
			writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"done"}`)})
		}
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{
		LLMConfig:  &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
		AllowTools: map[config.McpToolName]struct{}{config.GlobMCPToolName: {}, config.ListDirMCPToolName: {}, config.ReadFileMCPToolName: {}},
	}
	durable := &fileContext.DurableState{CompletionCriteria: []string{"original task verified"}, RunId: "retention", Goal: "inspect workspace"}
	registered := false
	process.SetCheckpoint(func() error {
		if len(durable.ToolExecutions) == 2 && durable.ToolExecutions[0].Status == "" && durable.ToolExecutions[1].Status == "" {
			registered = true
		}
		return nil
	})
	result, _, err := process.Run(cfg, durable)
	if len(durable.ToolExecutions) != 5 || durable.ToolExecutions[0].Result == "" || durable.ToolExecutions[1].Result == "" {
		t.Error("complete execution history was not retained")
	}
	if !registered {
		t.Error("batch not registered before execution")
	}
	if err != nil || result.Answer != "done" || turn != 5 {
		t.Fatalf("Run = %+v, %v; requests = %d", result, err, turn)
	}
}
