package context

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// FormatHistoricalResult decodes the result envelope once. Text leaves are never
// decoded again: source code and regular expressions may contain literal escapes.
func FormatHistoricalResult(raw string) string {
	if !json.Valid([]byte(raw)) {
		return raw
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return raw
	}
	var b strings.Builder
	var write func(any, int)
	write = func(value any, depth int) {
		indent := strings.Repeat("  ", depth)
		switch v := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				fmt.Fprintf(&b, "%s- %s:\n", indent, strings.NewReplacer("\n", " ", "\r", " ").Replace(key))
				write(v[key], depth+1)
			}
		case []any:
			for i, item := range v {
				fmt.Fprintf(&b, "%s- Item %d:\n", indent, i+1)
				write(item, depth+1)
			}
		default:
			text := fmt.Sprint(v)
			if v == nil {
				text = "null"
			}
			b.WriteByte('\n')
			for _, line := range strings.Split(text, "\n") {
				fmt.Fprintf(&b, "%s> %s\n", indent, line)
			}
			b.WriteByte('\n')
		}
	}
	write(value, 0)
	return strings.TrimRight(b.String(), "\n")
}
