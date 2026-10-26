package mcp

import (
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func skillToolSession(t *testing.T) *sdk.ClientSession {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := RegisterTools(server, Config{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(t.Context(), serverTransport) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestLoadSkillToolLoadsByDeclaredName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".agents", "skills", "testing")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	const skill = "---\nname: tdd\ndescription: Write tests first.\n---\n# Instructions\nWrite a failing test before implementation.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0600); err != nil {
		t.Fatal(err)
	}
	session := skillToolSession(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "load_skill" {
			found = true
		}
	}
	if !found {
		t.Fatal("load_skill is not registered")
	}
	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "load_skill", Arguments: map[string]any{"skill_name": "tdd"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("load_skill result: %+v", result)
	}
	text, ok := result.Content[0].(*sdk.TextContent)
	if !ok || text.Text != skill {
		t.Fatalf("skill content = %#v, want %q", result.Content, skill)
	}
}

func TestLoadSkillToolRejectsUnknownOrInvalidNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".agents", "skills", "invalid")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: No name.\n---\nMust not load this.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	session := skillToolSession(t)
	for _, args := range []map[string]any{
		{"skill_name": "unknown"},
		{"skill_name": ""},
		{"skill_name": " "},
		{"skill_name": "../invalid"},
		{"skill_name": "/tmp/SKILL.md"},
		{"skill_name": 42},
		{},
	} {
		result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "load_skill", Arguments: args})
		if err == nil && (result == nil || !result.IsError) {
			t.Errorf("invalid arguments %v accepted: %+v", args, result)
		}
	}
}
