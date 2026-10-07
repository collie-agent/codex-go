package provider

import (
	"reflect"
	"strings"
	"testing"
)

func TestJSONOutputSchemaNestedOptionalFields(t *testing.T) {
	type child struct {
		Note *string `json:"note,omitempty"`
	}
	type result struct {
		Children []child  `json:"children,omitempty"`
		Empty    struct{} `json:"empty"`
		Ignored  string   `json:"-"`
	}
	schema, err := (&JsonOutput[*result]{}).schema()
	if err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" || !reflect.DeepEqual(schema["required"], []string{"children", "empty"}) {
		t.Fatalf("unexpected root schema: %v", schema)
	}
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["Ignored"]; ok {
		t.Fatal("json ignored field appears in schema")
	}
	items := properties["children"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false || !reflect.DeepEqual(items["required"], []string{"note"}) {
		t.Fatalf("unexpected nested schema: %v", items)
	}
	note := items["properties"].(map[string]any)["note"].(map[string]any)
	if !reflect.DeepEqual(note["type"], []any{"null", "string"}) {
		t.Fatalf("pointer field must allow null: %v", note)
	}
	empty := properties["empty"].(map[string]any)
	if empty["additionalProperties"] != false || len(empty["properties"].(map[string]any)) != 0 || !reflect.DeepEqual(empty["required"], []string{}) {
		t.Fatalf("unexpected empty object schema: %v", empty)
	}
}

func TestJSONOutputSchemaRejectsUnsupportedTypes(t *testing.T) {
	type recursive struct {
		Next *recursive `json:"next"`
	}
	for _, tc := range []struct {
		name     string
		generate func() (map[string]any, error)
		message  string
	}{
		{"scalar", (&JsonOutput[*string]{}).schema, "object root"},
		{"map", (&JsonOutput[*struct {
			Data map[string]string `json:"data"`
		}]{}).schema, "$.data: dynamic objects"},
		{"any", (&JsonOutput[*struct {
			Data any `json:"data"`
		}]{}).schema, "$.data: unconstrained types"},
		{"recursive", (&JsonOutput[*recursive]{}).schema, "cycle detected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.generate()
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("got error %v, want %q", err, tc.message)
			}
		})
	}
}
