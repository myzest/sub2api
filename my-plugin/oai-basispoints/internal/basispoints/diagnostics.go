package basispoints

import (
	"slices"
	"strings"
	"time"
)

// Only the last completed routed request is retained, in memory. This contains
// protocol labels and counts, never prompts, schemas, tool arguments or pixels.
type requestDiagnostic struct {
	ID                   string         `json:"id"`
	AccountID            int64          `json:"account_id"`
	Model                string         `json:"model,omitempty"`
	StartedAt            int64          `json:"started_at"`
	FinishedAt           int64          `json:"finished_at"`
	Stage                string         `json:"stage"`
	HTTPStatus           int            `json:"http_status,omitempty"`
	AttachmentHTTPStatus int            `json:"attachment_http_status,omitempty"`
	ToolTypes            map[string]int `json:"tool_types"`
	CallableTools        int            `json:"callable_tools"`
	ToolNames            []string       `json:"tool_names"`
	ToolChoice           string         `json:"tool_choice"`
	ParallelToolCalls    bool           `json:"parallel_tool_calls"`
	InputImages          int            `json:"input_images"`
	OutputToolCalls      int            `json:"output_tool_calls"`
	Terminal             string         `json:"terminal,omitempty"`
	Error                string         `json:"error,omitempty"`
}

func (d *requestDiagnostic) input(raw []byte) {
	source, err := decodeObject(raw)
	if err != nil {
		return
	}
	if model := str(source, "model"); modelPattern.MatchString(model) {
		d.Model = model
	}
	d.ParallelToolCalls = source["parallel_tool_calls"] != false
	d.ToolChoice = "auto"
	switch choice := source["tool_choice"].(type) {
	case string:
		switch choice {
		case "auto", "none", "required":
			d.ToolChoice = choice
		default:
			d.ToolChoice = "invalid"
		}
	case object:
		switch str(choice, "type") {
		case "function", "custom":
			d.ToolChoice = str(choice, "type")
		case "allowed_tools":
			d.ToolChoice = "allowed_tools"
			if mode := str(choice, "mode"); mode == "auto" || mode == "required" {
				d.ToolChoice += "." + mode
			}
		default:
			d.ToolChoice = "unsupported"
		}
	}
	var count func([]any, int)
	count = func(list []any, depth int) {
		if depth > 4 {
			return
		}
		for _, raw := range list {
			tool, ok := raw.(object)
			if !ok {
				continue
			}
			kind := strings.ToLower(strings.TrimSpace(str(tool, "type")))
			switch kind {
			case "function", "custom", "namespace", "tool_search", "web_search", "web_search_preview", "file_search", "computer", "computer_use_preview", "mcp", "shell", "apply_patch", "image_generation", "code_interpreter", "local_shell":
			default:
				kind = "other"
			}
			d.ToolTypes[kind]++
			if kind == "namespace" {
				children, _ := tool["tools"].([]any)
				count(children, depth+1)
			}
		}
	}
	tools, _ := source["tools"].([]any)
	count(tools, 0)
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item, _ := raw.(object)
		for _, key := range []string{"content", "output"} {
			parts, _ := item[key].([]any)
			for _, raw := range parts {
				part, _ := raw.(object)
				if str(part, "type") == "input_image" {
					d.InputImages++
				}
			}
		}
	}
}

func (d *requestDiagnostic) catalog(c *toolCatalog) {
	d.CallableTools = len(c.tools)
	names := make([]string, 0, len(c.tools))
	for name := range c.tools {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names[:min(len(names), 32)] {
		d.ToolNames = append(d.ToolNames, redactProbeText(name, nil, ""))
	}
}

func (s *Server) finishDiagnostic(d *requestDiagnostic) {
	d.FinishedAt = time.Now().Unix()
	s.mu.Lock()
	s.lastRequest = d
	s.mu.Unlock()
}

func responseToolCalls(response object) []object {
	var calls []object
	output, _ := response["output"].([]any)
	for _, raw := range output {
		if item, ok := raw.(object); ok && nativeTool(item) {
			calls = append(calls, item)
		}
	}
	return calls
}
