package agent_test

import (
	"encoding/json"
	"net/http"
)

func criteriaResponse(w http.ResponseWriter, text string, calls ...any) {
	w.Header().Set("Content-Type", "application/json")
	output := append([]any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}, calls...)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "criteria", "status": "completed", "output": output})
}
