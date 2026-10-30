package provider

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

type JsonOutput[T any] struct{}

func (*JsonOutput[T]) Parse(text string, model T) error {
	return json.Unmarshal([]byte(text), model)
}

// LineProtocolOutput 将一个完整的 Line Protocol 文档解码到业务 struct。
// T 使用目标指针类型，例如：
//
//	var result Response
//	parser := &LineProtocolOutput[*Response]{}
//	err := parser.Parse(document, &result)
//
// 块边界支持空格和下划线两种形式，名称不区分大小写，也可以混用：
//
//	BEGIN RESPONSE
//	TEXT="hello"
//	END RESPONSE
//
//	begin_response
//	text="hello"
//	end_response
//
// 字段和块边界可以用换行或空格、Tab 分隔；字符串内的空白保持原样。
// 每个字段必须写成 FIELD=JSON_LITERAL。值只能是 JSON 字符串、数字、
// 布尔值或 null；不允许数组或对象字面量：
//
//	BEGIN RESPONSE
//	TEXT="hello\nworld"
//	COUNT=2
//	RATIO=0.5
//	OK=true
//	OPTIONAL=null
//	END RESPONSE
//
// 字段默认匹配 json tag，没有 json tag 时匹配 Go 字段名，匹配不区分
// 大小写。lineprotocol tag 可以覆盖协议名称。例如：
//
//	Text string `json:"text"`                         // TEXT="hello"
//	Done bool `json:"done" lineprotocol:"COMPLETED"` // COMPLETED=true
//	Note string `json:"note" lineprotocol:"META.NOTE"` // META.NOTE="hello"
//
// 点分名称是完整字段名，不会自动展开为嵌套对象。json:"-" 或
// lineprotocol:"-" 的字段以及未导出字段不能出现在协议中。
//
// 重复标量写入切片，按出现顺序保留；只有一次出现也会生成单元素切片：
//
//	// Tags []string `json:"tags"`
//	BEGIN RESPONSE
//	TAGS="first"
//	TAGS="second"
//	END RESPONSE
//
// 列表通过重复字段表达，嵌套结构通过子块表达；VALUE=[1,2]、
// VALUE={"a":1} 和 VALUE={'a':1} 均不合法。引号内的字符串内容可以
// 包含方括号或花括号，例如 TEXT="[1,2]" 是合法字符串。
//
// 子块写入嵌套 struct 或 struct 指针；重复子块写入 struct 切片或
// struct 指针切片。子块名默认匹配字段的 json tag、Go 字段名（无 json
// tag 时）或目标 struct 类型名；显式 lineprotocol tag 会覆盖这些名称：
//
//	type Step struct {
//	    ID string `json:"id"`
//	}
//	type Plan struct {
//	    Revision int `json:"revision"`
//	    Steps []Step `json:"steps" lineprotocol:"STEP"`
//	}
//
//	BEGIN PLAN
//	REVISION=1
//	BEGIN STEP
//	ID="read"
//	END STEP
//	BEGIN_STEP
//	ID="write"
//	END_STEP
//	END PLAN
//
// 子块可以继续嵌套，使用相同的映射规则。空块也合法，例如
// "BEGIN NO_TOOL\nEND NO_TOOL" 可以解码到 struct{}。
//
// 文档必须只有一个完整根块；根块名称不与 Go 类型名绑定。支持 LF、
// CRLF 和 CR 换行，末尾允许一个换行。空行、注释、Markdown 围栏、
// 块外文本、行首缩进、JSON 值两侧空白、未知字段或子块、边界不匹配、
// 重复的非切片字段或子块，以及同一字段同时使用标量和子块均会报错。
// 缺失字段不做必填校验，业务约束由调用方检查。
type LineProtocolOutput[T any] struct{}

// Parse decodes one complete block into a business struct. Protocol names are
// case-insensitive and match JSON tags (or Go field names). A lineprotocol tag
// overrides that name. Child blocks may also match their destination type name.
// Repeated scalar lines and child blocks populate slice fields.
func (*LineProtocolOutput[T]) Parse(text string, model T) error {
	destination := reflect.ValueOf(model)
	if !destination.IsValid() || destination.Kind() != reflect.Pointer || destination.IsNil() {
		return fmt.Errorf("line protocol destination must be a non-nil pointer to a struct")
	}
	block, err := parseLineProtocol(text)
	if err != nil {
		return err
	}
	data, err := blockJSON(block, destination.Type().Elem())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, model)
}

