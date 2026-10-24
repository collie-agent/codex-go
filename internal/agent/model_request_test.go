package agent

import (
	"encoding/json"
	"io"
)

// decodeModelRequest lets text assertions cover both string input and message input.
// Role assertions use the original request in prompt_test.go.
func decodeModelRequest(body io.Reader, target any) error {
	var request map[string]json.RawMessage
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		return err
	}
	var items []struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(request["input"], &items) == nil && items != nil {
		var text string
		for _, item := range items {
			text += item.Content + "\n"
		}
		request["input"], _ = json.Marshal(text)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}
