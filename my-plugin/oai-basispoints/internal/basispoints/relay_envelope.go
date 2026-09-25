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
	Layer       int    `json:"layer"`
	Field       string `json:"field"`
	Type        string `json:"type"`
	Bytes       int    `json:"bytes"`
	Fenced      bool   `json:"fenced,omitempty"`
	ErrorKind   string `json:"error_kind,omitempty"`
	ErrorOffset int64  `json:"error_offset,omitempty"`
}

type relayDiagnostic struct {
	State     string                 `json:"state"`
	Unwrapped int                    `json:"unwrapped"`
	Fields    []relayFieldDiagnostic `json:"fields"`
	ErrorKind string                 `json:"error_kind,omitempty"`
}

type relayDecodeError struct{ kind string }

func (e *relayDecodeError) Error() string {
	return fmt.Sprintf("工具信封解析失败（%s）；需要单个 JSON 对象，不执行脚本、不猜测修复参数；详见工具信封诊断", e.kind)
}

func relayName(name string) bool {
	return name == "run_officejs" || name == "functions.run_officejs"
}

func relayJSON(value any, field string, layer int, allowFence bool) (object, relayFieldDiagnostic, error) {
	d := relayFieldDiagnostic{Layer: layer, Field: field, Type: "other"}
	fail := func(kind string, offset int64) (object, relayFieldDiagnostic, error) {
		d.ErrorKind, d.ErrorOffset = kind, offset
		return nil, d, &relayDecodeError{kind: kind}
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
		const fence = "\x60\x60\x60"
		if allowFence && strings.HasPrefix(text, fence) {
			header, rest, found := strings.Cut(text, "\n")
			header = strings.TrimSuffix(header, "\r")
			end := strings.LastIndex(rest, "\n")
			if !found || (header != fence && header != fence+"json") || end < 0 || strings.TrimSpace(rest[end+1:]) != fence {
				return fail("invalid_fence", 0)
			}
			text, d.Fenced = rest[:end], true
		}
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
				offset = syntax.Offset
				// Only inspect fixed decoder labels, never retain its message.
				if strings.Contains(syntax.Error(), "escape") {
					kind = "invalid_escape"
				}
			}
			return fail(kind, offset)
		}
		if _, err := decoder.Token(); err != io.EOF {
			return fail("trailing_data", decoder.InputOffset())
		}
		obj, ok := decoded.(object)
		if !ok {
			return fail("not_object", 0)
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

// Follow CPA's two-layer unwrapping. From Excel's text extraction, accept
// only a complete single JSON fence. No assignments or escape repairs.
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