func indirectType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func blockJSON(block *ParsedBlock, t reflect.Type) ([]byte, error) {
	return blockJSONAt(block, t, "", nil)
}

type protocolFieldLocation struct {
	path, block string
	root        bool
}

func protocolPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

func blockJSONAt(block *ParsedBlock, t reflect.Type, path string, ancestors map[string]protocolFieldLocation) ([]byte, error) {
	t = indirectType(t)
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("block %s requires a struct destination", block.Type)
	}
	type fieldDeclaration struct {
		key string
		typ reflect.Type
	}
	fields := make(map[string]fieldDeclaration)
	children := make(map[string]fieldDeclaration)
	add := func(declarations map[string]fieldDeclaration, name string, declaration fieldDeclaration) error {
		name = strings.ToUpper(name)
		if previous, exists := declarations[name]; exists && previous.key != declaration.key {
			return fmt.Errorf("block %s has ambiguous destination for %s", block.Type, name)
		}
		declarations[name] = declaration
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		if !field.IsExported() || key == "-" {
			continue
		}
		if key == "" {
			key = field.Name
		}
		name := field.Tag.Get("lineprotocol")
		if name == "-" {
			continue
		}
		if name == "" {
			name = key
		}
		declaration := fieldDeclaration{key: key, typ: field.Type}
		if err := add(fields, name, declaration); err != nil {
			return nil, err
		}
		childType := indirectType(field.Type)
		if childType.Kind() == reflect.Slice {
			childType = indirectType(childType.Elem())
		}
		if childType.Kind() == reflect.Struct {
			if err := add(children, name, declaration); err != nil {
				return nil, err
			}
			if field.Tag.Get("lineprotocol") == "" && childType.Name() != "" {
				if err := add(children, childType.Name(), declaration); err != nil {
					return nil, err
				}
			}
		}
	}
	object := make(map[string]json.RawMessage)
	for name, values := range block.Fields {
		declaration, ok := fields[name]
		if !ok {
			message := fmt.Sprintf("block %s contains undeclared field %s (path %s)", block.Type, name, protocolPath(path, strings.ToLower(name)))
			// 只根据真实祖先的 schema 提供移动建议，不猜测未知字段的含义。
			if target, exists := ancestors[name]; exists {
				location := "ancestor"
				if target.root {
					location = "outer"
				}
				message += fmt.Sprintf(". Move %s to the %s %s block at %s, outside the nested %s block", name, location, target.block, target.path, block.Type)
			}
			return nil, fmt.Errorf("%s", message)
		}
		if indirectType(declaration.typ).Kind() == reflect.Slice {
			data, err := json.Marshal(values)
			if err != nil {
				return nil, err
			}
			object[declaration.key] = data
		} else {
			if len(values) != 1 {
				return nil, fmt.Errorf("block %s contains repeated non-array field %s", block.Type, name)
			}
			object[declaration.key] = values[0]
		}
	}
	childValues := make(map[string][]json.RawMessage)
	// 每一层复制祖先字段表，避免兄弟块互相污染纠错目标。
	locations := make(map[string]protocolFieldLocation, len(ancestors)+len(fields))
	for name, location := range ancestors {
		locations[name] = location
	}
	for name, declaration := range fields {
		locations[name] = protocolFieldLocation{path: protocolPath(path, declaration.key), block: block.Type, root: path == ""}
	}
	for _, child := range block.Children {
		declaration, ok := children[child.Type]
		if !ok {
			return nil, fmt.Errorf("block %s contains undeclared child block %s (path %s)", block.Type, child.Type, protocolPath(path, strings.ToLower(child.Type)))
		}
		if _, exists := object[declaration.key]; exists && len(childValues[declaration.key]) == 0 {
			return nil, fmt.Errorf("block %s uses %s as both a field and child block", block.Type, declaration.key)
		}
		childType := indirectType(declaration.typ)
		isSlice := childType.Kind() == reflect.Slice
		if isSlice {
			childType = childType.Elem()
		}
		childPath := protocolPath(path, declaration.key)
		if isSlice {
			childPath = fmt.Sprintf("%s[%d]", childPath, len(childValues[declaration.key]))
		}
		data, err := blockJSONAt(child, childType, childPath, locations)
		if err != nil {
			return nil, err
		}
		childValues[declaration.key] = append(childValues[declaration.key], data)
		values := childValues[declaration.key]
		if isSlice {
			object[declaration.key], err = json.Marshal(values)
			if err != nil {
				return nil, err
			}
		} else {
			if len(values) > 1 {
				return nil, fmt.Errorf("block %s contains repeated non-array child %s", block.Type, child.Type)
			}
			object[declaration.key] = data
		}
	}
	return json.Marshal(object)
}

