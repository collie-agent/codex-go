package mcp

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

func toolInputSchema[T any]() *jsonschema.Schema {
	// *int 仅用于区分省略参数和显式传入 0；MCP 输入允许省略，但不接受 null。
	// 用值类型的副本推导 schema，保留字段标签及 omitempty，不改变实际参数类型。
	argsType := reflect.TypeFor[T]()
	fields := make([]reflect.StructField, argsType.NumField())
	for i := range fields {
		fields[i] = argsType.Field(i)
		if fields[i].Type == reflect.TypeFor[*int]() {
			fields[i].Type = reflect.TypeFor[int]()
		}
	}
	schema, err := jsonschema.ForType(reflect.StructOf(fields), nil)
	if err != nil {
		panic(fmt.Sprintf("generate MCP input schema: %v", err))
	}
	// Optional string slices mean an omitted array, not a nullable argument. Keep the
	// SDK's element schema while removing the null alternative inferred for nil.
	for _, field := range fields {
		if field.Type == reflect.TypeFor[[]string]() {
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			property := schema.Properties[name]
			property.Type, property.Types = "array", nil
		}
	}
	return schema
}
