package basispoints

import (
	"strings"
	"unicode"
)

// Protocol reference: ranxi2001/sub2api f671a8d (custom_transport.go and
// function_code_transport.go). A marker opts in; unmarked code remains JSON.
const (
	rawCustomPrefix       = "codex2api.custom/"
	rawFunctionCodePrefix = "codex2api.function_code/"
	maxRawTransportBytes  = 1 << 20
)

func rawTransportName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/\\") &&
		strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func functionCodeSchema(parameters any) bool {
	schema, _ := parameters.(object)
	properties, _ := schema["properties"].(object)
	code, _ := properties["code"].(object)
	return str(schema, "type") == "object" && str(code, "type") == "string"
}

// Raw text is not parsed, repaired, trimmed, evaluated or searched for commands.
// The caller validates the exact catalog name, kind, schema and native identity.
func markedRelayEnvelope(args object, d *relayDiagnostic) (object, bool, error) {
	summary := str(args, "summary")
	prefix, mode := "", ""
	switch {
	case strings.HasPrefix(summary, rawCustomPrefix):
		prefix, mode = rawCustomPrefix, "custom"
	case strings.HasPrefix(summary, rawFunctionCodePrefix):
		prefix, mode = rawFunctionCodePrefix, "function_code"
	default:
		return nil, false, nil
	}
	d.Transport = mode
	fail := func(kind, field string) (object, bool, error) {
		d.ErrorKind = kind
		return nil, true, &relayDecodeError{kind: kind, field: field}
	}
	name := strings.TrimPrefix(summary, prefix)
	if !rawTransportName(name) {
		return fail("raw_transport_name", "summary")
	}
	code, ok := args["code"].(string)
	shape := relayFieldDiagnostic{Layer: d.Unwrapped, Field: "code", Type: "string", Bytes: len(code)}
	if !ok {
		shape.Type = "invalid_type"
		shape.ErrorKind = "invalid_type"
	}
	d.Fields = append(d.Fields, shape)
	if !ok {
		return fail("invalid_type", "code")
	}
	if len(code) > maxRawTransportBytes {
		return fail("raw_transport_limit", "code")
	}
	if mode == "custom" {
		return object{"name": name, "input": code}, true, nil
	}
	metadata, ok := args["extended_summary"].(string)
	if !ok {
		return fail("invalid_type", "extended_summary")
	}
	if len(metadata) > maxRawTransportBytes-len(code) {
		return fail("raw_transport_limit", "extended_summary")
	}
	other, diagnostic, err := relayJSON(metadata, "extended_summary", d.Unwrapped, false)
	d.Fields = append(d.Fields, diagnostic)
	if err != nil {
		d.ErrorKind = diagnostic.ErrorKind
		return nil, true, err
	}
	if _, exists := other["code"]; exists {
		return fail("duplicate_code", "extended_summary")
	}
	other["code"] = code
	if len(encoded(other)) > maxRawTransportBytes {
		return fail("raw_transport_limit", "extended_summary")
	}
	return object{"name": name, "arguments": other}, true, nil
}

// Annotate both the initial and positional additional_tools catalogs. The
// parameters/format contracts are preserved; only relay documentation changes.
func relayCatalog(entries []any) string {
	var descriptions []string
	for _, value := range entries {
		entry, ok := value.(object)
		if !ok {
			continue
		}
		name, kind := str(entry, "name"), str(entry, "type")
		if rawTransportName(name) && kind == "custom" {
			descriptions = append(descriptions, "CUSTOM "+name+": set summary exactly "+rawCustomPrefix+name+"; put the exact raw input in code, without a JSON envelope or Markdown fences.")
		} else if kind == "custom" {
			descriptions = append(descriptions, "CUSTOM_JSON "+name+": this name cannot use a raw marker; code must contain one legacy JSON envelope with name and string input.")
		} else if rawTransportName(name) && kind == "function" && functionCodeSchema(entry["parameters"]) {
			descriptions = append(descriptions, "FUNCTION_CODE "+name+": set summary exactly "+rawFunctionCodePrefix+name+"; put the exact code argument in native code. Put all other supplied arguments as one JSON object in extended_summary ({} if none); do not duplicate code there.")
		} else {
			descriptions = append(descriptions, "FUNCTION "+name+": code contains one JSON envelope with name and arguments.")
		}
	}
	return string(encoded(entries)) + "\nTransport for each exact catalog name:\n" + strings.Join(descriptions, "\n")
}
