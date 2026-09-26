package basispoints

import (
	"encoding/json"
	"fmt"
	"strings"
)

const toolTraceLimit = 16

// These are client-reported protocol facts, not verified execution outcomes.
// In particular, queued/opened/completed do not prove a rendered preview.
type clientToolDiagnostic struct {
	InputIndex   int      `json:"input_index"`
	Type         string   `json:"type"`
	Name         string   `json:"name,omitempty"`
	OutputFormat string   `json:"output_format,omitempty"`
	Signals      []string `json:"signals,omitempty"`
}

type outputToolDiagnostic struct {
	Type               string `json:"type"`
	Name               string `json:"name"`
	ArgumentEncryption string `json:"argument_encryption,omitempty"`
}

func diagnosticToolName(item object) string {
	name := str(item, "name")
	if namespace := str(item, "namespace"); namespace != "" && name != "" {
		name = namespace + "." + name
	}
	return limitCharacters(redactProbeText(name, nil, ""), 200)
}

func (d *requestDiagnostic) clientToolHistory(input []any) {
	// Link results only to an unambiguous, type-matched call supplied in this
	// request. Never retain call IDs, inspect tool code or fetch additional KV.
	names, kinds := map[string]string{}, map[string]string{}
	lastUser := -1
	for index, raw := range input {
		item, _ := raw.(object)
		if strings.EqualFold(strings.TrimSpace(str(item, "role")), "user") {
			lastUser = index
		}
		switch str(item, "type") {
		case "function_call", "custom_tool_call":
			id := str(item, "call_id")
			if id == "" {
				continue
			}
			if _, exists := names[id]; exists {
				names[id], kinds[id] = "", ""
			} else {
				names[id], kinds[id] = diagnosticToolName(item), str(item, "type")
			}
		}
	}
	d.ClientToolHistory, d.ClientToolHistoryCount = []clientToolDiagnostic{}, 0
	// Walk backwards to keep the most recent entries without allocating a
	// diagnostic for every item in a long conversation.
	for index := len(input) - 1; index > lastUser; index-- {
		item, _ := input[index].(object)
		kind := str(item, "type")
		isResult := kind == "function_call_output" || kind == "custom_tool_call_output"
		if !isResult && kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		d.ClientToolHistoryCount++
		if len(d.ClientToolHistory) >= toolTraceLimit {
			continue
		}
		entry := clientToolDiagnostic{InputIndex: index, Type: kind, Name: diagnosticToolName(item)}
		if isResult {
			entry.Name = ""
			id := str(item, "call_id")
			if kind == kinds[id]+"_output" {
				entry.Name = names[id]
			}
			value, present := item["output"]
			entry.OutputFormat = "other"
			switch value.(type) {
			case nil:
				entry.OutputFormat = "null"
			case string:
				entry.OutputFormat = "string"
			case object:
				entry.OutputFormat = "object"
			case []any:
				entry.OutputFormat = "array"
			}
			if !present {
				entry.OutputFormat = "missing"
			}
			budget := 64 << 10
			collectToolResultSignals(value, "output", 0, &budget, &entry.Signals)
		}
		d.ClientToolHistory = append(d.ClientToolHistory, entry)
	}
	for left, right := 0, len(d.ClientToolHistory)-1; left < right; left, right = left+1, right-1 {
		d.ClientToolHistory[left], d.ClientToolHistory[right] = d.ClientToolHistory[right], d.ClientToolHistory[left]
	}
}

// Only inspect explicit machine fields and documented MCP/Responses text
// containers. No substring/error-message guessing, script parsing, arbitrary
// object traversal, URLs, paths or free-form output are stored in diagnostics.
func collectToolResultSignals(value any, path string, depth int, budget *int, signals *[]string) {
	if depth > 6 || len(*signals) >= 8 {
		return
	}
	switch value := value.(type) {
	case string:
		if len(value) > *budget {
			return
		}
		*budget -= len(value)
		if obj, err := decodeObject([]byte(value)); err == nil {
			collectToolResultSignals(obj, path, depth+1, budget, signals)
		}
	case object:
		for _, key := range []string{"status", "isError", "is_error", "exit_code"} {
			if len(*signals) >= 8 {
				return
			}
			var label string
			switch key {
			case "status":
				switch status := str(value, key); status {
				case "queued", "pending", "running", "in_progress", "accepted", "opened", "completed", "failed", "error", "rejected", "denied", "blocked", "canceled", "cancelled", "ok", "success", "succeeded":
					label = status
				}
			case "isError", "is_error":
				if flag, ok := value[key].(bool); ok {
					label = fmt.Sprint(flag)
				}
			case "exit_code":
				if number, ok := value[key].(json.Number); ok {
					if code, err := number.Int64(); err == nil && code >= -2147483648 && code <= 4294967295 {
						label = fmt.Sprint(code)
					}
				}
			}
			if label != "" {
				*signals = append(*signals, path+"."+key+"="+label)
			}
		}
		if content, ok := value["structuredContent"].(object); ok {
			collectToolResultSignals(content, path+".structuredContent", depth+1, budget, signals)
		}
		if content, ok := value["content"].([]any); ok {
			collectToolResultSignals(content, path+".content", depth+1, budget, signals)
		}
		switch str(value, "type") {
		case "text", "input_text", "output_text":
			if text, ok := value["text"].(string); ok {
				collectToolResultSignals(text, path+".text", depth+1, budget, signals)
			}
		}
	case []any:
		for index, part := range value[:min(len(value), toolTraceLimit)] {
			collectToolResultSignals(part, fmt.Sprintf("%s[%d]", path, index), depth+1, budget, signals)
		}
	}
}

func (d *requestDiagnostic) output(response object) {
	calls := responseToolCalls(response)
	d.OutputToolCalls = len(calls)
	d.OutputTools = []outputToolDiagnostic{}
	for _, call := range calls[:min(len(calls), toolTraceLimit)] {
		d.OutputTools = append(d.OutputTools, outputToolDiagnostic{Type: str(call, "type"), Name: diagnosticToolName(call), ArgumentEncryption: argumentEncryptionState(call)})
	}
}
