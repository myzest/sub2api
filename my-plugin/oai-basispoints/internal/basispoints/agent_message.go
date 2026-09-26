package basispoints

import (
	"fmt"
	"strings"
)

// Encryption labels alone do not prove corruption or age. Both original
// adapters preserve ordinary input items; BPS validates opaque content.
// This inspection is diagnostic only and must not rewrite or reject input.
type agentInputDiagnostic struct {
	Messages       int      `json:"messages"`
	EncryptedParts int      `json:"encrypted_parts"`
	EncryptedPaths []string `json:"encrypted_paths,omitempty"`
	Handling       string   `json:"handling,omitempty"`
}

func inspectAgentInput(input []any) agentInputDiagnostic {
	d := agentInputDiagnostic{}
	for index, raw := range input {
		item, _ := raw.(object)
		if strings.ToLower(strings.TrimSpace(str(item, "type"))) != "agent_message" {
			continue
		}
		d.Messages++
		parts, _ := item["content"].([]any)
		for partIndex, rawPart := range parts {
			part, _ := rawPart.(object)
			if strings.ToLower(strings.TrimSpace(str(part, "type"))) != "encrypted_content" {
				continue
			}
			d.EncryptedParts++
			if len(d.EncryptedPaths) < toolTraceLimit {
				d.EncryptedPaths = append(d.EncryptedPaths, fmt.Sprintf("input[%d].content[%d]", index, partIndex))
			}
		}
	}
	return d
}

// Only classify protocol metadata, never retain argument names or values.
func argumentEncryptionState(call object) string {
	if str(call, "type") != "function_call" {
		return ""
	}
	metadata, exists := call["encrypted_function_args"]
	if !exists {
		return "absent"
	}
	if metadata == nil {
		return "null"
	}
	count := 0
	switch list := metadata.(type) {
	case []any:
		count = len(list)
	case []string:
		count = len(list)
	default:
		return "invalid"
	}
	if count == 0 {
		return "plaintext"
	}
	return "declared"
}
