package basispoints

import (
	"slices"
	"strings"
	"time"
)

// Bounded completed-request summaries are retained only in memory. Raw request
// bodies, image URLs/pixels, schemas and tool arguments are never stored here.
type requestDiagnostic struct {
	ID                   string            `json:"id"`
	Version              string            `json:"version"`
	Instance             string            `json:"instance"`
	Origin               string            `json:"origin"`
	AccountID            int64             `json:"account_id"`
	Model                string            `json:"model,omitempty"`
	StartedAt            int64             `json:"started_at"`
	FinishedAt           int64             `json:"finished_at"`
	Stage                string            `json:"stage"`
	HTTPStatus           int               `json:"http_status,omitempty"`
	AttachmentHTTPStatus int               `json:"attachment_http_status,omitempty"`
	Attachment           *attachmentReport `json:"attachment,omitempty"`
	ImageTransport       string            `json:"image_transport"`
	ReasoningEffort      string            `json:"reasoning_effort,omitempty"`
	RequestBytes         int               `json:"request_bytes,omitempty"`
	UpstreamRequestBytes int               `json:"upstream_request_bytes,omitempty"`
	RequestID            string            `json:"request_id,omitempty"`
	ContentType          string            `json:"content_type,omitempty"`
	UpstreamError        string            `json:"upstream_error,omitempty"`
	UpstreamErrorState   string            `json:"upstream_error_state,omitempty"`
	Images               []imageDiagnostic `json:"images,omitempty"`
	UpstreamImages       []imageDiagnostic `json:"upstream_images,omitempty"`
	ToolTypes            map[string]int    `json:"tool_types"`
	ToolSources          map[string]int    `json:"tool_sources"`
	AdditionalToolItems  int               `json:"additional_tool_items"`
	CallableTools        int               `json:"callable_tools"`
	ToolNames            []string          `json:"tool_names"`
	HistoryTypes         map[string]int    `json:"history_types,omitempty"`
	HistoryHandles       map[string]int    `json:"history_handles,omitempty"`
	ToolChoice           string            `json:"tool_choice"`
	ParallelToolCalls    bool              `json:"parallel_tool_calls"`
	InputImages          int               `json:"input_images"`
	OutputToolCalls      int               `json:"output_tool_calls"`
	NativeToolTypes      map[string]int    `json:"native_tool_types,omitempty"`
	NativeToolNames      []string          `json:"native_tool_names,omitempty"`
	Terminal             string            `json:"terminal,omitempty"`
	Error                string            `json:"error,omitempty"`
}

func (d *requestDiagnostic) input(raw []byte) {
	d.RequestBytes = len(raw)
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
	if d.ToolTypes == nil {
		d.ToolTypes = map[string]int{}
	}
	d.ToolSources = map[string]int{}
	groups, _ := readToolGroups(source) // prepare reports malformed carriers.
	for _, group := range groups {
		name := "tools"
		if group.index >= 0 {
			name = "input.additional_tools"
			d.AdditionalToolItems++
		}
		d.ToolSources[name] += len(group.tools)
		count(group.tools, 0)
	}
	d.Images, d.InputImages = summarizeImages(source)
	d.HistoryTypes, d.HistoryHandles = map[string]int{}, map[string]int{}
	input, _ := source["input"].([]any)
	for _, raw := range input {
		item, ok := raw.(object)
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(str(item, "type")))
		switch kind {
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
			d.HistoryTypes[kind]++
			d.HistoryHandles[replayHandleKind(str(item, "call_id"))]++
		}
	}
}

// Record labels before relay validation so a rejected native tool is visible
// without retaining its arguments, input text, response body or credentials.
func (d *requestDiagnostic) observe(event object) {
	switch str(event, "type") {
	case "response.completed", "response.failed", "response.incomplete":
	default:
		return
	}
	response, _ := event["response"].(object)
	d.NativeToolTypes = map[string]int{}
	d.NativeToolNames = []string{}
	for _, call := range responseToolCalls(response) {
		d.NativeToolTypes[str(call, "type")]++
		name := str(call, "name")
		if namespace := str(call, "namespace"); namespace != "" {
			name = namespace + "." + name
		}
		name = limitCharacters(redactProbeText(name, nil, ""), 200)
		if len(d.NativeToolNames) < 32 && !slices.Contains(d.NativeToolNames, name) {
			d.NativeToolNames = append(d.NativeToolNames, name)
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
		d.ToolNames = append(d.ToolNames, limitCharacters(redactProbeText(name, nil, ""), 200))
	}
}

func (s *Server) finishDiagnostic(d *requestDiagnostic) {
	d.FinishedAt = time.Now().Unix()
	s.mu.Lock()
	s.lastRequest = d
	s.recentRequests = prependDiagnostic(s.recentRequests, d, 20)
	if d.InputImages > 0 {
		s.imageRequests = prependDiagnostic(s.imageRequests, d, 10)
	}
	s.mu.Unlock()
}

func prependDiagnostic(history []*requestDiagnostic, d *requestDiagnostic, limit int) []*requestDiagnostic {
	next := make([]*requestDiagnostic, 1, min(len(history)+1, limit))
	next[0] = d
	return append(next, history[:min(len(history), limit-1)]...)
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
