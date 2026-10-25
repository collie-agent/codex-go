package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

func validateToolArguments(inputSchema, arguments any) error {
	encoded, err := json.Marshal(inputSchema)
	if err != nil {
		return fmt.Errorf("encode tool schema: %w", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return fmt.Errorf("decode tool schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolve tool schema: %w", err)
	}
	encoded, err = json.Marshal(arguments)
	if err != nil {
		return fmt.Errorf("encode tool arguments: %w", err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		// 字符串误用于数字或布尔字段时给出直接的修正示例，不自动改写参数。
		var hints []string
		if object, ok := instance.(map[string]any); ok {
			for name, property := range schema.Properties {
				if property == nil || property.Type == "string" || slices.Contains(property.Types, "string") {
					continue
				}
				if _, ok := object[name].(string); !ok {
					continue
				}
				if property.Type == "integer" || slices.Contains(property.Types, "integer") {
					hints = append(hints, name+" must be a JSON integer, not a quoted string (example: 1)")
				} else if property.Type == "number" || slices.Contains(property.Types, "number") {
					hints = append(hints, name+" must be a JSON number, not a quoted string (example: 1.5)")
				} else if property.Type == "boolean" || slices.Contains(property.Types, "boolean") {
					hints = append(hints, name+" must be a JSON boolean, not a quoted string (example: false)")
				}
			}
		}
		if len(hints) > 0 {
			sort.Strings(hints)
			return fmt.Errorf("invalid arguments: %v. %s. Correct these fields and resubmit the tool call; the tool was not executed", err, strings.Join(hints, "; "))
		}
		return fmt.Errorf("invalid arguments: %v. Correct the arguments to match the tool schema and resubmit; the tool was not executed", err)
	}
	return nil
}
