package provider

const jsonFewShotPrompt = `Output format: JSON.
Return exactly one valid JSON value matching the requested output schema. Do not include Markdown fences, comments, or explanatory text. Use the field names and types requested by the task.

Now answer the original task using its requested schema and JSON format only.`

const lineProtocolFewShotPrompt = `Output format: Line Protocol.

Document and names:
- Return exactly one complete root block. Use uppercase BEGIN NAME and END NAME on separate lines, and close nested blocks in reverse opening order with matching names.
- Names are case-insensitive. Block names match [A-Z][A-Z0-9_]*. Field names use the same pattern, optionally with dot-separated segments of that pattern. BEGIN_NAME and END_NAME are also accepted, including mixed space/underscore boundaries; prefer the space form shown below.
- Use the root name requested by the task; the parser does not require it to equal the destination Go type name.
- For fields, use an explicit lineprotocol tag when present, otherwise the json tag name before any comma options, otherwise the Go field name. For child blocks, the same mapping applies; without an explicit lineprotocol tag, the destination struct type name is also accepted. An explicit lineprotocol tag replaces these aliases.
- A dotted name such as META.NOTE is one literal field name, not a path that creates nested objects. Emit it only when declared by the destination schema.
- Never output undeclared fields or blocks, unexported fields, or fields tagged json:"-" or lineprotocol:"-". Use only the current task's declared schema; the examples below do not supply missing schema information.

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

The following examples illustrate formatting only; use the current task's field names, types, tags, and values.

Example 1 input: In RESULT, return message "hello", integer count 2, ratio 0.5, ok true, and enabled false.
Example 1 output:
BEGIN RESULT
MESSAGE="hello"
COUNT=2
RATIO=0.5
OK=true
ENABLED=false
END RESULT

Example 2 input: In PLAN, return goal "inspect", tags "first" and "second", two steps with ids "read" and "write" (steps have lineprotocol tag STEP), and optional string pointer note null.
Example 2 output:
BEGIN PLAN
GOAL="inspect"
TAGS="first"
TAGS="second"
BEGIN STEP
ID="read"
END STEP
BEGIN STEP
ID="write"
END STEP
NOTE=null
END PLAN

Example 3 input: In RESULT, return text containing a quote, a backslash, a newline, and a tab; return literal string "[1,2]" in RAW. The completed boolean has lineprotocol tag COMPLETED, and a string field has lineprotocol tag META.NOTE.
Example 3 output:
BEGIN RESULT
TEXT="quote: \"; slash: \\; newline: \n; tab: \t"
RAW="[1,2]"
COMPLETED=true
META.NOTE="literal dotted field"
END RESULT

Example 4 input: In RESULT, return one tag "only" in a string list and one ITEM in a struct list. ITEM contains a DETAIL struct with message "nested" and an EMPTY struct with no fields. Omit the empty observations list and return optional struct pointer fallback null.
Example 4 output:
BEGIN RESULT
TAGS="only"
BEGIN ITEM
BEGIN DETAIL
MESSAGE="nested"
END DETAIL
BEGIN EMPTY
END EMPTY
END ITEM
FALLBACK=null
END RESULT

Example 5 input: Return an empty NO_TOOL struct.
Example 5 output:
BEGIN NO_TOOL
END NO_TOOL

Invalid forms (do not output these): TAGS=[]; DETAIL={}; MESSAGE=hello; COUNT=NaN; META.NOTE="x" when no such literal field is declared; BEGIN RESULT closed by END PLAN; two root blocks; or a field emitted both as FIELD=null and as a FIELD child block.

Now answer the original task using its declared schema and Line Protocol format only.`
