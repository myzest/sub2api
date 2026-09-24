package inspector

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type responseEnvelope struct {
	ID     string          `json:"id"`
	Model  string          `json:"model"`
	Status string          `json:"status"`
	Usage  json.RawMessage `json:"usage"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails json.RawMessage `json:"incomplete_details"`
	Output            []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"output"`
}

func (r responseEnvelope) apply(a *Answer) {
	a.ResponseID, a.ReturnedModel, a.Usage = r.ID, r.Model, r.Usage
	var text, reasoning strings.Builder
	for _, output := range r.Output {
		for _, c := range output.Content {
			if c.Type == "output_text" {
				text.WriteString(c.Text)
			}
			if c.Type == "refusal" {
				text.WriteString(c.Refusal)
			}
		}
		for _, c := range output.Summary {
			reasoning.WriteString(c.Text)
		}
	}
	if text.Len() > 0 {
		a.Text = text.String()
	}
	if reasoning.Len() > 0 {
		a.Reasoning = reasoning.String()
	}
	if r.Error != nil {
		a.Error = r.Error.Message
	}
	if r.Status == "incomplete" {
		a.Error = "上游响应不完整：" + string(r.IncompleteDetails)
	}
	if r.Status == "failed" && a.Error == "" {
		a.Error = "上游响应失败"
	}
}

func parseResponse(reader io.Reader, contentType string, prompt Prompt) Answer {
	a := Answer{Prompt: prompt}
	if strings.Contains(contentType, "application/json") {
		raw, err := io.ReadAll(io.LimitReader(reader, 16*1024*1024+1))
		if err != nil || len(raw) > 16*1024*1024 {
			a.Error = "读取上游 JSON 响应失败或响应过大"
			return a
		}
		var r responseEnvelope
		if err := json.Unmarshal(raw, &r); err != nil {
			a.Error = "上游返回无效 JSON"
			return a
		}
		r.apply(&a)
		if r.Status != "completed" && a.Error == "" {
			a.Error = "上游未确认响应完成"
		}
		return limitAnswer(a)
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, 32*1024*1024))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var data strings.Builder
	var text, reasoning strings.Builder
	terminal := false
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if raw == "[DONE]" {
			return io.EOF
		}
		var event struct {
			Type     string           `json:"type"`
			Delta    string           `json:"delta"`
			Message  string           `json:"message"`
			Response responseEnvelope `json:"response"`
			Error    *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return errors.New("上游 SSE 事件不是有效 JSON")
		}
		switch event.Type {
		case "response.output_text.delta", "response.refusal.delta":
			text.WriteString(event.Delta)
		case "response.reasoning_summary_text.delta":
			reasoning.WriteString(event.Delta)
		case "response.completed", "response.failed", "response.incomplete":
			a.Text, a.Reasoning = text.String(), reasoning.String()
			event.Response.apply(&a)
			terminal = true
			if event.Type != "response.completed" && a.Error == "" {
				a.Error = "上游未完整完成响应"
			}
			return io.EOF
		case "error":
			a.Error = event.Message
			if event.Error != nil {
				a.Error = event.Error.Message
			}
			if a.Error == "" {
				a.Error = "上游流返回错误"
			}
			return io.EOF
		}
		if text.Len()+reasoning.Len() > maxOutput {
			return errors.New("输出超过 2 MiB 上限，已保留接收到的部分内容")
		}
		return nil
	}
	var parseErr error
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if parseErr = flush(); parseErr != nil {
				break
			}
		} else if strings.HasPrefix(line, "data:") {
			line = strings.TrimPrefix(line, "data:")
			line = strings.TrimPrefix(line, " ")
			data.WriteString(line)
			data.WriteByte('\n')
			if data.Len() > 16*1024*1024 {
				parseErr = errors.New("上游 SSE 事件超过大小限制")
				break
			}
		}
	}
	if parseErr == nil && data.Len() > 0 {
		parseErr = flush()
	}
	if !terminal {
		a.Text, a.Reasoning = text.String(), reasoning.String()
	}
	if parseErr != nil && parseErr != io.EOF {
		a.Error = parseErr.Error()
	}
	if scanner.Err() != nil && a.Error == "" {
		a.Error = "上游响应流读取中断或事件过大"
	}
	if !terminal && a.Error == "" {
		a.Error = "上游流提前结束，未收到 response.completed"
	}
	return limitAnswer(a)
}
func limitAnswer(a Answer) Answer {
	if len(a.Text)+len(a.Reasoning) > maxOutput {
		a.Text = utf8Prefix(a.Text, maxOutput)
		a.Reasoning = utf8Prefix(a.Reasoning, maxOutput-len(a.Text))
		a.Error = "输出超过 2 MiB 上限，结果不完整"
	}
	if len(a.Usage) > 64*1024 {
		a.Usage = nil
	}
	if a.Text == "" && a.Error == "" {
		a.Error = "上游没有返回可展示的文本"
	}
	return a
}
func utf8Prefix(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && s[limit]&0xc0 == 0x80 {
		limit--
	}
	return s[:limit]
}
