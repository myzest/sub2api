package basispoints

import (
	"context"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type toolSpec struct {
	key, name, namespace, kind string
	spec                       object
	schema                     *jsonschema.Schema
}
type toolCatalog struct {
	tools        map[string]toolSpec
	entries      []any
	entriesAt    map[int][]any
	required     bool
	forced       string
	parallel     bool
	observeRelay func(relayDiagnostic)
}
type noExternalSchemas struct{}

func (noExternalSchemas) Load(string) (any, error) {
	return nil, errors.New("external schema references are disabled")
}

func readTools(source object) (*toolCatalog, error) {
	c := &toolCatalog{tools: map[string]toolSpec{}, entries: []any{}, entriesAt: map[int][]any{-1: {}}, parallel: source["parallel_tool_calls"] != false}
	if str(source, "tool_choice") == "none" {
		return c, nil
	}
	groups, err := readToolGroups(source)
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		start := len(c.entries)
		if err := c.add(group.tools, "", 0); err != nil {
			return nil, err
		}
		c.entriesAt[group.index] = c.entries[start:]
	}
	switch v := source["tool_choice"].(type) {
	case nil:
	case string:
		if v != "auto" && v != "required" {
			return nil, errors.New("不支持的 tool_choice")
		}
		c.required = v == "required"
	case object:
		if str(v, "type") == "allowed_tools" {
			mode := str(v, "mode")
			if mode != "" && mode != "auto" && mode != "required" {
				return nil, errors.New("allowed_tools.mode 只支持 auto/required")
			}
			list, ok := v["tools"].([]any)
			if !ok {
				return nil, errors.New("allowed_tools.tools 必须为数组")
			}
			selected := map[string]bool{}
			for _, raw := range list {
				tool, ok := raw.(object)
				if !ok {
					return nil, errors.New("allowed_tools 的工具声明必须为对象")
				}
				name, err := c.selectedTool(tool)
				if err != nil {
					return nil, err
				}
				selected[name] = true
			}
			c.restrict(selected)
			c.required = mode == "required"
			break
		}
		if t := str(v, "type"); t != "function" && t != "custom" {
			return nil, errors.New("tool_choice 只支持 auto/none/required、function/custom 或 allowed_tools")
		}
		name, err := c.selectedTool(v)
		if err != nil {
			return nil, err
		}
		c.forced, c.required = name, true
		c.restrict(map[string]bool{name: true})
	default:
		return nil, errors.New("无效的 tool_choice")
	}
	if c.required && len(c.tools) == 0 {
		return nil, errors.New("tool_choice=required 需要可用工具")
	}
	return c, nil
}

func (c *toolCatalog) selectedTool(choice object) (string, error) {
	name := str(choice, "name")
	if namespace := str(choice, "namespace"); namespace != "" {
		name = namespace + "." + name
	}
	spec, ok := c.tools[name]
	if !ok || spec.kind != str(choice, "type") {
		return "", errors.New("tool_choice 指定了未声明的工具")
	}
	return name, nil
}

