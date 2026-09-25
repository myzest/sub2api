package basispoints

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type probeRoundResult struct {
	Round         int               `json:"round"`
	HTTPStatus    int               `json:"http_status,omitempty"`
	RequestID     string            `json:"request_id,omitempty"`
	ResponseID    string            `json:"response_id,omitempty"`
	ReturnedModel string            `json:"returned_model,omitempty"`
	LatencyMS     int64             `json:"latency_ms"`
	Usage         any               `json:"usage,omitempty"`
	Relay         []relayDiagnostic `json:"relay,omitempty"`
	Replay        *replayDiagnostic `json:"replay,omitempty"`
}

// Publish a stable copy; the worker continues updating its own probe value.
func (s *Server) publishProbe(p *Probe) {
	copy := *p
	copy.Rounds = append([]probeRoundResult(nil), p.Rounds...)
	copy.Relay = append([]relayDiagnostic(nil), p.Relay...)
	if p.Replay != nil {
		replay := *p.Replay
		copy.Replay = &replay
	}
	s.mu.Lock()
	s.probe = &copy
	s.mu.Unlock()
}

func (s *Server) probeOnce(ctx context.Context, p *Probe, cfg Config) error {
	p.Stage = "identity"
	i, err := s.identity(ctx, p.AccountID)
	if err != nil {
		return err
	}
	h := headersFromProto(i.Headers)
	h.Set("Authorization", "Bearer "+i.Token)
	if p.Kind == "image_route" {
		return s.probeImageRoute(ctx, p, cfg, h, i.ProxyUrl)
	}
	headers, err := bpsHeaders(h)
	if err != nil {
		return err
	}
	if p.Kind == "tools" {
		return s.probeTools(ctx, p, cfg, h, headers, i.ProxyUrl)
	}
	source, err := probeSource(p)
	if err != nil {
		return err
	}
	p.Round = 1
	response, err := s.probeRound(ctx, p, cfg, source, h, headers, i.ProxyUrl)
	if err != nil {
		return err
	}
	text := responseText(response)
	if strings.TrimSpace(text) == "" {
		return errors.New("BPS 返回 completed 但没有文本，探测未通过")
	}
	if p.Kind == "image" {
		p.Stage = "image_check"
		p.ImageReply = redactProbeText(text, headers, p.ImagePreview)
		if strings.TrimSpace(text) != p.ImageExpected {
			return errors.New("BPS 已完成响应，但回复与图中六位数字不完全一致；请查看测试图和实际回复，不能仅据此判定通道不支持图片")
		}
	}
	p.Stage = "completed"
	return nil
}

// This exercises the same prepare, native relay, KV and restore path used by
// desktop requests. Both tools are virtual: no shell or filesystem execution.
const toolProbeInput = `const file = "C:\\workspace\\probe.txt";
const pattern = /\(probe\)/;
const data = {"quote":"\"hello\"","nested":{"enabled":true}};`

