package basispoints

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type probeRoundResult struct {
	Round         int    `json:"round"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	ResponseID    string `json:"response_id,omitempty"`
	ReturnedModel string `json:"returned_model,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	Usage         any    `json:"usage,omitempty"`
}

// Publish a stable copy; the worker continues updating its own probe value.
func (s *Server) publishProbe(p *Probe) {
	copy := *p
	copy.Rounds = append([]probeRoundResult(nil), p.Rounds...)
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
// desktop requests. The only tool is virtual: no shell or filesystem execution.
func (s *Server) probeTools(ctx context.Context, p *Probe, cfg Config, h, headers http.Header, proxy string) error {
	p.ToolName = "diagnostics.read_probe"
	p.ToolExpected = "BPS_TOOL_" + newID()
	input := []any{message("user", "Call diagnostics.read_probe with path=probe.txt. After the tool result arrives, reply only with the exact challenge value from that result. This is a simulated read-only diagnostic tool, not a real filesystem. Do not invent the result.")}
	source := object{
		"model": p.Model, "stream": true, "reasoning": object{"effort": p.Effort},
		"prompt_cache_key": "bps-probe-" + p.ID, "input": input, "parallel_tool_calls": false,
		"tool_choice": object{"type": "allowed_tools", "mode": "required", "tools": []any{object{"type": "function", "name": "read_probe", "namespace": "diagnostics"}}},
		"tools": []any{object{"type": "namespace", "name": "diagnostics", "tools": []any{
			object{"type": "function", "name": "read_probe", "description": "Returns a simulated diagnostic challenge. No files are accessed.", "parameters": object{
				"type": "object", "properties": object{"path": object{"type": "string", "enum": []any{"probe.txt"}}},
				"required": []any{"path"}, "additionalProperties": false,
			}},
		}}},
	}
	p.Round = 1
	first, err := s.probeRound(ctx, p, cfg, source, h, headers, proxy)
	if err != nil {
		return err
	}
	p.Stage = "tool_check"
	calls := responseToolCalls(first)
	p.ToolCalls = len(calls)
	if len(calls) != 1 || str(calls[0], "type") != "function_call" || str(calls[0], "name") != "read_probe" || str(calls[0], "namespace") != "diagnostics" {
		return errors.New("工具探测第一轮没有返回预期的命名空间 function_call")
	}
	output, _ := first["output"].([]any)
	input = append(input, output...)
	input = append(input, object{
		"type": "function_call_output", "call_id": calls[0]["call_id"],
		"output": string(encoded(object{"challenge": p.ToolExpected, "simulated": true})),
	})
	source["input"], source["tool_choice"] = input, "none"
	p.Round = 2
	second, err := s.probeRound(ctx, p, cfg, source, h, headers, proxy)
	if err != nil {
		return err
	}
	p.Stage = "tool_result_check"
	text := responseText(second)
	p.ToolReply = redactProbeText(text, headers, "")
	if strings.TrimSpace(text) != p.ToolExpected {
		return errors.New("工具第二轮已响应，但未原样返回模拟结果中的随机值；请检查实际回复，尚不能确认工具回放可用")
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
