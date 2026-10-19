package mcp

import (
	"fmt"
	"reflect"

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
	return schema
}