func (s *Server) probeTools(ctx context.Context, p *Probe, cfg Config, h, headers http.Header, proxy string) error {
	p.ToolName = "diagnostics.read_probe / diagnostics.echo_probe"
	p.ToolType = "custom / function"
	p.ToolSource = "input.additional_tools"
	customChallenge, functionChallenge := "BPS_CUSTOM_"+newID(), "BPS_FUNCTION_"+newID()
	p.ToolExpected = customChallenge + "/" + functionChallenge
	functionArgs := object{"script": toolProbeInput, "nested": object{"quote": "\"quoted\"\nsecond line", "integer": json.Number("9007199254740993")}}
	input := []any{
		message("user", "Call diagnostics.read_probe. Its exact raw input is the decoded value of this JSON string: "+string(encoded(toolProbeInput))+". Preserve every character, including newlines and backslashes. This is a virtual diagnostic tool; do not execute the text or invent its result."),
		object{"type": "additional_tools", "role": "developer", "tools": []any{
			object{"type": "namespace", "name": "diagnostics", "tools": []any{
				object{"type": "custom", "name": "read_probe", "description": "Returns a simulated diagnostic challenge. Copy the requested raw input exactly. No code is executed and no files are accessed.", "format": object{"type": "text"}},
				object{"type": "function", "name": "echo_probe", "description": "Returns another simulated challenge. Echo the exact requested JSON arguments; no code is executed.", "parameters": object{"type": "object", "properties": object{"script": object{"type": "string"}, "nested": object{"type": "object", "properties": object{"quote": object{"type": "string"}, "integer": object{"type": "integer"}}, "required": []any{"quote", "integer"}, "additionalProperties": false}}, "required": []any{"script", "nested"}, "additionalProperties": false}},
			}},
		}},
	}
	source := object{
		"model": p.Model, "stream": true, "reasoning": object{"effort": p.Effort},
		"prompt_cache_key": "bps-probe-" + p.ID, "input": input, "parallel_tool_calls": false,
		"tool_choice": object{"type": "allowed_tools", "mode": "required", "tools": []any{object{"type": "custom", "name": "read_probe", "namespace": "diagnostics"}}},
	}
	p.Round = 1
	first, err := s.probeRound(ctx, p, cfg, source, h, headers, proxy)
	if err != nil {
		return err
	}
	p.Stage = "tool_check"
	calls := responseToolCalls(first)
	p.ToolCalls = len(calls)
	if len(calls) != 1 || str(calls[0], "type") != "custom_tool_call" || str(calls[0], "name") != "read_probe" || str(calls[0], "namespace") != "diagnostics" || str(calls[0], "input") != toolProbeInput {
		return errors.New("工具探测第一轮没有返回预期的命名空间 custom_tool_call 及原始 input")
	}
	output, _ := first["output"].([]any)
	input = append(input, output...)
	input = append(input, object{
		"type": "custom_tool_call_output", "call_id": calls[0]["call_id"],
		"output": string(encoded(object{"challenge": customChallenge, "simulated": true})),
	})
	input = append(input, message("user", "Now call diagnostics.echo_probe with exactly these JSON arguments: "+string(encoded(functionArgs))+". Preserve the integer exactly."))
	source["input"], source["tool_choice"] = input, object{"type": "function", "name": "echo_probe", "namespace": "diagnostics"}
	p.Round = 2
	second, err := s.probeRound(ctx, p, cfg, source, h, headers, proxy)
	if err != nil {
		return err
	}
	p.Stage = "tool_check"
	calls = responseToolCalls(second)
	p.ToolCalls += len(calls)
	if len(calls) != 1 || str(calls[0], "type") != "function_call" || str(calls[0], "name") != "echo_probe" || str(calls[0], "namespace") != "diagnostics" {
		return errors.New("工具探测第二轮没有返回预期的命名空间 function_call")
	}
	args, err := decodeObject([]byte(str(calls[0], "arguments")))
	if err != nil || digest(args) != digest(functionArgs) {
		return errors.New("工具探测第二轮的嵌套参数、转义或大整数未完整保留")
	}
	output, _ = second["output"].([]any)
	input = append(input, output...)
	input = append(input, object{"type": "function_call_output", "call_id": calls[0]["call_id"], "output": string(encoded(object{"challenge": functionChallenge, "simulated": true}))}, message("user", "Reply only with the challenge from read_probe, then a slash /, then the challenge from echo_probe. Use the supplied tool results."))
	source["input"], source["tool_choice"] = input, "none"
	p.Round = 3
	third, err := s.probeRound(ctx, p, cfg, source, h, headers, proxy)
	if err != nil {
		return err
	}
	p.Stage = "tool_result_check"
	text := responseText(third)
	p.ToolReply = redactProbeText(text, headers, "")
	if strings.TrimSpace(text) != p.ToolExpected {
		return errors.New("工具第三轮已响应，但未原样返回两次模拟结果中的随机值；尚不能确认多轮工具回放可用")
	}
	p.Stage = "completed"
	return nil
}

func responseText(response object) string {
	var text strings.Builder
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item, _ := raw.(object)
		if str(item, "type") != "message" {
			continue
		}
		parts, _ := item["content"].([]any)
		for _, raw := range parts {
			part, _ := raw.(object)
			if str(part, "type") == "output_text" {
				text.WriteString(str(part, "text"))
			}
		}
	}
	return text.String()
}
