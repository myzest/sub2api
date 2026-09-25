package basispoints

import (
	"errors"
	"strings"
)

// A malformed plugin handle must not bypass account/scope/expiry checks by
// being treated as imported history. These prefixes match the host's ID edits.
func replayHandleKind(handle string) string {
	if handle == "" {
		return "missing"
	}
	if handlePattern.MatchString(handle) {
		return "plugin"
	}
	// Strip repeated known prefixes only to recognize the reserved family.
	// They remain invalid: this does not normalize them into valid KV handles.
	value := strings.TrimSpace(handle)
	for {
		previous := value
		for _, prefix := range []string{"fc_", "ctc_", "call_"} {
			if strings.HasPrefix(value, prefix) {
				value = strings.TrimPrefix(value, prefix)
				break
			}
		}
		if value == previous {
			break
		}
	}
	if strings.HasPrefix(value, "bp_") {
		return "invalid_plugin"
	}
	return "external"
}

// Adapt complete client-supplied history using CPA fallbackTransportCall and
// Excel _fallback_transport_call. This creates historical input, never a new
// client command, and does not read or write another account's KV state.
func historicalTransportCall(item object) (object, error) {
	handle := str(item, "call_id")
	if handle == "" || len(handle) > 512 || strings.ContainsAny(handle, "\r\n\t ") {
		return nil, errors.New("旧工具历史缺少有效 call_id；需要完整调用及对应结果")
	}
	name, namespace := str(item, "name"), str(item, "namespace")
	if value := item["namespace"]; value != nil {
		if _, ok := value.(string); !ok {
			return nil, errors.New("旧工具历史的 namespace 必须为字符串")
		}
	}
	if name == "" || len(name) > 512 || len(namespace) > 512 || strings.ContainsAny(name+namespace, "\r\n\t ") {
		return nil, errors.New("旧工具历史缺少有效工具名称")
	}
	if namespace != "" {
		name = namespace + "." + name
	}
	inner := object{"name": name}
	if str(item, "type") == "custom_tool_call" {
		input, ok := item["input"].(string)
		if !ok {
			return nil, errors.New("旧 custom 工具历史需要原始字符串 input")
		}
		inner["input"] = input
	} else {
		args, err := decodeObject([]byte(str(item, "arguments")))
		if err != nil {
			return nil, errors.New("旧 function 工具历史需要有效 JSON 对象 arguments")
		}
		inner["arguments"] = args
		// The reference also accepts a native relay already present in the
		// supplied history. Preserve all its original fields and state.
		if name == "run_officejs" || name == "functions.run_officejs" {
			native := clone(item)
			if str(native, "id") == "" {
				native["id"] = functionItemID(handle)
			}
			return native, nil
		}
	}
	return object{
		"type": "function_call", "id": functionItemID(handle), "call_id": handle,
		"name": "run_officejs", "status": "completed",
		"arguments": string(encoded(object{
			"summary": "Run client tool " + name, "extended_summary": "Relay " + name + " through the external client",
			"code": string(encoded(inner)), "destructive": false, "references": []any{},
		})),
	}, nil
}

func functionItemID(handle string) string {
	if strings.HasPrefix(handle, "fc_") {
		return handle
	}
	return "fc_" + handle
}
