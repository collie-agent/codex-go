package provider

import (
	"encoding/json"
	"regexp"
	"strings"
)

var protocolItemStart = regexp.MustCompile(`(?i)^(?:BEGIN[ _]|END[ _]|[A-Z][A-Z0-9_.]*=)`)

// normalizeProtocolSpacing converts separators between complete protocol items
// to newlines. Quoted JSON content is never changed, even when it contains
// strings that look like protocol fields or block boundaries.
func normalizeProtocolSpacing(text string) string {
	var out strings.Builder
	start := 0
	quoted, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c == '\r' || c == '\n' {
			out.WriteString(text[start : i+1])
			start = i + 1
			continue
		}
		if c != ' ' && c != '\t' {
			continue
		}
		item := text[start:i]
		complete := lineBoundary.MatchString(item)
		if field := lineScalar.FindStringSubmatch(item); field != nil {
			complete = json.Valid([]byte(field[2]))
		}
		if !complete {
			continue
		}
		next := i
		for next < len(text) && (text[next] == ' ' || text[next] == '\t') {
			next++
		}
		if !protocolItemStart.MatchString(text[next:]) {
			continue
		}
		out.WriteString(item)
		out.WriteByte('\n')
		start = next
		i = next - 1
	}
	out.WriteString(text[start:])
	return out.String()
}