// ParsedBlock preserves repeated scalar values and the order of child blocks.
type ParsedBlock struct {
	Type     string                       `json:"type"`
	Fields   map[string][]json.RawMessage `json:"fields"`
	Children []*ParsedBlock               `json:"children"`
}

var (
	lineBoundary = regexp.MustCompile(`(?i)^(BEGIN|END)[ _]([A-Z][A-Z0-9_]*)$`)
	lineScalar   = regexp.MustCompile(`(?i)^([A-Z][A-Z0-9_]*(?:\.[A-Z][A-Z0-9_]*)*)=(.+)$`)
)

func parseLineProtocol(document string) (*ParsedBlock, error) {
	document = normalizeProtocolSpacing(document)
	document = strings.ReplaceAll(strings.ReplaceAll(document, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(strings.TrimSuffix(document, "\n"), "\n")
	if document == "" || document == "\n" {
		return nil, fmt.Errorf("protocol response is empty")
	}
	var root *ParsedBlock
	var stack []*ParsedBlock
	for i, line := range lines {
		fail := func(message string) (*ParsedBlock, error) {
			return nil, fmt.Errorf("line protocol line %d: %s", i+1, message)
		}
		if line == "" || strings.HasPrefix(line, "#") {
			return fail("blank lines and comments are not permitted")
		}
		if boundary := lineBoundary.FindStringSubmatch(line); boundary != nil {
			kind, name := strings.ToUpper(boundary[1]), strings.ToUpper(boundary[2])
			if kind == "BEGIN" {
				block := &ParsedBlock{Type: name, Fields: make(map[string][]json.RawMessage), Children: make([]*ParsedBlock, 0)}
				if len(stack) > 0 {
					parent := stack[len(stack)-1]
					parent.Children = append(parent.Children, block)
				} else if root != nil {
					return fail("multiple top-level blocks")
				} else {
					root = block
				}
				stack = append(stack, block)
			} else {
				if len(stack) == 0 || stack[len(stack)-1].Type != name {
					return fail("mismatched block boundary")
				}
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if scalar := lineScalar.FindStringSubmatch(line); scalar != nil && len(stack) > 0 {
			path, literal := strings.ToUpper(scalar[1]), scalar[2]
			if literal != strings.TrimSpace(literal) {
				return fail("whitespace around JSON literal for " + path)
			}
			if !json.Valid([]byte(literal)) {
				return fail("invalid JSON literal for " + path)
			}
			if literal[0] == '[' || literal[0] == '{' {
				return fail("array and object literals are not permitted for " + path)
			}
			block := stack[len(stack)-1]
			block.Fields[path] = append(block.Fields[path], json.RawMessage(literal))
			continue
		}
		return fail("invalid line or text outside a block")
	}
	if root == nil || len(stack) != 0 {
		return nil, fmt.Errorf("incomplete protocol block")
	}
	return root, nil
}

// lineProtocolResponseDocument 只在 LLM 响应入口去掉块外说明，协议解析器仍严格校验。
// 保留所有边界之间的内容，避免静默选取多根块中的一个或掩盖损坏的子块。
func lineProtocolResponseDocument(text string) string {
	text = normalizeProtocolSpacing(text)
	normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(normalized, "\n")
	first, last := -1, -1
	for i, line := range lines {
		if lineBoundary.MatchString(line) {
			if first == -1 {
				first = i
			}
			last = i
		}
	}
	if first == -1 {
		return text
	}
	for i, line := range lines {
		// 块外字段不是说明文字；交给严格解析器报错，不能丢弃潜在业务数据。
		if (i < first || i > last) && lineScalar.MatchString(line) {
			return text
		}
	}
	return strings.Join(lines[first:last+1], "\n")
}
