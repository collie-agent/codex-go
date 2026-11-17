package context

import "github.com/collie-agent/codex-go/internal/config"

// 公共层只声明协议规则；具体字段与 few-shot 示例由各业务上下文提供。
const jsonFormatPrompt = `Output format: JSON.
Return exactly one valid JSON value matching the requested output schema. Do not include Markdown fences, comments, or explanatory text. Preserve field name casing exactly. Use JSON arrays for lists, including [] for empty lists.`

// 格式约束只作用于最终文本，不限制原生工具调用。
const lineProtocolFormatPrompt = `Final text output format: Line Protocol.

If supplied function tools are needed to gather evidence or advance the task, call them through the native tool-call interface. Do not write tool calls as Line Protocol text and do not emit a final result while required tool results are still missing. The following contract applies only when returning a final text result without tool calls.

Mandatory response contract:
- Your entire response must contain only one Line Protocol document. Never combine prose with a protocol block.
- Start immediately with the root BEGIN line and stop immediately after its matching END line. Do not add a preamble, summary, explanation, heading, Markdown fence, or postscript, even when the task asks for an explanation.
- Put all requested answers, explanations, and evidence inside the declared protocol fields as JSON-escaped strings. Never write them before or after the root block. Do not add fields absent from the task's schema.
- For example, when the task requests RESULT with STATUS and ANSWER, start with BEGIN RESULT, write STATUS and ANSWER inside the block, and finish with END RESULT. A sentence such as "Based on the code examined, I have enough information to answer." before BEGIN RESULT is forbidden.

Document and names:
- Return exactly one complete root block. Use uppercase BEGIN NAME and END NAME on separate lines, and close nested blocks in reverse opening order with matching names.
- Names are case-insensitive. Block names match [A-Z][A-Z0-9_]*. Field names use the same pattern, optionally with dot-separated segments of that pattern. BEGIN_NAME and END_NAME are also accepted, including mixed space/underscore boundaries; prefer the space form.
- Use the root name requested by the task; the parser does not require it to equal the destination Go type name.
- For fields, use an explicit lineprotocol tag when present, otherwise the json tag name before any comma options, otherwise the Go field name. For child blocks, the same mapping applies; without an explicit lineprotocol tag, the destination struct type name is also accepted. An explicit lineprotocol tag replaces these aliases.
- A dotted name such as META.NOTE is one literal field name, not a path that creates nested objects. Emit it only when declared by the destination schema.
- Never output undeclared fields or blocks, unexported fields, or fields tagged json:"-" or lineprotocol:"-". Use only the current task's declared schema.

Values and structure:
- Write each scalar on one physical line as FIELD=JSON_LITERAL with no spaces around = or the literal. Values must be valid JSON strings, numbers, true, false, or null and match the destination type. Do not quote numbers or booleans unless the destination is a string. NaN, Infinity, single-quoted strings, and unquoted text are invalid.
- Inside strings, escape quotes as \", backslashes as \\, newlines as \n, carriage returns as \r, and tabs as \t. Brackets and braces inside a quoted string are ordinary text: TEXT="[1,2]" is valid.
- Never use array or object literals, including [] and {}. Represent scalar lists by repeated field lines, and struct objects by child blocks. Structs and struct pointers can have recursively nested child blocks; lists of structs or struct pointers use repeated child blocks.
- A single occurrence still produces a one-element list when the destination is a slice. List order follows occurrence order. Never repeat a non-list field or child block, even through different aliases.
- Never represent the same destination field using both scalar lines and child blocks, even through different aliases.
- Empty blocks are valid and represent zero-valued structs (or allocated struct pointers). For an optional struct pointer, FIELD=null represents a nil pointer; do not also emit its child block.
- There is no explicit empty-list literal. Omit a list with no elements; on a fresh destination it remains at its zero value, usually a nil slice, not a guaranteed non-nil empty slice. LIST=null is one null list element, not a null or empty list, and is appropriate only when the element type and task allow it.
- Omitted fields remain at their zero values on a fresh destination. The parser does not enforce required fields; still provide every field required by the task. Use null only when the task and destination type permit it; it is not a universal substitute for an empty or missing value.

Whitespace:
- Do not include blank lines, comments, indentation, trailing spaces, Markdown fences, explanations, or text outside the root block. Strings may contain whitespace inside their quotes.
- Prefer LF line endings. CRLF and CR are also accepted. End immediately after the root END line, or with exactly one final newline.

Invalid forms (do not output these): TAGS=[]; DETAIL={}; MESSAGE=hello; COUNT=NaN; META.NOTE="x" when no such literal field is declared; BEGIN RESULT closed by END PLAN; two root blocks; or a field emitted both as FIELD=null and as a FIELD child block.

Now handle the original task. If tools are needed, use the supplied native function tools. Otherwise, return the final result using its declared schema and Line Protocol format only. Before sending final text, check that the first line is the root BEGIN line, the last line is its matching END line, and every answer or explanation is inside a declared field. Output only that document for a final text result.`

// CommonFormatFewShot 公开公共格式契约，调用方随后追加自身字段及业务示例。
func CommonFormatFewShot(schema config.LLMOutputSchema) string {
	switch schema {
	case config.JsonLLMOutputSchema:
		return jsonFormatPrompt
	case config.LineProtocolLLMOutputSchema:
		return lineProtocolFormatPrompt
	default:
		return ""
	}
}