func (c *toolCatalog) restrict(selected map[string]bool) {
	for key := range c.tools {
		if !selected[key] {
			delete(c.tools, key)
		}
	}
	entries := []any{}
	for _, raw := range c.entries {
		if selected[str(raw.(object), "name")] {
			entries = append(entries, raw)
		}
	}
	c.entries = entries
	for index, group := range c.entriesAt {
		kept := []any{}
		for _, raw := range group {
			if selected[str(raw.(object), "name")] {
				kept = append(kept, raw)
			}
		}
		c.entriesAt[index] = kept
	}
}
func (c *toolCatalog) add(list []any, namespace string, depth int) error {
	if depth > 4 {
		return errors.New("工具 namespace 嵌套过深")
	}
	for _, v := range list {
		t, ok := v.(object)
		if !ok {
			continue
		}
		name, kind := strings.TrimSpace(str(t, "name")), strings.ToLower(strings.TrimSpace(str(t, "type")))
		// _iter_client_tools in excel-codex-bridge only catalogs callable
		// function/custom leaves. Other declarations are not BPS wire tools.
		if kind != "function" && kind != "custom" && kind != "namespace" {
			continue
		}
		if name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\t ") {
			return errors.New("工具 name 无效")
		}
		key := name
		if namespace != "" {
			key = namespace + "." + name
		}
		if kind == "namespace" {
			children, ok := t["tools"].([]any)
			if !ok {
				return errors.New("namespace.tools 必须为数组")
			}
			if err := c.add(children, key, depth+1); err != nil {
				return err
			}
			continue
		}
		if previous, ok := c.tools[key]; ok {
			if previous.kind != kind || previous.namespace != namespace || digest(previous.spec) != digest(t) {
				return errors.New("同名客户端工具存在冲突定义")
			}
			continue
		}
		if len(c.tools) >= 512 {
			return errors.New("客户端工具数量超过 512")
		}
		spec := toolSpec{key: key, name: name, namespace: namespace, kind: kind, spec: t}
		entry := clone(t)
		entry["name"] = key
		if kind == "function" {
			parameters := t["parameters"]
			if parameters == nil {
				parameters = t["inputSchema"]
			}
			if parameters == nil {
				parameters = t["input_schema"]
			}
			if parameters == nil {
				parameters = object{"type": "object"}
			}
			compiler := jsonschema.NewCompiler()
			compiler.UseLoader(noExternalSchemas{})
			const location = "https://schemas.invalid/tool.json"
			if err := compiler.AddResource(location, parameters); err != nil {
				return errors.New("工具 JSON Schema 无效")
			}
			schema, err := compiler.Compile(location)
			if err != nil {
				return errors.New("工具 JSON Schema 无效或包含外部引用；请使用内联 schema")
			}
			spec.schema = schema
			entry["parameters"] = parameters
		}
		c.tools[key] = spec
		c.entries = append(c.entries, entry)
	}
	return nil
}

func (c *toolCatalog) instructions() string {
	if len(c.tools) == 0 {
		return "This request comes from an external Responses API client. No client tools are available for this turn. Answer using the supplied inputs as assistant text. Do not invoke native Excel, Office, workbook, connector, search, or other server tools."
	}
	text := `This request is from an external Codex/Responses API client. The JSON catalogs in developer messages describe real tools executed by that client in its own environment, not in the Excel workbook. The initial catalog below may be empty; additional catalogs can appear later in the input and become available from that position. When asked to inspect a local project, invoke the declared shell or file tools, including a custom code-execution tool when provided, to inspect it; the directory name alone is not the file contents. Do not claim that local files or shell access are unavailable when the catalog provides them. Follow the client's skill instructions and use its declared tools to read relevant SKILL.md files when needed. Never invent file contents or tool execution results.
Use the native run_officejs function solely as a relay envelope containing exactly one catalog tool per call. Its code field must contain a serialized JSON object, never JavaScript or OfficeJS. For a function tool use {"name":"CATALOG_NAME","arguments":{...}}. For a custom tool use {"name":"CATALOG_NAME","input":"RAW_INPUT"}. Include summary, extended_summary, destructive=false and references=[] in the outer run_officejs arguments. Serialize strings correctly, preserving quotes and backslashes. Use the exact qualified catalog name for namespace tools. Do not nest run_officejs in code. The relay will return the requested tool's result on the next turn. Do not repeat calls whose results are already in the conversation. Do not invoke any other native Excel, workbook, Office, connector, search, or planning tool.
Client tool catalog:
`
	text += string(encoded(c.entriesAt[-1]))
	text += "\nEncoding example only (replace EXACT_CATALOG_NAME with a declared custom tool): " + string(encoded(object{
		"summary": "Call client tool", "extended_summary": "Relay one declared tool", "destructive": false, "references": []any{},
		"code": string(encoded(object{"name": "EXACT_CATALOG_NAME", "input": "const path = \"C:\\\\workspace\\\\file.txt\";\nconst quoted = \"\\\"hello\\\"\";"})),
	}))
	text += "\nSerialize the inner object once, then JSON-escape that string as the outer code value. Preserve raw custom input including every newline, quote and backslash. No Markdown fences, assignments or surrounding prose in code. The example is not an additional tool declaration."
	if !c.parallel {
		text += "\nInvoke at most one catalog tool in this response."
	}
	if c.forced != "" {
		text += "\nThis response must call the catalog tool " + c.forced + "."
	} else if c.required {
		text += "\nThis response must make one catalog tool call."
	}
	return text
}

