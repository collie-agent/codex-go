package agent_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactRepeatedSearchRecoversAndCompletes(t *testing.T) {
	for _, mode := range []string{"grep", "glob", "eof", "mixed", "exact_grep", "exact_glob", "after_eof"} {
		t.Run(mode, func(t *testing.T) { repeatedSearchRecovers(t, mode) })
	}
}

func repeatedSearchRecovers(t *testing.T, mode string) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("needle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turns, recovered := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		turns++
		if strings.Contains(string(body), "investigation recovery") {
			recovered = true
			if mode == "after_eof" && turns != 4 {
				t.Errorf("EOF was already observed: recovery turn=%d, want 4", turns)
			}
			criteriaResponse(w, `{"status":"completed","answer":"needle located"}`)
			return
		}
		if turns > 8 {
			criteriaResponse(w, `{"status":"failed","answer":"no recovery"}`)
			return
		}
		name, args := "grep", fmt.Sprintf(`{"pattern":"needle","limit":%d}`, 20+turns)
		if mode == "exact_grep" {
			args = `{"pattern":"needle"}`
		}
		if mode == "exact_glob" {
			name, args = "glob", `{"pattern":"*.txt"}`
		}
		if mode == "glob" {
			name, args = "glob", fmt.Sprintf(`{"pattern":"*.txt","limit":%d}`, 20+turns)
		}
		if mode == "eof" || (mode == "after_eof" && turns > 1) {
			args = fmt.Sprintf(`{"pattern":"needle","offset":%d,"limit":%d}`, 10+turns, 20+turns)
		}
		calls := []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": name, "arguments": args}}
		if mode == "mixed" {
			calls = append(calls, map[string]any{"type": "function_call", "id": fmt.Sprintf("file_%d", turns), "call_id": fmt.Sprintf("file_%d", turns), "name": "read_file", "arguments": `{"path":"a.txt"}`})
		}
		criteriaResponse(w, "", calls...)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
	durable := &state.DurableState{RunId: "search", Goal: "locate needle"}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.GrepMCPToolName: {}, config.GlobMCPToolName: {}, config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err != nil || result.Status != agent.CompletedReactStatus || !recovered {
		t.Fatalf("result=%+v recovered=%v error=%v", result, recovered, err)
	}
}

func TestReactSearchNewEvidenceContinuesWithoutRecovery(t *testing.T) {
	for _, mode := range []string{"pagination", "empty_scopes", "expressions", "source_change"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("needle\nneedle\nneedle\nneedle\nneedle\nneedle\nstate0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "empty_scopes" {
				for i := 1; i <= 6; i++ {
					if err := os.Mkdir(filepath.Join(workspace, fmt.Sprintf("dir%d", i)), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			turns := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "investigation recovery") {
					t.Error("new search evidence incorrectly triggered recovery")
				}
				turns++
				last := 7
				if mode == "source_change" {
					last = 12
				}
				if turns == last {
					criteriaResponse(w, `{"status":"completed","answer":"all evidence collected"}`)
					return
				}
				name, args := "grep", fmt.Sprintf(`{"pattern":"needle","limit":1,"offset":%d}`, turns-1)
				switch mode {
				case "empty_scopes":
					args = fmt.Sprintf(`{"pattern":"absent","path":"dir%d"}`, turns)
				case "expressions":
					args = fmt.Sprintf(`{"pattern":"absent%d"}`, turns)
				case "source_change":
					if turns%2 == 0 {
						name = "apply_patch"
						n := turns / 2
						args = fmt.Sprintf(`{"patch":"--- a.txt\n+++ a.txt\n@@ -7 +7 @@\n-state%d\n+state%d\n"}`, n-1, n)
					} else {
						args = `{"pattern":"needle"}`
					}
				}
				criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": name, "arguments": args})
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.GrepMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
			result, _, err := process.Run(cfg, &state.DurableState{RunId: "fresh-search", Goal: "investigate search evidence"})
			if err != nil || result.Status != agent.CompletedReactStatus {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}
