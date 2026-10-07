package provider

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"
)

func (*JsonOutput[T]) schema() (map[string]any, error) {
	// T 是解析目标的指针类型；根节点必须是 object，不能因目标指针而允许 null。
	schema, err := jsonschema.ForType(indirectType(reflect.TypeFor[T]()), nil)
	if err != nil {
		return nil, err
	}
	if schema.Type != "object" {
		return nil, fmt.Errorf("JSON output requires an object root, got %v", reflect.TypeFor[T]())
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode JSON output schema: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode JSON output schema: %w", err)
	}
	if err := strictJSONSchema(result, "$"); err != nil {
		return nil, err
	}
	return result, nil
}

func strictJSONSchema(value any, path string) error {
	// 推导器可能用布尔 schema true 表示 any；strict 输出不接受这种无约束节点。
	schema, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("JSON output schema %s: unconstrained types such as any are unsupported", path)
	}
	var types []string
	switch typ := schema["type"].(type) {
	case string:
		types = []string{typ}
	case []any:
		for _, typ := range typ {
			types = append(types, typ.(string))
		}
	default:
		return fmt.Errorf("JSON output schema %s: unconstrained types such as any are unsupported", path)
	}
	for _, typ := range types {
		switch typ {
		case "object":
			// strict 模式禁止动态键；不能把 map 静默改成只允许空对象。
			if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
				return fmt.Errorf("JSON output schema %s: dynamic objects such as maps are unsupported", path)
			}
			properties, _ := schema["properties"].(map[string]any)
			if properties == nil {
				properties = map[string]any{}
				schema["properties"] = properties
			}
			// 所有属性（包括 omitempty/omitzero）必须必填；指针的 null 类型由推导器保留。
			required := make([]string, 0, len(properties))
			for name := range properties {
				required = append(required, name)
			}
			sort.Strings(required)
			schema["required"] = required
			for _, name := range required {
				if err := strictJSONSchema(properties[name], path+"."+name); err != nil {
					return err
				}
			}
		case "array":
			if err := strictJSONSchema(schema["items"], path+"[]"); err != nil {
				return err
			}
		}
	}
	return nil
}