func (c *toolCatalog) convert(native object) (object, error) {
	if str(native, "type") != "function_call" || (str(native, "name") != "run_officejs" && str(native, "name") != "functions.run_officejs") {
		return nil, errors.New("BPS 返回了不支持的原生工具；未释放给客户端")
	}
	if str(native, "call_id") == "" || str(native, "id") == "" {
		return nil, errors.New("BPS 工具调用缺少原生身份，不能可靠回放")
	}
	inner, diagnostic, err := decodeRelayEnvelope(native)
	if c.observeRelay != nil {
		c.observeRelay(diagnostic)
	}
	if err != nil {
		return nil, err
	}
	for key := range inner {
		if key != "name" && key != "tool" && key != "arguments" && key != "args" && key != "input" {
			return nil, errors.New("工具信封包含未知字段")
		}
	}
	name := str(inner, "name")
	if _, exists := inner["tool"]; exists {
		if _, both := inner["name"]; both {
			return nil, errors.New("工具信封同时包含 name 和 tool")
		}
		name = str(inner, "tool")
	}
	spec, ok := c.tools[name]
	if !ok {
		return nil, errors.New("BPS 请求了未声明的客户端工具")
	}
	if c.forced != "" && name != c.forced {
		return nil, errors.New("BPS 工具调用不符合 tool_choice")
	}
	prefix, kind := "fc_", "function_call"
	if spec.kind == "custom" {
		prefix, kind = "ctc_", "custom_tool_call"
	}
	id := prefix + "bp_" + newID()
	client := object{"id": id, "call_id": id, "type": kind, "name": spec.name, "status": "completed"}
	if spec.namespace != "" {
		client["namespace"] = spec.namespace
	}
	if spec.kind == "custom" {
		value := inner["input"]
		if args, exists := inner["args"]; exists {
			if _, both := inner["input"]; both {
				return nil, errors.New("custom 工具信封同时包含 input 和 args")
			}
			value = args
		}
		input, ok := value.(string)
		if !ok || inner["arguments"] != nil {
			return nil, errors.New("custom 工具需要字符串 input")
		}
		client["input"] = input
	} else {
		value := inner["arguments"]
		if _, exists := inner["args"]; exists {
			if _, both := inner["arguments"]; both {
				return nil, errors.New("工具信封同时包含 arguments 和 args")
			}
			value = inner["args"]
		}
		if text, ok := value.(string); ok {
			value, err = decodeObject([]byte(text))
			if err != nil {
				return nil, errors.New("客户端工具 arguments 必须为 JSON 对象")
			}
		}
		obj, ok := value.(object)
		if !ok || inner["input"] != nil {
			return nil, errors.New("客户端工具 arguments 必须为 JSON 对象")
		}
		if spec.schema.Validate(obj) != nil {
			return nil, errors.New("BPS 工具参数不符合客户端 JSON Schema")
		}
		client["arguments"] = string(encoded(obj))
	}
	return client, nil
}

func transformResponse(ctx context.Context, response object, catalog *toolCatalog, store replayStore, success bool) (object, error) {
	output, ok := response["output"].([]any)
	if !ok {
		return nil, errors.New("BPS 响应缺少 output 数组")
	}
	next := make([]any, 0, len(output))
	var calls []struct{ native, client object }
	callIDs, itemIDs := map[string]bool{}, map[string]bool{}
	for _, v := range output {
		item, ok := v.(object)
		if !ok {
			return nil, errors.New("BPS output 项不是对象")
		}
		switch str(item, "type") {
		case "function_call", "custom_tool_call":
			if !success {
				continue
			}
			if len(calls) > 0 && !catalog.parallel {
				return nil, errors.New("BPS 返回多个工具，但客户端禁止并行调用")
			}
			if callIDs[str(item, "call_id")] || itemIDs[str(item, "id")] {
				return nil, errors.New("BPS 返回重复的工具调用身份")
			}
			client, err := catalog.convert(item)
			if err != nil {
				return nil, err
			}
			callIDs[str(item, "call_id")], itemIDs[str(item, "id")] = true, true
			calls = append(calls, struct{ native, client object }{item, client})
			next = append(next, client)
		default:
			next = append(next, normalizeOutput(item))
		}
	}
	if success && len(calls) == 0 && catalog.required {
		return nil, errors.New("BPS 未遵守必须调用工具的 tool_choice")
	}
	for _, call := range calls {
		if err := store.save(ctx, call.native, call.client); err != nil {
			return nil, err
		}
	}
	result := clone(response)
	result["output"] = next
	return result, nil
}
func normalizeOutput(item object) object {
	n := clone(item)
	if str(n, "type") == "reasoning" && n["summary"] == nil {
		n["summary"] = []any{}
	}
	return n
}
