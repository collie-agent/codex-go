package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func runCommandExploration(t *testing.T, workspace string, outputs [][]any, policies ...config.ExplorationBudget) (*state.DurableState, error) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		if calls >= len(outputs) {
			writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"script exhausted"}`)})
			return
		}
		output := outputs[calls]
		calls++
		raw, _ := json.Marshal(output)
		if strings.Contains(string(raw), "$session") {
			id := regexp.MustCompile(`\b[a-f0-9]{32}\b`).FindString(string(body.Input))
			if id == "" {
				t.Error("missing session identity in model input")
			}
			raw = []byte(strings.ReplaceAll(string(raw), "$session", id))
			_ = json.Unmarshal(raw, &output)
		}
		writeObservationResponse(w, output)
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{ExecutionID: "commands", Goal: "fix main.go and test", CompletionCriteria: []string{"changed and verified"}}
	policy := config.ExplorationBudget{}
	if len(policies) > 0 {
		policy = policies[0]
	}
	result, _, err := process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}, config.ExecStartMCPToolName: {}, config.ExecPollMCPToolName: {}, config.ExecStopMCPToolName: {}, config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil && result.Status != CompletedReactStatus {
		err = fmt.Errorf("unexpected result: %+v", result)
	}
	return durable, err
}

func TestReactCommandModificationAndVerificationComplete(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationHandoff(),
		modificationTool("edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/package old/package main/", "main.go"}}),
		modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"changed and tested"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Exploration struct {
			Used                  int
			ModificationConfirmed bool
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Exploration.ModificationConfirmed || saved.Exploration.Used != 0 {
		t.Fatalf("unexpected accounting: %s", durable.ExecutionProgress)
	}
	content, _ := os.ReadFile(filepath.Join(workspace, "main.go"))
	if string(content) != "package main\n" {
		t.Fatalf("content=%q", content)
	}
}

func TestReactCommandInvestigationInWindowConsumesExploration(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationHandoff(),
		modificationTool("search", "exec", map[string]any{"command": "rg", "args": []string{"old", "main.go"}}),
		modificationTool("edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/old/new/", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"fixed"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct{ Exploration struct{ Used int } }
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 {
		t.Fatalf("search must consume one turn: %s", durable.ExecutionProgress)
	}
}

func TestReactMixedCommandBatchChargesOneInvestigation(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	batch := append(modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}), modificationTool("search", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}})...)
	durable, err := runCommandExploration(t, workspace, [][]any{batch, {observationMessage(`{"status":"completed","answer":"inspected"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Exploration struct {
			Used       int
			Accounting []struct {
				Batch   int
				Purpose string
			}
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 || len(saved.Exploration.Accounting) != 1 || saved.Exploration.Accounting[0].Purpose != "investigation" {
		t.Fatalf("missing batch accounting: %s", durable.ExecutionProgress)
	}
}

func TestReactUnknownTargetSnapshotIsRecorded(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "main.go"), 0700); err != nil {
		t.Fatal(err)
	}
	durable, _ := runCommandExploration(t, workspace, [][]any{
		modificationHandoff(),
		modificationTool("read", "exec", map[string]any{"command": "python3", "args": []string{"-c", "print('no change')"}}),
		{observationMessage(`{"status":"completed","answer":"claimed done"}`)},
	})
	var saved struct {
		Exploration struct {
			ModificationConfirmed bool
			Accounting            []struct{ Outcome string }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.ModificationConfirmed || len(saved.Exploration.Accounting) != 1 || saved.Exploration.Accounting[0].Outcome != "unknown" {
		t.Fatalf("unknown read must be retained: %s", durable.ExecutionProgress)
	}
}

func TestReactTestGeneratedTargetCannotConfirmImplementation(t *testing.T) {
	for _, companion := range []struct {
		name    string
		command string
		args    []string
	}{
		{"readonly", "rg", []string{"package", "main.go"}},
		{"noop-modification", "sed", []string{"-i", "s/package/package/", "main.go"}},
	} {
		t.Run(companion.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			testSource := `package main
import("os"
 "testing")
func TestArtifact(t *testing.T) {if err:=os.WriteFile("main.go",[]byte("package generated\n"),0600);err!=nil{t.Fatal(err)}}
`
			if err := os.WriteFile(filepath.Join(workspace, "artifact_test.go"), []byte(testSource), 0600); err != nil {
				t.Fatal(err)
			}
			batch := append(modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "artifact_test.go"}}), modificationTool("search", "exec", map[string]any{"command": companion.command, "args": companion.args})...)
			durable, err := runCommandExploration(t, workspace, [][]any{modificationHandoff(), batch, {observationMessage(`{"status":"completed","answer":"claimed changed"}`)}})
			if err == nil {
				t.Fatal("test artifact cannot satisfy handoff")
			}
			var saved struct {
				Exploration struct{ ModificationConfirmed bool }
			}
			if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Exploration.ModificationConfirmed {
				t.Fatalf("artifact counted as implementation: %s", durable.ExecutionProgress)
			}
			content, readErr := os.ReadFile(filepath.Join(workspace, "main.go"))
			if readErr != nil || string(content) != "package generated\n" {
				t.Fatalf("test generator did not run: content=%q err=%v", content, readErr)
			}
		})
	}
}

