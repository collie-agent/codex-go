// Package testutil provides model API fixtures shared by application and command tests.
package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// RespondWithCriteria handles the initial criteria-tool round of a fake model.
// Task-specific handlers can then focus on the behavior they exercise. Dedicated
// agent tests cover the criteria protocol itself with explicit request assertions.
func RespondWithCriteria(t testing.TB, w http.ResponseWriter, r *http.Request, criteria ...string) bool {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		http.Error(w, err.Error(), 500)
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		t.Error(err)
		http.Error(w, err.Error(), 500)
		return true
	}
	for _, tool := range request.Tools {
		if tool.Name == "update_completion_criteria" {
			if len(criteria) == 0 {
				criteria = []string{"original task verified"}
			}
			args, _ := json.Marshal(map[string]any{"completion_criteria": criteria})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "criteria", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc_criteria", "call_id": "criteria", "name": "update_completion_criteria", "arguments": string(args)}}})
			return true
		}
	}
	return false
}
