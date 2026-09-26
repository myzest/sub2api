package basispoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Protocol shape only: never retain JSON keys, arguments or source code.
type relayFieldDiagnostic struct {
	Layer               int    `json:"layer"`
	Field               string `json:"field"`
	Type                string `json:"type"`
	Bytes               int    `json:"bytes"`
	Fenced              bool   `json:"fenced,omitempty"`
	Extracted           bool   `json:"extracted,omitempty"`
	ErrorKind           string `json:"error_kind,omitempty"`
	ErrorOffset         int64  `json:"error_offset,omitempty"`
	BackslashesRepaired int    `json:"backslashes_repaired,omitempty"`
}

type relayDiagnostic struct {
	State     string                 `json:"state"`
	Transport string                 `json:"transport,omitempty"`
	Unwrapped int                    `json:"unwrapped"`
	Fields    []relayFieldDiagnostic `json:"fields"`
	ErrorKind string                 `json:"error_kind,omitempty"`
}

type relayDecodeError struct {
	kind   string
	field  string
	offset int64
}

func (e *relayDecodeError) Error() string {
	return fmt.Sprintf("工具信封解析失败（%s）；需要完整、无歧义的单个 JSON 对象；详见工具信封诊断", e.kind)
}

func relayName(name string) bool {
	return name == "run_officejs" || name == "functions.run_officejs"
}

func relayJSON(value any, field string, layer int, allowFence bool) (object, relayFieldDiagnostic, error) {
	d := relayFieldDiagnostic{Layer: layer, Field: field, Type: "other"}
	fail := func(kind string, offset int64) (object, relayFieldDiagnostic, error) {
		d.ErrorKind, d.ErrorOffset = kind, offset
		return nil, d, &relayDecodeError{kind: kind, field: field, offset: offset}
	}
	switch v := value.(type) {
	case object:
		d.Type, d.Bytes = "object", len(encoded(v))
		if v == nil {
			return fail("not_object", 0)
		}
		return v, d, nil
	case string:
		d.Type, d.Bytes = "string", len(v)
		text := strings.TrimSpace(v)
		obj, kind, offset := parseRelayObject(text)
		if allowFence && kind != "" && kind != "duplicate_key" && kind != "nesting_limit" && kind != "not_object" {
			candidate, extracted, candidateErr := relayObjectCandidate(text)
			if candidateErr != "" {
				return fail(candidateErr, 0)
			}
			if extracted {
				d.Extracted = true
				d.Fenced = strings.HasPrefix(text, "\x60\x60\x60")
				text = candidate
				obj, kind, offset = parseRelayObject(text)
			}
		}
		if allowFence && kind == "invalid_escape" {
			// Excel bridge 66c41df: only code uses this compatibility pass.
			// The outer/nested arguments must still be valid JSON themselves.
			repaired, count := repairInvalidJSONBackslashes(text)
			if count > 0 {
				d.BackslashesRepaired = count
				obj, kind, offset = parseRelayObject(repaired)
			}
		}
		if kind != "" {
			return fail(kind, offset)
		}
		return obj, d, nil
	case nil:
		d.Type = "missing_or_null"
	case []any:
		d.Type = "array"
	case bool:
		d.Type = "boolean"
	case json.Number:
		d.Type = "number"
	}
	return fail("invalid_type", 0)
}

// Excel's decoder accepts fences, assignments and surrounding prose without
// evaluating them. Use its first complete object, but reject a second object
// and never salvage a nested object from a damaged outer envelope.
func relayObjectCandidate(text string) (string, bool, string) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return text, false, ""
	}
	depth, quoted, escaped := 0, false, false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			quoted = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				if strings.HasSuffix(strings.TrimSpace(text[:start]), "[") || strings.HasPrefix(strings.TrimSpace(text[i+1:]), "]") {
					return text, false, "not_object" // Do not turn a fenced/assigned array into a single call.
				}
				if strings.ContainsAny(text[i+1:], "{}") {
					return text, false, "multiple_objects"
				}
				return text[start : i+1], start != 0 || i+1 != len(text), ""
			}
		}
	}
	return text, false, ""
}

