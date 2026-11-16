package mcp

import (
	"encoding/json"
	"testing"
)

func TestExecToolRetainsRuntimePurposeEvidence(t *testing.T) {
	session := execToolSession(t, Config{Workspace: t.TempDir()})
	result := fileToolCall(t, session, "exec", map[string]any{"command": "python3", "args": []string{"-c", "print('verification')"}})
	raw, _ := json.Marshal(result.StructuredContent)
	var out struct {
		Evidence struct {
			Purpose string `json:"purpose"`
		} `json:"command_evidence"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Evidence.Purpose != "investigation" {
		t.Fatalf("arbitrary interpreter cannot claim verification: %s", raw)
	}
}

func TestExecSessionRetainsCommandPurposeThroughStop(t *testing.T) {
	session := execToolSession(t, Config{Workspace: t.TempDir()})
	started := fileToolCall(t, session, "exec_start", map[string]any{"command": "python3", "args": []string{"-c", "print('test')"}})
	raw, _ := json.Marshal(started.StructuredContent)
	var start struct {
		SessionID string `json:"session_id"`
		Purpose   string `json:"purpose"`
	}
	if err := json.Unmarshal(raw, &start); err != nil {
		t.Fatal(err)
	}
	if start.SessionID == "" || start.Purpose != "investigation" {
		t.Fatalf("missing start purpose: %s", raw)
	}
	stopped := fileToolCall(t, session, "exec_stop", map[string]any{"session_id": start.SessionID})
	raw, _ = json.Marshal(stopped.StructuredContent)
	var stop struct {
		Purpose string `json:"purpose"`
	}
	if err := json.Unmarshal(raw, &stop); err != nil {
		t.Fatal(err)
	}
	if stop.Purpose != "investigation" {
		t.Fatalf("lost session purpose: %s", raw)
	}
}
