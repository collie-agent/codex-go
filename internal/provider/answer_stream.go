package provider

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// answerStream keeps provisional answer text separate from final result validation.
// Once the answer starts, only its unfinished string suffix is retained.
type answerStream struct {
	lineProtocol bool
	prefix       string
	pending      string
	started      bool
	done         bool
}

func (s *answerStream) feed(delta string) string {
	if s.done {
		return ""
	}
	if !s.started {
		s.prefix += delta
		offset := -1
		if s.lineProtocol {
			s.prefix = normalizeProtocolSpacing(s.prefix)
			offset = lineAnswerOffset(s.prefix)
		} else {
			offset = jsonAnswerOffset(s.prefix)
		}
		if offset >= 0 {
			s.pending, s.started, s.prefix = s.prefix[offset:], true, ""
		}
		if !s.started {
			return ""
		}
	} else {
		s.pending += delta
	}
	var out strings.Builder
	for len(s.pending) > 0 {
		c := s.pending[0]
		if c == '"' {
			s.done = true
			s.pending = ""
			break
		}
		if c == '\\' {
			n := 2
			if len(s.pending) < n {
				break
			}
			if s.pending[1] == 'u' {
				n = 6
				if len(s.pending) < n {
					break
				}
				// A high surrogate must wait for its low surrogate across chunk boundaries.
				if strings.HasPrefix(strings.ToLower(s.pending[2:6]), "d") && strings.ContainsRune("89ab", rune(strings.ToLower(s.pending[3:4])[0])) {
					n = 12
					if len(s.pending) < n {
						break
					}
				}
			}
			var decoded string
			if json.Unmarshal([]byte("\""+s.pending[:n]+"\""), &decoded) != nil {
				s.done = true
				break
			}
			out.WriteString(decoded)
			s.pending = s.pending[n:]
			continue
		}
		if c < 0x20 {
			s.done = true
			break
		}
		if c >= utf8.RuneSelf {
			if !utf8.FullRuneInString(s.pending) {
				break
			}
			_, n := utf8.DecodeRuneInString(s.pending)
			out.WriteString(s.pending[:n])
			s.pending = s.pending[n:]
			continue
		}
		out.WriteByte(c)
		s.pending = s.pending[1:]
	}
	return out.String()
}

// Locate only the root answer; other objects may contain unrelated answer keys.
func jsonAnswerOffset(prefix string) int { return jsonAnswerObjectOffset(prefix, true) }

func jsonAnswerObjectOffset(prefix string, allowResult bool) int {
	d := json.NewDecoder(strings.NewReader(prefix))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return -1
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return -1
		}
		offset := int(d.InputOffset())
		if key == "answer" {
			rest := strings.TrimSpace(prefix[offset:])
			if !strings.HasPrefix(rest, ":") {
				return -1
			}
			rest = strings.TrimSpace(rest[1:])
			if !strings.HasPrefix(rest, "\"") {
				return -1
			}
			return len(prefix) - len(rest) + 1
		}
		if key == "result" && allowResult {
			rest := strings.TrimSpace(prefix[offset:])
			if strings.HasPrefix(rest, ":") {
				rest = strings.TrimSpace(rest[1:])
				if nested := jsonAnswerObjectOffset(rest, false); nested >= 0 {
					return len(prefix) - len(rest) + nested
				}
			}
		}
		var skip json.RawMessage
		if d.Decode(&skip) != nil {
			return -1
		}
	}
	return -1
}

func lineAnswerOffset(prefix string) int {
	depth, offset := 0, 0
	nestedResult := false
	// Protocol boundaries accept CR, LF and CRLF.
	for _, line := range strings.FieldsFunc(prefix, func(r rune) bool { return r == '\r' || r == '\n' }) {
		pos := strings.Index(prefix[offset:], line) + offset
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "BEGIN ") || strings.HasPrefix(upper, "BEGIN_") {
			depth++
			if depth == 2 {
				nestedResult = upper == "BEGIN RESULT" || upper == "BEGIN_RESULT"
			}
		}
		if (depth == 1 || (depth == 2 && nestedResult)) && strings.HasPrefix(upper, "ANSWER=\"") {
			return pos + len("ANSWER=\"")
		}
		if strings.HasPrefix(upper, "END ") || strings.HasPrefix(upper, "END_") {
			depth--
		}
		offset = pos + len(line)
	}
	return -1
}