func parseRelayObject(text string) (object, string, int64) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	decoded, err := decodeValue(decoder, 0)
	if err != nil {
		kind, offset := "invalid_json", decoder.InputOffset()
		var syntax *json.SyntaxError
		switch {
		case errors.Is(err, errDuplicateJSONKey):
			kind = "duplicate_key"
		case errors.Is(err, errJSONNesting):
			kind = "nesting_limit"
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			kind = "incomplete_json"
		case errors.As(err, &syntax):
			// Token-by-token decoding can report a token-relative syntax
			// offset. Revalidate the whole text to locate the same error.
			var raw json.RawMessage
			var full *json.SyntaxError
			if err := json.Unmarshal([]byte(text), &raw); errors.As(err, &full) {
				syntax = full
			}
			offset = syntax.Offset
			if strings.Contains(syntax.Error(), "escape") {
				kind = "invalid_escape"
			}
		}
		return nil, kind, offset
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, "trailing_data", decoder.InputOffset()
	}
	obj, ok := decoded.(object)
	if !ok {
		return nil, "not_object", 0
	}
	return obj, "", 0
}

// Port of Excel bridge 66c41df _repair_invalid_json_backslashes. Preserve
// valid JSON escapes; represent an invalid backslash as a literal backslash.
// This pass never evaluates code, adds missing delimiters or extracts objects
// from surrounding prose. The complete candidate is strictly parsed again.
func repairInvalidJSONBackslashes(text string) (string, int) {
	var result strings.Builder
	result.Grow(len(text))
	inString, count := false, 0
	for index := 0; index < len(text); index++ {
		ch := text[index]
		if ch == '"' {
			inString = !inString
		}
		if !inString || ch != '\\' {
			result.WriteByte(ch)
			continue
		}
		valid := false
		if index+1 < len(text) {
			next := text[index+1]
			valid = strings.ContainsRune("\"\\/bfnrt", rune(next))
			if next == 'u' && index+5 < len(text) {
				valid = true
				for _, digit := range []byte(text[index+2 : index+6]) {
					if !((digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f') || (digit >= 'A' && digit <= 'F')) {
						valid = false
						break
					}
				}
			}
		}
		result.WriteByte('\\')
		if valid {
			index++
			result.WriteByte(text[index])
		} else {
			result.WriteByte('\\')
			count++
		}
	}
	if count == 0 {
		return text, 0
	}
	return result.String(), count
}

// Follow CPA/Excel's two-layer unwrapping and Excel's code-text compatibility.
// Multiple objects, duplicate keys and incomplete envelopes remain rejected.
func decodeRelayEnvelope(native object) (object, relayDiagnostic, error) {
	d := relayDiagnostic{State: "rejected"}
	read := func(value any, field string, layer int, fence bool) (object, error) {
		obj, summary, err := relayJSON(value, field, layer, fence)
		d.Fields = append(d.Fields, summary)
		if err != nil {
			d.ErrorKind = summary.ErrorKind
		}
		return obj, err
	}
	args, err := read(native["arguments"], "arguments", 0, false)
	if err != nil {
		return nil, d, err
	}
	// Responses function_call.arguments remains a JSON string. Only the
	// nested relay's arguments/code may use an object representation.
	if _, ok := native["arguments"].(string); !ok {
		d.ErrorKind, d.Fields[0].ErrorKind = "invalid_type", "invalid_type"
		return nil, d, &relayDecodeError{kind: d.ErrorKind}
	}
	for {
		if inner, marked, err := markedRelayEnvelope(args, &d); marked {
			if err == nil {
				d.State = "decoded"
			}
			return inner, d, err
		}
		inner, err := read(args["code"], "code", d.Unwrapped, true)
		if err != nil {
			return nil, d, err
		}
		if !relayName(str(inner, "name")) {
			d.State = "decoded"
			return inner, d, nil
		}
		if d.Unwrapped == 2 {
			d.ErrorKind = "wrapper_limit"
			return nil, d, &relayDecodeError{kind: d.ErrorKind}
		}
		for key := range inner {
			if key != "name" && key != "arguments" {
				d.ErrorKind = "wrapper_fields"
				return nil, d, &relayDecodeError{kind: d.ErrorKind}
			}
		}
		d.Unwrapped++
		args, err = read(inner["arguments"], "arguments", d.Unwrapped, false)
		if err != nil {
			return nil, d, err
		}
	}
}
