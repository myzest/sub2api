package basispoints

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

var errSSEWithoutTerminal = errors.New("BPS SSE 提前结束，未收到 completed/failed/incomplete/error 终态")

type sseReadError struct {
	cause   error
	partial bool
}

func (e *sseReadError) Error() string { return "BPS 响应读取中断" }
func (e *sseReadError) Unwrap() error { return e.cause }

const streamKeepaliveInterval = 15 * time.Second

// Excel bridge excel_stream._with_ticks and completion_from_finished_items.
// All state and writes remain on one goroutine; only the blocking reader runs
// in the background, and closing/canceling the request releases that reader.
func (r *relay) consumeStream(response *http.Response) error {
	return r.consumeStreamEvery(response, streamKeepaliveInterval)
}

func (r *relay) consumeStreamEvery(response *http.Response, interval time.Duration) error {
	type readResult struct {
		event object
		err   error
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	defer response.Body.Close()
	events := make(chan readResult, 1)
	go func() {
		err := readSSE(response.Body, func(e object) (bool, error) {
			select {
			case events <- readResult{event: e}:
				switch str(e, "type") {
				case "response.completed", "response.failed", "response.incomplete", "error", "response.error":
					return true, nil
				}
				return false, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		})
		select {
		case events <- readResult{err: err}:
		case <-ctx.Done():
		}
	}()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if str(r.started, "id") != "" && r.emit != nil {
				progress := clone(r.started)
				progress["status"], progress["output"] = "in_progress", []any{}
				if err := r.send(object{"type": "response.in_progress", "response": progress}); err != nil {
					return err
				}
				r.keepalives++
			}
			timer.Reset(interval)
		case next := <-events:
			timer.Reset(interval)
			if next.event != nil {
				done, err := r.event(next.event)
				if err != nil || done {
					return err
				}
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var readErr *sseReadError
			if errors.Is(next.err, errSSEWithoutTerminal) || (errors.As(next.err, &readErr) && !readErr.partial) {
				if response := r.completionFromFinishedItems(); response != nil {
					_, err := r.event(object{"type": "response.completed", "response": response})
					r.recovered = err == nil
					return err
				}
			}
			if next.err == nil {
				return errSSEWithoutTerminal
			}
			return next.err
		}
	}
}

// The reference retries non-streaming RemoteProtocolError once. Retry only
// the corresponding explicit Go HTTP framing/EOF errors, never JSON/schema
// rejection, context cancellation, ordinary timeout, or a streaming response.
func protocolInterruption(err error) bool {
	var stream http2.StreamError
	var connection http2.ConnectionError
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &stream) || errors.As(err, &connection)
}

func (r *relay) completionFromFinishedItems() object {
	if r.started == nil || str(r.started, "id") == "" || len(r.done) == 0 || len(r.done) != len(r.seen) || len(r.unfinished) > 0 || r.unindexed {
		return nil
	}
	output := make([]any, len(r.done))
	for index := range output {
		item := r.done[index]
		if item == nil || (str(item, "status") != "" && str(item, "status") != "completed") {
			return nil
		}
		output[index] = item
	}
	last := output[len(output)-1].(object)
	if str(last, "type") == "message" {
		if str(last, "phase") == "commentary" {
			return nil
		}
	} else if !nativeTool(last) {
		return nil // Reasoning alone does not establish a completed answer.
	}
	response := clone(r.started)
	response["status"], response["output"] = "completed", output
	delete(response, "error")
	delete(response, "incomplete_details")
	delete(response, "usage") // No terminal usage was observed; do not invent it.
	return response
}
