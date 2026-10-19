package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
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
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, property := range schema.Properties {
			if property.Type == "null" || len(property.Types) != 0 {
				t.Errorf("tool %s property %s must have a non-null scalar type: %s", tool.Name, name, encoded)
			}
		}
		// 直接校验 MCP 定义，确保省略参数仍可用，但显式 null 会被拒绝。
		if tool.Name == "list_dir" {
			if err := resolved.Validate(map[string]any{}); err != nil {
				t.Errorf("list_dir defaults rejected: %v", err)
			}
			for _, name := range []string{"depth", "limit"} {
				if err := resolved.Validate(map[string]any{name: nil}); err == nil {
					t.Errorf("list_dir accepts null %s", name)
				}
			}
		}
	}
	for _, name := range []string{"list_dir", "read_file", "glob", "grep", "apply_patch"} {
		if !got[name] {
			t.Errorf("registered tools missing %q: %#v", name, got)
		}
	}
}