func TestReactFailedTestThenSourceInvestigationChargesOnlyInvestigation(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main_test.go"), []byte("package main\nimport \"testing\"\nfunc TestBroken(t *testing.T){t.Fatal(\"failure\")}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "-run", "TestBroken", "-count=1", "main_test.go"}}),
		modificationTool("investigate", "read_file", map[string]any{"path": "main.go"}),
		{observationMessage(`{"status":"completed","answer":"diagnosed"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Exploration struct {
			Used       int
			Accounting []struct{ Purpose string }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 || len(saved.Exploration.Accounting) != 2 || saved.Exploration.Accounting[0].Purpose != "verification" {
		t.Fatalf("test and investigation conflated: %s", durable.ExecutionProgress)
	}
}

func TestReactExhaustedExplorationCanModifyTestAndComplete(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}),
		modificationHandoff(),
		modificationTool("edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/package old/package main/", "main.go"}}),
		modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"changed and verified"}`)},
	}, policy)
	if err != nil {
		t.Fatalf("verification and completion must not require new exploration: %v; %s", err, durable.ExecutionProgress)
	}
	var saved struct{ Exploration struct{ Used int } }
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 {
		t.Fatalf("budget refreshed or overcharged: %s", durable.ExecutionProgress)
	}
}

func TestReactStoredInvestigationOutputDoesNotBecomeFreeVerification(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("source", "read_file", map[string]any{"path": "main.go"}),
		modificationTool("more", "read_tool_result", map[string]any{"call_id": "source", "offset": 0, "limit": 100}),
		{observationMessage(`{"status":"completed","answer":"inspected"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct{ Exploration struct{ Used int } }
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 2 {
		t.Fatalf("source results must retain investigation purpose: %s", durable.ExecutionProgress)
	}
}

func TestReactSessionWaitRetainsVerifiedCommandPurpose(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("start", "exec_start", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		modificationTool("poll", "exec_poll", map[string]any{"session_id": "$session", "wait_ms": 1000}),
		modificationTool("stop", "exec_stop", map[string]any{"session_id": "$session"}),
		{observationMessage(`{"status":"completed","answer":"checked session"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Exploration struct {
			Used            int
			Accounting      []struct{ Purpose string }
			SessionPurposes map[string]string
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 0 || len(saved.Exploration.Accounting) != 3 || len(saved.Exploration.SessionPurposes) != 1 {
		t.Fatalf("session accounting: %s", durable.ExecutionProgress)
	}
	for _, batch := range saved.Exploration.Accounting {
		if batch.Purpose != "verification" {
			t.Fatalf("lost origin: %s", durable.ExecutionProgress)
		}
	}
}

func TestReactUnknownSessionCannotGrantFreeInvestigation(t *testing.T) {
	durable, err := runCommandExploration(t, t.TempDir(), [][]any{
		modificationTool("poll", "exec_poll", map[string]any{"session_id": "invented", "wait_ms": 0}),
		{observationMessage(`{"status":"completed","answer":"session unavailable"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct{ Exploration struct{ Used int } }
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 {
		t.Fatalf("unknown session granted free turn: %s", durable.ExecutionProgress)
	}
}

func TestReactCommandEvidenceUsesActualTargetContent(t *testing.T) {
	for _, tc := range []struct {
		name, initial, script string
		changed               bool
	}{
		{"create", "", `from pathlib import Path; Path('main.go').write_text('new\n')`, true},
		{"delete", "old\n", `from pathlib import Path; Path('main.go').unlink()`, true},
		{"existing-dirty-content", "dirty\n", `from pathlib import Path; print(Path('main.go').read_text())`, false},
		{"same-content", "old\n", `from pathlib import Path; Path('main.go').write_text('old\n')`, false},
		{"unrelated-output", "old\n", `from pathlib import Path; Path('test-output.txt').write_text('artifact')`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if tc.initial != "" {
				if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(tc.initial), 0600); err != nil {
					t.Fatal(err)
				}
			}
			durable, err := runCommandExploration(t, workspace, [][]any{modificationHandoff(), modificationTool("command", "exec", map[string]any{"command": "python3", "args": []string{"-c", tc.script}}), {observationMessage(`{"status":"completed","answer":"done"}`)}})
			var saved struct {
				Exploration struct {
					ModificationConfirmed bool
					ModificationEvidence  []struct {
						Path  string
						Batch int
					}
				}
			}
			if decodeErr := json.Unmarshal(durable.ExecutionProgress, &saved); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if saved.Exploration.ModificationConfirmed != tc.changed || (tc.changed && err != nil) || (!tc.changed && err == nil) {
				t.Fatalf("changed=%v err=%v state=%s", tc.changed, err, durable.ExecutionProgress)
			}
			for _, evidence := range saved.Exploration.ModificationEvidence {
				if evidence.Path != "main.go" || evidence.Batch != 2 {
					t.Fatalf("unexpected attribution: %s", durable.ExecutionProgress)
				}
			}
		})
	}
}

func TestReactParallelCommandChangesHaveBatchEvidence(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	batch := append(modificationTool("target", "exec", map[string]any{"command": "python3", "args": []string{"-c", `from pathlib import Path; Path('main.go').write_text('new\n')`}}), modificationTool("other", "exec", map[string]any{"command": "python3", "args": []string{"-c", `from pathlib import Path; Path('other.go').write_text('unrelated\n')`}})...)
	durable, err := runCommandExploration(t, workspace, [][]any{modificationHandoff(), batch, {observationMessage(`{"status":"completed","answer":"done"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Exploration struct {
			Used                 int
			ModificationEvidence []struct {
				Path  string
				Batch int
			}
			Accounting []struct{ Calls map[string]string }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 || len(saved.Exploration.ModificationEvidence) != 1 || saved.Exploration.ModificationEvidence[0].Batch != 2 || saved.Exploration.ModificationEvidence[0].Path != "main.go" || len(saved.Exploration.Accounting[0].Calls) != 2 {
		t.Fatalf("missing batch boundary: %s", durable.ExecutionProgress)
	}
}

func TestReactTargetDiffVerifiesConfirmedModification(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "main.go"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, output)
		}
	}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationHandoff(),
		modificationTool("edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/old/new/", "main.go"}}),
		modificationTool("verify", "exec", map[string]any{"command": "git", "args": []string{"diff", "--", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"verified diff"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct{ Exploration struct{ Used int } }
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 0 {
		t.Fatalf("target result verification mischarged: %s", durable.ExecutionProgress)
	}
}

func TestReactProcessCommandModificationUsesStartBaseline(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationHandoff(),
		modificationTool("start", "exec_start", map[string]any{"command": "python3", "args": []string{"-c", `from pathlib import Path; Path('main.go').write_text('new\n')`}}),
		modificationTool("poll", "exec_poll", map[string]any{"session_id": "$session", "wait_ms": 1000}),
		modificationTool("stop", "exec_stop", map[string]any{"session_id": "$session"}),
		{observationMessage(`{"status":"completed","answer":"changed"}`)},
	})
	if err != nil {
		t.Fatalf("process modification must retain start baseline: %v; %s", err, durable.ExecutionProgress)
	}
}

func TestReactOversizedTargetSnapshotRemainsUnknown(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(strings.Repeat("x", 4*1024*1024+1)), 0600); err != nil {
		t.Fatal(err)
	}
	durable, _ := runCommandExploration(t, workspace, [][]any{modificationHandoff(), modificationTool("edit", "exec", map[string]any{"command": "python3", "args": []string{"-c", `from pathlib import Path; Path('main.go').write_text('new')`}}), {observationMessage(`{"status":"completed","answer":"claimed changed"}`)}})
	var saved struct {
		Exploration struct {
			ModificationConfirmed bool
			Accounting            []struct{ Outcome string }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.ModificationConfirmed || saved.Exploration.Accounting[0].Outcome != "unknown" {
		t.Fatalf("snapshot limit must not invent evidence: %s", durable.ExecutionProgress)
	}
}
