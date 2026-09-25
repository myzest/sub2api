package basispoints

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

type eventWriter func([]byte) error
type relay struct {
	ctx      context.Context
	plan     *requestPlan
	emit     eventWriter
	sequence int
	seen     map[int]object
	done     map[int]object
	response object
	terminal string
	observe  func(object) // Optional diagnostics for explicit probes only.
}

func newRelay(ctx context.Context, plan *requestPlan, emit eventWriter) *relay {
	return &relay{ctx: ctx, plan: plan, emit: emit, seen: map[int]object{}, done: map[int]object{}}
}
func (r *relay) send(event object) error {
	if r.emit == nil {
		return nil
	}
	e := clone(event)
	e["sequence_number"] = r.sequence
	r.sequence++
	return r.emit([]byte("event: " + str(e, "type") + "\ndata: " + string(encoded(e)) + "\n\n"))
}
func indexOf(e object) (int, error) {
	v, ok := e["output_index"].(json.Number)
	if !ok {
		return 0, errors.New("SSE 缺少 output_index")
	}
	n, err := v.Int64()
	if err != nil || n < 0 || n >= 1024 {
		return 0, errors.New("SSE output_index 超出限制")
	}
	return int(n), nil
}
func nativeTool(item object) bool {
	return str(item, "type") == "function_call" || str(item, "type") == "custom_tool_call"
}
func (r *relay) event(e object) (bool, error) {
	if r.observe != nil {
		r.observe(e)
	}
	t := str(e, "type")
	switch t {
	case "response.completed", "response.failed", "response.incomplete":
		response, ok := e["response"].(object)
		if !ok {
			return false, errors.New("BPS 终态事件缺少 response")
		}
		return true, r.finish(e, response)
	case "error", "response.error":
		return false, errors.New("BPS 返回流内错误，未完成响应")
	case "response.output_item.added", "response.output_item.done":
		item, ok := e["item"].(object)
		if !ok {
			return false, errors.New("BPS output item 不是对象")
		}
		i, err := indexOf(e)
		if nativeTool(item) {
			if err != nil {
				return false, err
			}
			if old := r.seen[i]; old != nil {
				if t == "response.output_item.added" || str(old, "type") != str(item, "type") || str(old, "id") != str(item, "id") {
					return false, errors.New("BPS 原生工具身份变化或索引重复")
				}
			}
		}
		if err == nil {
			if old := r.seen[i]; old != nil && nativeTool(old) && !nativeTool(item) {
				return false, errors.New("BPS 原生工具 output_index 被其他项覆盖")
			}
			r.seen[i] = item
			if t == "response.output_item.done" {
				r.done[i] = item
			}
		}
		if nativeTool(item) {
			return false, nil
		}
		e = clone(e)
		e["item"] = normalizeOutput(item)
		return false, r.send(e)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return false, nil // Never release unvalidated native tool names/arguments.
	case "response.created", "response.in_progress", "response.queued":
		if res, ok := e["response"].(object); ok {
			if output, ok := res["output"].([]any); ok {
				for _, value := range output {
					if item, ok := value.(object); ok && nativeTool(item) {
						return false, errors.New("BPS 在非终态响应中嵌入原生工具，无法安全转换")
					}
				}
			}
		}
		return false, r.send(e)
	default:
		if strings.HasPrefix(t, "response.function_call_arguments.") || strings.HasPrefix(t, "response.custom_tool_call_input.") {
			return false, nil
		}
		if item, ok := e["item"].(object); ok && nativeTool(item) {
			return false, errors.New("BPS 在未知事件中嵌入原生工具，无法安全转换")
		}
		// excel_tool_stream_transform forwards all remaining event types.
		// Text/reasoning deltas need not be preceded by a locally known item.
		return false, r.send(e)
	}
}

