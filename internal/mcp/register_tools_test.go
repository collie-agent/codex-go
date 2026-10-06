package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRegisterToolsExposesAllFileTools(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := RegisterTools(server, Config{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil)
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), serverTransport) }()
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{"list_dir", "read_file", "glob", "grep", "apply_patch"} {
		if !got[name] {
			t.Errorf("registered tools missing %q: %#v", name, got)
		}
	}
}

func TestRegisterToolsOnlyExposesAndExecutesAllowedTools(t *testing.T) {
	workspace := t.TempDir()
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := RegisterTools(server, Config{
		Workspace:  workspace,
		AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}},
	}); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil)
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), serverTransport) }()
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != string(config.ReadFileMCPToolName) {
		t.Fatalf("exposed tools=%v, want only read_file", tools.Tools)
	}
	_, err = session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: string(config.ListDirMCPToolName), Arguments: map[string]any{"path": "."},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unallowed tool call error=%v, want unknown tool", err)
	}
}
