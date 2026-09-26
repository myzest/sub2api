package basispoints

import (
	"fmt"
	"strings"
)

// Codex Responses Lite carries runtime tools in input.additional_tools.
// Sub2API also moves namespace declarations there before invoking transports.
// Preserve each declaration's position when lowering it to the BPS relay.
type toolGroup struct {
	index int // -1 denotes the top-level tools field.
	tools []any
}

func additionalTools(item object) bool {
	return strings.ToLower(strings.TrimSpace(str(item, "type"))) == "additional_tools"
}

func readToolGroups(source object) ([]toolGroup, error) {
	groups := []toolGroup{}
	if raw := source["tools"]; raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return groups, fmt.Errorf("tools 必须为数组")
		}
		groups = append(groups, toolGroup{-1, list})
	}
	input, _ := source["input"].([]any)
	for index, raw := range input {
		item, ok := raw.(object)
		if !ok || !additionalTools(item) {
			continue
		}
		if role := str(item, "role"); role != "" && role != "developer" {
			return groups, fmt.Errorf("input[%d].additional_tools.role 必须为 developer", index)
		}
		list := []any{}
		if raw := item["tools"]; raw != nil {
			var ok bool
			list, ok = raw.([]any)
			if !ok {
				return groups, fmt.Errorf("input[%d].additional_tools.tools 必须为数组", index)
			}
		}
		groups = append(groups, toolGroup{index, list})
	}
	return groups, nil
}

func (c *toolCatalog) inputWithCatalogs(input []any) []any {
	items := append([]any(nil), input...)
	for index, raw := range input {
		item, ok := raw.(object)
		if !ok || !additionalTools(item) {
			continue
		}
		entries := c.entriesAt[index]
		text := "No additional client tools are enabled at this position for this turn."
		if len(entries) > 0 {
			text = "Additional client tools become available at this point in the conversation. Use these exact catalog names through the run_officejs relay described above; they execute in the external client's environment.\nClient tool catalog:\n" + relayCatalog(entries)
		}
		// Consume the private carrier. Leaving it intact would also expose
		// native declarations to BPS outside our validated relay/KV path.
		items[index] = message("developer", text)
	}
	return items
}
