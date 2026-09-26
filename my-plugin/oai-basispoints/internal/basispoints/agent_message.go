package basispoints

import (
	"errors"
	"fmt"
	"strings"
)

var errAgentEncryptedContent = errors.New("本插件不支持 agent_message 中的 encrypted_content，不能安全重解释为明文；请在升级后的父任务重新发起明文委派，不要继续重试旧子任务消息")

// The reference's agent_message_test.go rejects both plain-looking strings
// and ciphertext labelled encrypted_content. Do not guess, decrypt or drop
// them. Scope this check to agent messages; reasoning/compaction stay intact.
type agentInputDiagnostic struct {
	Messages       int      `json:"messages"`
	EncryptedParts int      `json:"encrypted_parts"`
	EncryptedPaths []string `json:"encrypted_paths,omitempty"`
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

func validateAgentInput(input []any) error {
	d := inspectAgentInput(input)
	if d.EncryptedParts > 0 {
		return fmt.Errorf("%s.type=encrypted_content: %w", d.EncryptedPaths[0], errAgentEncryptedContent)
	}
	return nil
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
