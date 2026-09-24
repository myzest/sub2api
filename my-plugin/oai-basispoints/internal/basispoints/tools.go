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
	tools    map[string]toolSpec
	entries  []any
	required bool
	forced   string
}
type noExternalSchemas struct{}

func (noExternalSchemas) Load(string) (any, error) {
	return nil, errors.New("external schema references are disabled")
}

func readTools(source object) (*toolCatalog, error) {
	c := &toolCatalog{tools: map[string]toolSpec{}, entries: []any{}}
	if str(source, "tool_choice") == "none" {
		return c, nil
	}
	if raw, ok := source["tools"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("tools 必须为数组")
		}
		if err := c.add(list, "", 0); err != nil {
			return nil, err
		}
	}
	switch v := source["tool_choice"].(type) {
	case nil:
	case string:
		if v != "auto" && v != "required" {
			return nil, errors.New("不支持的 tool_choice")
		}
		c.required = v == "required"
	case object:
		if t := str(v, "type"); t != "function" && t != "custom" {
			return nil, errors.New("tool_choice 只支持 auto/none/required 或单个 function/custom")
		}
		name := str(v, "name")
		if ns := str(v, "namespace"); ns != "" {
			name = ns + "." + name
		}
		spec, ok := c.tools[name]
		if !ok || spec.kind != str(v, "type") {
			return nil, errors.New("tool_choice 指定了未声明的工具")
		}
		c.forced, c.required = name, true
	default:
		return nil, errors.New("无效的 tool_choice")
	}
	if c.required && len(c.tools) == 0 {
		return nil, errors.New("tool_choice=required 需要可用工具")
	}
	return c, nil
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
		name, kind := str(t, "name"), str(t, "type")
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
		if _, ok := c.tools[key]; ok || len(c.tools) >= 512 {
			return errors.New("工具名重复或数量超过 512")
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
		return "This is an external text-only Responses API request. Answer the user's request as assistant text. Do not invoke Excel, Office, workbook, connector, search, or other server tools."
	}
	text := `This request is from an external Responses API client. The JSON catalog below describes the tools that the client can execute. Use the native run_officejs function solely as the relay envelope for exactly one catalog tool per response. Its code field must contain a serialized JSON object, never JavaScript or OfficeJS. For a function tool use {"name":"CATALOG_NAME","arguments":{...}}. For a custom tool use {"name":"CATALOG_NAME","input":"RAW_INPUT"}. Include summary, extended_summary, destructive=false and references=[] in the outer run_officejs arguments. Serialize strings correctly, preserving quotes and backslashes. Use the exact qualified catalog name for namespace tools. Do not nest run_officejs in code. The relay will return the requested tool's result on the next turn. Do not repeat calls whose results are already in the conversation. Do not invoke any other native Excel, workbook, Office, connector, search, or planning tool. Either call one catalog tool using this envelope or answer as assistant text.
Client tool catalog:
`
	text += string(encoded(c.entries))
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
	args, err := decodeObject([]byte(str(native, "arguments")))
	if err != nil {
		return nil, errors.New("run_officejs.arguments 不是有效 JSON")
	}
	var inner object
	switch code := args["code"].(type) {
	case string:
		inner, err = decodeObject([]byte(code))
	case object:
		inner = code
	default:
		err = errors.New("invalid code")
	}
	if err != nil {
		return nil, errors.New("run_officejs.code 必须为一个严格 JSON 对象；不会执行或修复脚本")
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
		input, ok := inner["input"].(string)
		if !ok || inner["arguments"] != nil || inner["args"] != nil {
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
	var native, client object
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
			if native != nil {
				return nil, errors.New("BPS 返回多个工具；当前仅支持串行调用")
			}
			var err error
			client, err = catalog.convert(item)
			if err != nil {
				return nil, err
			}
			native = item
			next = append(next, client)
		default:
			next = append(next, normalizeOutput(item))
		}
	}
	if success && native == nil && catalog.required {
		return nil, errors.New("BPS 未遵守必须调用工具的 tool_choice")
	}
	if native != nil {
		if err := store.save(ctx, native, client); err != nil {
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
