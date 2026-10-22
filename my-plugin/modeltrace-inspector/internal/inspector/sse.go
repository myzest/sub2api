package inspector

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const maxEventSize = 16 * 1024 * 1024

type responseContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}
type responseOutput struct {
	Type    string            `json:"type"`
	Content []responseContent `json:"content"`
	Summary []responseContent `json:"summary"`
}

type responseEnvelope struct {
	ID     string          `json:"id"`
	Model  string          `json:"model"`
	Status string          `json:"status"`
	Usage  json.RawMessage `json:"usage"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails json.RawMessage  `json:"incomplete_details"`
	Output            []responseOutput `json:"output"`
}

type outputPart struct {
	output, index int
	reasoning     bool
}
type streamedOutput struct {
	parts map[outputPart]*strings.Builder
	size  int
}

func (s *streamedOutput) write(key outputPart, text string, replace bool) {
	part := s.parts[key]
	if part == nil {
		if text == "" {
			return
		}
		part = &strings.Builder{}
		s.parts[key] = part
	}
	if replace {
		s.size -= part.Len()
		part.Reset()
	}
	part.WriteString(text)
	s.size += len(text)
}
func (s *streamedOutput) apply(a *Answer) {
	keys := make([]outputPart, 0, len(s.parts))
	for key := range s.parts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].output != keys[j].output {
			return keys[i].output < keys[j].output
		}
		return keys[i].index < keys[j].index
	})
	var text, reasoning strings.Builder
	for _, key := range keys {
		if key.reasoning {
			reasoning.WriteString(s.parts[key].String())
		} else {
			text.WriteString(s.parts[key].String())
		}
	}
	a.Text, a.Reasoning = text.String(), reasoning.String()
}

func (r responseEnvelope) metadata(a *Answer) {
	if r.ID != "" {
		a.ResponseID = r.ID
	}
	if r.Model != "" {
		a.ReturnedModel = r.Model
	}
	if len(r.Usage) > 0 {
		a.Usage = r.Usage
	}
}

func (r responseEnvelope) apply(a *Answer) {
	r.metadata(a)
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
	if a.Error != "" {
		a.ErrorCode = "upstream_error"
	}
}

func streamReadFailure(a *Answer, err error) {
	if errors.Is(err, bufio.ErrTooLong) {
		a.ErrorCode, a.Error = "event_too_large", "上游单条 SSE 事件超过 16 MiB 限制，已保留接收到的部分内容"
		return
	}
	a.ErrorCode = "stream_interrupted"
	a.Error = fmt.Sprintf("上游响应流读取中断（%v），未确认完成；已保留接收到的部分内容", err)
}

func parseResponse(reader io.Reader, contentType string, prompt Prompt) Answer {
	a := Answer{Prompt: prompt}
	if strings.Contains(contentType, "application/json") {
		raw, readErr := io.ReadAll(io.LimitReader(reader, maxEventSize+1))
		if len(raw) > maxEventSize {
			a.ErrorCode, a.Error = "event_too_large", "上游 JSON 响应超过 16 MiB 限制"
			return a
		}
		var r responseEnvelope
		if err := json.Unmarshal(raw, &r); err != nil {
			a.ErrorCode, a.Error = "invalid_event", "上游返回无效 JSON"
			if readErr != nil {
				streamReadFailure(&a, readErr)
			}
			return a
		}
		r.apply(&a)
		if r.Status != "completed" && a.Error == "" {
			a.ErrorCode, a.Error = "stream_incomplete", "上游未确认响应完成"
			if readErr != nil {
				streamReadFailure(&a, readErr)
			}
		}
		return limitAnswer(a)
	}
	// Bound each event and the actual answer, not accumulated keepalives or
	// protocol overhead. Long reasoning streams may contain many small events.
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxEventSize)
	var data strings.Builder
	output := streamedOutput{parts: map[outputPart]*strings.Builder{}}
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
			Type         string           `json:"type"`
			Delta        string           `json:"delta"`
			Text         string           `json:"text"`
			Refusal      string           `json:"refusal"`
			OutputIndex  int              `json:"output_index"`
			ContentIndex int              `json:"content_index"`
			SummaryIndex int              `json:"summary_index"`
			Part         responseContent  `json:"part"`
			Item         responseOutput   `json:"item"`
			Message      string           `json:"message"`
			Response     responseEnvelope `json:"response"`
			Error        *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			a.ErrorCode = "invalid_event"
			return errors.New("上游 SSE 事件不是有效 JSON")
		}
		switch event.Type {
		case "response.created", "response.in_progress":
			event.Response.metadata(&a)
		case "response.output_text.delta", "response.refusal.delta":
			output.write(outputPart{event.OutputIndex, event.ContentIndex, false}, event.Delta, false)
		case "response.reasoning_summary_text.delta":
			output.write(outputPart{event.OutputIndex, event.SummaryIndex, true}, event.Delta, false)
		case "response.output_text.done", "response.refusal.done":
			output.write(outputPart{event.OutputIndex, event.ContentIndex, false}, event.Text+event.Refusal, true)
		case "response.content_part.done":
			output.write(outputPart{event.OutputIndex, event.ContentIndex, false}, event.Part.Text+event.Part.Refusal, true)
		case "response.reasoning_summary_text.done":
			output.write(outputPart{event.OutputIndex, event.SummaryIndex, true}, event.Text, true)
		case "response.reasoning_summary_part.done":
			output.write(outputPart{event.OutputIndex, event.SummaryIndex, true}, event.Part.Text, true)
		case "response.output_item.done":
			for i, part := range event.Item.Content {
				output.write(outputPart{event.OutputIndex, i, false}, part.Text+part.Refusal, true)
			}
			for i, part := range event.Item.Summary {
				output.write(outputPart{event.OutputIndex, i, true}, part.Text, true)
			}
		case "response.completed", "response.failed", "response.incomplete":
			output.apply(&a)
			event.Response.apply(&a)
			terminal = true
			if event.Type != "response.completed" && a.Error == "" {
				a.ErrorCode, a.Error = "upstream_error", "上游未完整完成响应"
			}
			return io.EOF
		case "error":
			a.ErrorCode = "upstream_error"
			a.Error = event.Message
			if event.Error != nil {
				a.Error = event.Error.Message
			}
			if a.Error == "" {
				a.Error = "上游流返回错误"
			}
			return io.EOF
		}
		if output.size > maxOutput {
			a.ErrorCode = "output_too_large"
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
			if data.Len() > maxEventSize {
				a.ErrorCode = "event_too_large"
				parseErr = errors.New("上游单条 SSE 事件超过 16 MiB 限制，已保留接收到的部分内容")
				break
			}
		}
	}
	if parseErr == nil && data.Len() > 0 {
		parseErr = flush()
	}
	if !terminal {
		output.apply(&a)
	}
	if parseErr != nil && parseErr != io.EOF {
		a.Error = parseErr.Error()
	}
	// A completed event is authoritative even when the last Read also reports
	// a closed socket. Otherwise a truncated event is a read failure, not JSON.
	if !terminal && scanner.Err() != nil && (a.Error == "" || a.ErrorCode == "invalid_event") {
		streamReadFailure(&a, scanner.Err())
	}
	if !terminal && a.Error == "" {
		a.ErrorCode, a.Error = "stream_incomplete", "上游流提前结束，未收到 response.completed；已保留接收到的部分内容"
	}
	return limitAnswer(a)
}
func limitAnswer(a Answer) Answer {
	if len(a.Text)+len(a.Reasoning) > maxOutput {
		a.Text = utf8Prefix(a.Text, maxOutput)
		a.Reasoning = utf8Prefix(a.Reasoning, maxOutput-len(a.Text))
		a.Error = "输出超过 2 MiB 上限，结果不完整"
		a.ErrorCode = "output_too_large"
	}
	if len(a.Usage) > 64*1024 {
		a.Usage = nil
	}
	if a.Text == "" && a.Error == "" {
		a.Error = "上游没有返回可展示的文本"
		a.ErrorCode = "empty_response"
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
