package basispoints

// The host reference treats error as a terminal event, not a broken pipe
// (ranxi2001/sub2api f671a8d, basispoints/stream.go). Keep the original error
// shape and bounded protocol fields. Error strings stay raw at the user's
// request; never copy the entire response, input, ctx or request headers.
func upstreamFailureFields(event object) (object, string) {
	value := event
	if response, ok := event["response"].(object); ok {
		value = response
	}
	if nested, ok := value["error"].(object); ok {
		value = nested
	}
	selected := object{}
	for _, key := range []string{"message", "code", "param", "detail", "type", "error"} {
		if v, ok := value[key]; ok {
			selected[key] = v
		}
	}
	if len(encoded(selected)) > maxErrorDiagnosticBytes {
		return nil, "too_large"
	}
	fields := diagnosticObject(selected, func(_ string, text string) string { return text })
	if kind := str(selected, "type"); kind != "" && kind != "error" && kind != "response.error" && kind != "response.failed" {
		if fields == nil {
			fields = object{}
		}
		fields["type"] = kind
	}
	if len(fields) == 0 {
		return nil, "no_fields"
	}
	if len(encoded(fields)) > maxErrorDiagnosticBytes {
		return nil, "too_large"
	}
	return fields, "captured_raw"
}

func upstreamClientError(fields object) object {
	err := object{"code": "bps_upstream_error", "type": "upstream_error", "message": "BPS 返回上游错误；未完成响应"}
	for _, key := range []string{"code", "type", "message", "param"} {
		if value := str(fields, key); value != "" {
			if key == "type" && (value == "error" || value == "response.error" || value == "response.failed") {
				continue // Event type is not an upstream semantic error type.
			}
			err[key] = value
		}
	}
	return err
}

func (r *relay) clientError(event object) object {
	if r.projectError != nil {
		return r.projectError(event)
	}
	fields, _ := upstreamFailureFields(event)
	return upstreamClientError(fields)
}

// A bare error event does not prove a completed response. Forward its error
// fields, drop held tools and stop; do not invent response IDs, usage or success.
func (r *relay) finishStreamError(event object) error {
	err := r.clientError(event)
	terminal := object{"type": "error"}
	if event["error"] != nil || event["response"] != nil {
		terminal["error"] = err
	} else {
		for _, key := range []string{"code", "message", "param"} {
			if value, ok := err[key]; ok {
				terminal[key] = value
			}
		}
	}
	r.pending, r.holdFrom = nil, -1
	for _, item := range r.seen {
		if nativeTool(item) {
			r.plan.tools.skippedTools++
		}
	}
	if sendErr := r.send(terminal); sendErr != nil {
		return sendErr
	}
	r.response, r.terminal = object{"error": err}, "error"
	return nil
}
