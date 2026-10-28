package agent_test

import (
	"encoding/json"
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

func TestLoadSkillsRegistersMetadataFromHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for dir, content := range map[string]string{
		"testing": "---\nname: tdd\ndescription: |\n  Write tests first.\n  Then implement.\n---\nSecret skill body.\n",
		"design":  "---\nname: design\ndescription: 'Design: small interfaces.'\n---\n# Instructions\n",
	} {
		path := filepath.Join(home, ".agents", "skills", dir)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	skills, err := agent.LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	want := []agent.Skill{
		{Name: "design", Description: "Design: small interfaces."},
		{Name: "tdd", Description: "Write tests first.\nThen implement."},
	}
	if !reflect.DeepEqual(skills, want) {
		t.Fatalf("registered skills = %#v, want %#v", skills, want)
	}
}

func TestReactRunIncludesRegisteredSkillsInModelContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		var content string
		for _, message := range request.Input {
			content += message.Content
		}
		for _, want := range []string{"# Available skills", `"name":"tdd"`, `"description":"Write tests first."`} {
			if !strings.Contains(content, want) {
				t.Errorf("model context missing %q", want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_final","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\"}"}]}]}`))
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetSkills([]agent.Skill{{Name: "tdd", Description: "Write tests first."}})
	cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{
		BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema,
	}}
	result, _, err := process.Run(cfg, &state.DurableState{CompletionCriteria: []string{"original task verified"}, RunId: "skills-run", Goal: "answer"})
	if err != nil || result.Answer != "done" {
		t.Fatalf("ReAct result = %+v, err = %v", result, err)
	}
}

func TestLoadSkillsAllowsMissingDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	skills, err := agent.LoadSkills()
	if err != nil || len(skills) != 0 {
		t.Fatalf("missing directory: skills = %#v, err = %v", skills, err)
	}
}

func TestLoadSkillsRejectsInvalidMetadata(t *testing.T) {
	for _, content := range []string{
		"# No front matter",
		"---\nname: tdd\n",
		"---\nname: [invalid\n---\n",
		"---\nname: ' '\ndescription: Tests first.\n---\n",
		"---\nname: tdd\ndescription: ' '\n---\n",
	} {
		t.Run(content, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".agents", "skills", "invalid")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			skills, err := agent.LoadSkills()
			if err == nil || !strings.Contains(err.Error(), path) || len(skills) != 0 {
				t.Fatalf("invalid metadata: skills = %#v, err = %v", skills, err)
			}
		})
	}
}

func TestReactRunLoadsBuiltInSkillWithoutToolConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".agents", "skills", "testing")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	const instruction = "Write a failing test before implementation."
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: tdd\ndescription: Use for test-driven development.\n---\n"+instruction+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []struct {
				Type    string `json:"type"`
				Content string `json:"content"`
				Output  string `json:"output"`
				CallID  string `json:"call_id"`
			} `json:"input"`
			Tools []struct {
				Name       string `json:"name"`
				Parameters struct {
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls++
		var prompt string
		for _, msg := range req.Input {
			prompt += msg.Content
		}
		for _, rule := range []string{
			"When a user request clearly matches a skill, load that skill before proceeding.",
			"Use the skill description to select skills.",
			"Do not assume skill instructions before loading.",
		} {
			if !strings.Contains(prompt, rule) {
				t.Errorf("missing skill rule %q", rule)
			}
		}
		found := false
		for _, tool := range req.Tools {
			if tool.Name == "load_skill" && reflect.DeepEqual(tool.Parameters.Required, []string{"skill_name"}) {
				found = true
			}
		}
		if !found {
			t.Error("model did not receive load_skill with required skill_name")
		}
		var output any
		if calls == 1 {
			if strings.Contains(prompt, instruction) {
				t.Error("skill instructions exposed before loading")
			}
			output = map[string]any{"type": "function_call", "id": "fc_skill", "call_id": "call_skill", "name": "load_skill", "arguments": `{"skill_name":"tdd"}`}
		} else {
			loaded := false
			for _, msg := range req.Input {
				if msg.Type == "function_call_output" && msg.CallID == "call_skill" && strings.Contains(msg.Output, instruction) {
					loaded = true
				}
			}
			if !loaded {
				t.Error("loaded skill instructions missing from next ReAct context")
			}
			output = map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": `{"status":"completed","answer":"done"}`}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_skill", "status": "completed", "output": []any{output}})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	contents := "name: react\nlog_level: error\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n"
	if err := os.WriteFile(configPath, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	skills, err := agent.LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetSkills(skills)
	result, _, err := process.Run(cfg, &state.DurableState{CompletionCriteria: []string{"original task verified"}, RunId: "load-skill", Goal: "Implement this feature using test-driven development."})
	if err != nil || result.Answer != "done" || calls != 2 {
		t.Fatalf("result = %+v, err = %v, model requests = %d", result, err, calls)
	}
}
