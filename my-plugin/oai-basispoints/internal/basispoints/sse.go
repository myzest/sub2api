package basispoints

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

type eventWriter func([]byte) error
type relay struct {
	ctx        context.Context
	plan       *requestPlan
	emit       eventWriter
	sequence   int
	seen       map[int]object
	done       map[int]object
	response   object
	terminal   string
	observe    func(object) // Observer applies its own bounded diagnostic projection.
	started    object
	holdFrom   int
	pending    []object
	keepalives int
	recovered  bool
	unfinished map[int]bool
	unindexed  bool // Output activity without a reliable index cannot prove completion.
}

func newRelay(ctx context.Context, plan *requestPlan, emit eventWriter) *relay {
	return &relay{ctx: ctx, plan: plan, emit: emit, seen: map[int]object{}, done: map[int]object{}, holdFrom: -1, unfinished: map[int]bool{}}
}
func (r *relay) send(event object) error {
	if r.emit == nil {
		return nil
	}
	e := clone(event)
	if r.holdFrom >= 0 {
		if index, err := indexOf(e); err == nil && index >= r.holdFrom {
			r.pending = append(r.pending, e)
			return nil
		}
	}
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
	if index, err := indexOf(e); err == nil {
		if t == "response.output_item.done" {
			delete(r.unfinished, index)
		} else if t == "response.output_item.added" || strings.HasSuffix(t, ".delta") || t == "response.content_part.added" {
			r.unfinished[index] = true
		}
	} else if t == "response.output_item.added" || t == "response.output_item.done" || strings.HasSuffix(t, ".delta") || t == "response.content_part.added" {
		r.unindexed = true
	}
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
			if (t == "response.output_item.added" && r.seen[i] != nil) || (t == "response.output_item.done" && r.done[i] != nil) {
				r.unindexed = true
			}
			if old := r.seen[i]; old != nil && nativeTool(old) && !nativeTool(item) {
				return false, errors.New("BPS 原生工具 output_index 被其他项覆盖")
			}
			r.seen[i] = item
			if t == "response.output_item.done" {
				r.done[i] = item
			}
		}
		if nativeTool(item) {
			if r.holdFrom < 0 || i < r.holdFrom {
				r.holdFrom = i
			}
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
			if r.started == nil || t == "response.in_progress" {
				r.started = clone(res)
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
	// Skipping unconvertible or serial-only extra tools changes output indexes.
	// Reindex the held suffix before releasing its text/reasoning events.
	pending := map[int][]object{}
	r.holdFrom = -1
	for _, e := range r.pending {
		old, err := indexOf(e)
		if err != nil {
			return err
		}
		pending[old] = append(pending[old], e)
	}
	r.pending = nil
	if status == "failed" {
		result["error"] = object{"code": "bps_response_failed", "message": "BPS 返回 response.failed；未完成响应"}
	}
	if status == "incomplete" {
		delete(result, "error")
	}
	for i, v := range result["output"].([]any) {
		item := v.(object)
		old := r.plan.tools.outputIndices[i]
		if nativeTool(item) {
			if status == "completed" {
				if err := r.emitTool(item, i); err != nil {
					return err
				}
			}
			continue
		}
		// Release each retained item at its final index, after any preceding
		// tool. A skipped native call must not leave a gap in streamed output.
		for _, e := range pending[old] {
			e["output_index"] = i
			if err := r.send(e); err != nil {
				return err
			}
		}
		if status == "completed" && r.seen[old] == nil {
			if err := r.emitItem(item, i); err != nil {
				return err
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
			return &sseReadError{cause: err}
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
	return r.consumeStream(resp)
}

// Bounds apply both per event and to the whole response. Returning immediately
// on a verified terminal avoids treating a later socket error as a failed call.
func readSSE(reader io.Reader, callback func(object) (bool, error)) error {
	limited := &io.LimitedReader{R: reader, N: maxResponse + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 32<<10), 4<<20)
	var data strings.Builder
	name := ""
	flush := func() (bool, error) {
		if data.Len() == 0 {
			name = ""
			return false, nil
		}
		raw := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if raw == "[DONE]" {
			return false, errSSEWithoutTerminal
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
		if limited.N == 0 {
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
	if limited.N == 0 {
		return errors.New("BPS 响应超过 32 MiB")
	}
	// A complete last event remains usable when the socket fails before
	// its separating blank line (Excel sse.iter_sse_messages).
	if data.Len() > 0 {
		done, err := flush()
		if err != nil {
			if readErr := scanner.Err(); readErr != nil && !errors.Is(readErr, bufio.ErrTooLong) {
				return &sseReadError{cause: readErr, partial: true}
			}
			return err
		}
		if done {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errors.New("BPS SSE 单行超过限制")
		}
		return &sseReadError{cause: err}
	}
	return errSSEWithoutTerminal
}
