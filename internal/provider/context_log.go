package provider

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go/v3/responses"
	"gopkg.in/yaml.v3"
)

// formatLLMContext 使用 SDK 的 JSON 序列化，并从日志中省略工具定义。
// YAML 只用于日志展示，多行字符串展开为文本块，不改变发送给 API 的内容。
func formatLLMContext(params responses.ResponseNewParams) ([]byte, error) {
	// params 是值拷贝，省略日志中的工具定义不会影响实际 API 请求。
	params.Tools = nil
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(encoded, &document); err != nil {
		return nil, err
	}
	var expand func(*yaml.Node)
	expand = func(node *yaml.Node) {
		node.Style = 0
		if node.Kind == yaml.ScalarNode && node.Tag == "!!str" && strings.Contains(node.Value, "\n") {
			node.Style = yaml.LiteralStyle
		}
		for _, child := range node.Content {
			expand(child)
		}
	}
	expand(&document)
	return yaml.Marshal(&document)
}
