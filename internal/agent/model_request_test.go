package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
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

// 直接检查发送给 LLM 的消息边界，再复用文本断言验证提示内容。
func decodeCorrectionRequest(t *testing.T, body io.Reader, target any, corrections int, marker string) error {
	t.Helper()
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	var request struct {
		Input []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}
	if len(request.Input) != 2+corrections {
		t.Errorf("messages = %d, want %d (system, state, corrections)", len(request.Input), 2+corrections)
	}
	for index, message := range request.Input {
		if index < 2 {
			if strings.Contains(message.Content, marker) {
				t.Errorf("correction mixed into system/state: %s", message.Content)
			}
			continue
		}
		if message.Role != "user" || !strings.Contains(message.Content, marker) {
			t.Errorf("expected independent user correction, got role=%q content=%q", message.Role, message.Content)
		}
	}
	return decodeModelRequest(bytes.NewReader(data), target)
}
