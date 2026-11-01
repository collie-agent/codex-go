package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"gopkg.in/yaml.v3"
)

func TestFormatLLMContextListsToolsAndPreservesInput(t *testing.T) {
	prompt := "# DurableState\n\n## Goal\n\n> 检查文件\n> C:\\notes.txt\n"
	output := responses.ResponseInputItemParamOfFunctionCallOutput("first line\nsecond line\n\n")
	output.OfFunctionCallOutput.CallID = openai.String("call_read")
	for name, input := range map[string]responses.ResponseNewParamsInputUnion{
		"text": {OfString: openai.String(prompt)},
		"tool messages": {OfInputItemList: responses.ResponseInputParam{
			responses.ResponseInputItemParamOfMessage(prompt, responses.EasyInputMessageRoleUser),
			responses.ResponseInputItemParamOfFunctionCall(`{"path":"notes.txt"}`, "call_read", "read_file"),
			output,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			params := responses.ResponseNewParams{
				Model: "test", Input: input,
				Tools: []responses.ToolUnionParam{{OfFunction: &responses.FunctionToolParam{
					Name: "read_file", Strict: openai.Bool(true),
					Parameters: map[string]any{
						"type": "object", "additionalProperties": false,
						"properties": map[string]any{"path": map[string]any{"type": "string"}},
						"required":   []string{"path"},
					},
				}}},
			}
			formatted, err := formatLLMContext(params)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(formatted), "|\n") || !strings.Contains(string(formatted), "# DurableState\n") || strings.Contains(string(formatted), `# DurableState\n`) {
				t.Fatalf("prompt was not expanded into a multiline block:\n%s", formatted)
			}
			// 将展示日志还原为 JSON，校验工具消息、尾部换行和字段类型均未丢失。
			var logged any
			if err := yaml.Unmarshal(formatted, &logged); err != nil {
				t.Fatal(err)
			}
			loggedJSON, err := json.Marshal(logged)
			if err != nil {
				t.Fatal(err)
			}
			requestJSON, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(loggedJSON, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(requestJSON, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got["tools"], []any{"read_file"}) {
				t.Fatalf("unexpected tool list in context log: %s", loggedJSON)
			}
			if _, exists := want["tools"]; !exists {
				t.Fatal("formatting removed tools from the API request")
			}
			delete(want, "tools")
			delete(got, "tools")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("log differs from API request:\nlog: %s\nrequest: %s", loggedJSON, requestJSON)
			}
		})
	}
}

func TestContextLogExpandsHistoryWithTrailingSpaces(t *testing.T) {
	prompt := "# Conversation history\n\n  > - Result:\n  > \n  >   > first line\n  >   > second line\n"
	params := responses.ResponseNewParams{Model: "test", Input: responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)}}
	before, _ := json.Marshal(params)
	formatted, err := formatLLMContext(params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(formatted), `history\n`) || !strings.Contains(string(formatted), "# Conversation history\n") || !strings.Contains(string(formatted), "> first line\n") {
		t.Fatalf("history printed as escaped string instead of lines:\n%s", formatted)
	}
	after, _ := json.Marshal(params)
	if string(before) != string(after) {
		t.Fatal("logging changed API input")
	}
}
