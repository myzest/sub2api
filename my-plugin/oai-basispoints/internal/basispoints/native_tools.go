package basispoints

import (
	"errors"
	"fmt"
	"strings"
)

func restoreNativePlan(args object) object {
	plan, ok := args["plan"].([]any)
	if !ok {
		return args
	}
	steps := []any{}
	for index, raw := range plan {
		item, _ := raw.(object)
		step, stepOK := item["step"].(string)
		status, statusOK := item["status"].(string)
		if stepOK && statusOK {
			steps = append(steps, object{"id": fmt.Sprintf("step%d", index+1), "description": step, "status": status, "result": ""})
		}
	}
	summary := str(args, "explanation")
	if summary == "" {
		summary = "Update task plan"
	}
	return object{"summary": summary, "plan": steps}
}

// excel_upstream._client_call_from_native also accepts an already declared
// native tool, especially update_plan. Names still resolve against this
// request's restricted client catalog, and arguments still pass its schema.
func (c *toolCatalog) directEnvelope(native object) (object, error) {
	name := str(native, "name")
	if namespace := str(native, "namespace"); namespace != "" {
		name = namespace + "." + name
	}
	name = c.originalName(name)
	spec, exists := c.tools[name]
	if !exists {
		return nil, errors.New("BPS 返回了未声明的原生工具；未释放给客户端")
	}
	inner := object{"name": name}
	if spec.kind == "custom" && str(native, "type") == "custom_tool_call" {
		input, ok := native["input"].(string)
		if !ok {
			return nil, errors.New("原生 custom 工具缺少字符串 input")
		}
		inner["input"] = input
		return inner, nil
	}
	if spec.kind != "function" || str(native, "type") != "function_call" {
		return nil, errors.New("原生工具类型与客户端声明不匹配")
	}
	args, err := decodeObject([]byte(str(native, "arguments")))
	if err != nil {
		return nil, errors.New("原生工具 arguments 必须为 JSON 对象")
	}
	if name == "update_plan" {
		args = normalizeNativePlan(args)
	}
	inner["arguments"] = args
	return inner, nil
}

func (c *toolCatalog) originalName(name string) string {
	if _, exists := c.tools[name]; exists {
		return name
	}
	return strings.TrimPrefix(name, "codex_client__")
}

func normalizeNativePlan(args object) object {
	plan, ok := args["plan"].([]any)
	if !ok {
		return args
	}
	aliases := map[string]string{
		"pending": "pending", "not_started": "pending", "todo": "pending", "planned": "pending", "queued": "pending", "blocked": "pending",
		"in_progress": "in_progress", "active": "in_progress", "started": "in_progress", "doing": "in_progress", "current": "in_progress",
		"completed": "completed", "complete": "completed", "done": "completed", "finished": "completed",
	}
	steps := []any{}
	for _, raw := range plan {
		step, _ := raw.(object)
		var text string
		found := false
		for _, key := range []string{"step", "description", "title"} {
			if value, ok := step[key].(string); ok {
				text, found = value, true
				break
			}
		}
		status, ok := step["status"].(string)
		if !found || !ok {
			continue
		}
		key := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(status)))
		if canonical, ok := aliases[key]; ok {
			status = canonical
		}
		steps = append(steps, object{"step": text, "status": status})
	}
	result := object{"plan": steps}
	explanation, ok := args["explanation"].(string)
	if !ok {
		explanation = str(args, "summary")
	}
	if explanation != "" {
		result["explanation"] = explanation
	}
	return result
}

const transportRetryGuidance = "The previous run_officejs relay was rejected. Retry once using exactly the transport documented for the intended catalog tool: FUNCTION uses one JSON name/arguments envelope; CUSTOM uses the codex2api.custom/CATALOG_NAME summary and exact raw input in code; FUNCTION_CODE uses its codex2api.function_code/CATALOG_NAME summary, exact code and other arguments as JSON in extended_summary. Serialize outer arguments correctly. Do not nest run_officejs, repeat an identical rejected payload, or change the intended tool arguments."

func normalizeNativeToolOutput(native object, output any) any {
	if str(native, "name") == "update_plan" {
		return "{\"status\":\"ok\"}"
	}
	if relayName(str(native, "name")) {
		if text, err := outputText(output); err == nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "unsupported call: run_officejs") {
			return transportRetryGuidance
		}
	}
	if text, ok := output.(string); (ok && strings.TrimSpace(text) == "") || output == nil {
		return "(tool call succeeded with no output)"
	}
	return output
}