func sameNative(a, b object) bool {
	for _, k := range []string{"id", "type", "call_id", "name", "arguments", "input", "namespace"} {
		if str(a, k) != str(b, k) {
			return false
		}
	}
	return true
}
func (r *relay) finish(event object, raw object) error {
	name := str(event, "type")
	status := strings.TrimPrefix(name, "response.")
	if str(raw, "status") != status {
		return errors.New("BPS 终态事件与 response.status 不一致")
	}
	output, ok := raw["output"].([]any)
	if !ok || len(output) > 1024 {
		return errors.New("BPS 终态 output 无效")
	}
	for index, item := range r.seen {
		if !nativeTool(item) {
			continue
		}
		if index >= len(output) {
			return errors.New("BPS 终态缺少已发送的 output item")
		}
		terminal, ok := output[index].(object)
		if !ok || str(terminal, "id") != str(item, "id") || str(terminal, "type") != str(item, "type") {
			return errors.New("BPS 终态与流式 output item 不一致")
		}
		if done := r.done[index]; done != nil {
			if nativeTool(done) && !sameNative(done, terminal) {
				return errors.New("BPS 原生工具完成项与终态不一致")
			}
		}
	}
	result, err := transformResponse(r.ctx, raw, r.plan.tools, r.plan.store, status == "completed")
	if err != nil {
		return err
	}
	if status == "failed" {
		result["error"] = object{"code": "bps_response_failed", "message": "BPS 返回 response.failed；未完成响应"}
	}
	if status == "incomplete" {
		delete(result, "error")
	}
	if status == "completed" {
		for i, v := range result["output"].([]any) {
			item := v.(object)
			if nativeTool(item) {
				if err := r.emitTool(item, i); err != nil {
					return err
				}
			} else if r.seen[i] == nil {
				if err := r.emitItem(item, i); err != nil {
					return err
				}
			}
		}
	}
	terminal := clone(event)
	terminal["response"] = result
	if err := r.send(terminal); err != nil {
		return err
	}
	r.response, r.terminal = result, name
	return nil
}
func (r *relay) emitTool(item object, index int) error {
	key, prefix := "arguments", "response.function_call_arguments"
	if str(item, "type") == "custom_tool_call" {
		key, prefix = "input", "response.custom_tool_call_input"
	}
	start := clone(item)
	start["status"], start[key] = "in_progress", ""
	for _, event := range []object{
		{"type": "response.output_item.added", "output_index": index, "item": start},
		{"type": prefix + ".delta", "output_index": index, "item_id": item["id"], "delta": item[key]},
		{"type": prefix + ".done", "output_index": index, "item_id": item["id"], key: item[key]},
		{"type": "response.output_item.done", "output_index": index, "item": item},
	} {
		if err := r.send(event); err != nil {
			return err
		}
	}
	return nil
}
func (r *relay) emitItem(item object, index int) error {
	start := clone(item)
	start["status"] = "in_progress"
	if str(item, "type") == "message" {
		start["content"] = []any{}
	}
	if err := r.send(object{"type": "response.output_item.added", "output_index": index, "item": start}); err != nil {
		return err
	}
	if content, ok := item["content"].([]any); ok {
		for j, v := range content {
			part, ok := v.(object)
			if !ok {
				return errors.New("BPS content 无效")
			}
			if str(part, "type") != "output_text" && str(part, "type") != "refusal" {
				// Unfamiliar parts remain in output_item.done and the terminal
				// response. Do not invent per-part deltas for JSON fallback.
				continue
			}
			key, prefix := "text", "response.output_text"
			if str(part, "type") == "refusal" {
				key, prefix = "refusal", "response.refusal"
			}
			empty := clone(part)
			empty[key] = ""
			for _, event := range []object{
				{"type": "response.content_part.added", "output_index": index, "item_id": item["id"], "content_index": j, "part": empty},
				{"type": prefix + ".delta", "output_index": index, "item_id": item["id"], "content_index": j, "delta": part[key]},
				{"type": prefix + ".done", "output_index": index, "item_id": item["id"], "content_index": j, key: part[key]},
				{"type": "response.content_part.done", "output_index": index, "item_id": item["id"], "content_index": j, "part": part},
			} {
				if err := r.send(event); err != nil {
					return err
				}
			}
		}
	}
	return r.send(object{"type": "response.output_item.done", "output_index": index, "item": item})
}

func (r *relay) consume(resp *http.Response) error {
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType == "application/json" {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		if err != nil {
			return errors.New(networkError(r.ctx, err))
		}
		if len(raw) > maxResponse {
			return errors.New("BPS 响应超过 32 MiB")
		}
		body, err := decodeObject(raw)
		if err != nil {
			return err
		}
		if r.observe != nil {
			r.observe(object{"type": "response." + str(body, "status"), "response": body})
		}
		status := str(body, "status")
		if status != "completed" && status != "failed" && status != "incomplete" {
			return errors.New("BPS JSON 响应没有可确认的终态")
		}
		initial := clone(body)
		initial["output"], initial["status"] = []any{}, "in_progress"
		delete(initial, "usage")
		delete(initial, "error")
		if err := r.send(object{"type": "response.created", "response": initial}); err != nil {
			return err
		}
		return r.finish(object{"type": "response." + status}, body)
	}
	if contentType != "text/event-stream" {
		return errors.New("BPS 返回了非 JSON/SSE 响应")
	}
	return readSSE(resp.Body, r.event)
}

// Bounds apply both per event and to the whole response. Returning immediately
// on a verified terminal avoids treating a later socket error as a failed call.
func readSSE(reader io.Reader, callback func(object) (bool, error)) error {
	scanner := bufio.NewScanner(io.LimitReader(reader, maxResponse+1))
	scanner.Buffer(make([]byte, 32<<10), 4<<20)
	var data strings.Builder
	name := ""
	total := 0
	flush := func() (bool, error) {
		if data.Len() == 0 {
			name = ""
			return false, nil
		}
		raw := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if raw == "[DONE]" {
			return false, errors.New("BPS 在完整终态前发送 [DONE]")
		}
		body, err := decodeObject([]byte(raw))
		if err != nil {
			return false, errors.New("BPS SSE 包含无效 JSON")
		}
		if name != "" && str(body, "type") != "" && name != str(body, "type") {
			return false, errors.New("SSE event 名与 JSON type 不一致")
		}
		if str(body, "type") == "" {
			body["type"] = name
		}
		name = ""
		return callback(body)
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > maxResponse {
			return errors.New("BPS 响应超过 32 MiB")
		}
		if line == "" {
			done, err := flush()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
			if data.Len() > 4<<20 {
				return errors.New("单条 BPS SSE 事件超过 4 MiB")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("BPS SSE 读取中断或单行过大")
	}
	if data.Len() > 0 {
		done, err := flush()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("BPS SSE 提前结束，未收到 completed/failed/incomplete 终态")
}
