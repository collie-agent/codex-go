package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCompletionCriteriaTool(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := filemcp.RegisterTools(server, filemcp.Config{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(t.Context(), st) }()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "update_completion_criteria" {
			found = true
		}
	}
	if !found {
		t.Fatal("update_completion_criteria is not registered")
	}
	for _, tc := range []struct {
		name     string
		args     map[string]any
		rejected bool
	}{
		{"valid", map[string]any{"completion_criteria": []string{"Report verified source content"}}, false},
		{"missing", map[string]any{}, true},
		{"empty", map[string]any{"completion_criteria": []string{}}, true},
		{"blank", map[string]any{"completion_criteria": []string{"valid", " "}}, true},
		{"wrong type", map[string]any{"completion_criteria": "done"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "update_completion_criteria", Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError != tc.rejected {
				t.Fatalf("result=%+v", result)
			}
			if !tc.rejected {
				encoded, _ := json.Marshal(result.StructuredContent)
				var got struct {
					Criteria []string `json:"completion_criteria"`
				}
				if err := json.Unmarshal(encoded, &got); err != nil || len(got.Criteria) != 1 || got.Criteria[0] != "Report verified source content" {
					t.Fatalf("result=%s err=%v", encoded, err)
				}
			}
		})
	}
}
