package basispoints

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFinishedItemsRecoverWithoutInventingUsage(t *testing.T) {
	created := eventBytes(object{"type": "response.created", "response": object{"id": "resp_fixture", "status": "in_progress", "output": []any{}}})
	for _, tool := range []bool{false, true} {
		item := terminalText("done")
		if tool {
			item = nativeItem("exec_command", object{"cmd": "pwd"})
		}
		raw := created + eventBytes(object{"type": "response.output_item.done", "output_index": 0, "item": item})
		for _, reader := range []io.Reader{strings.NewReader(raw), afterTerminalError{strings.NewReader(strings.TrimRight(raw, "\n"))}} {
			r := newRelay(context.Background(), mustPlan(t, fixtureServer(), fixtureRequest(), 7), nil)
			if err := r.consume(upstreamResponse("text/event-stream", reader)); err != nil || !r.recovered || r.terminal != "response.completed" || r.response["usage"] != nil {
				t.Fatal("finished items could not recover safely", err, r.response)
			}
		}
	}
}

func TestUnfinishedOrAmbiguousStreamsCannotRecover(t *testing.T) {
	created := eventBytes(object{"type": "response.created", "response": object{"id": "resp_fixture", "status": "in_progress", "output": []any{}}})
	done := eventBytes(object{"type": "response.output_item.done", "output_index": 0, "item": terminalText("done")})
	commentary := terminalText("I will read the project")
	commentary["phase"] = "commentary"
	for _, raw := range []string{
		created + eventBytes(object{"type": "response.output_item.done", "output_index": 0, "item": commentary}),
		created + done + eventBytes(object{"type": "response.output_text.delta", "output_index": 1, "delta": "unfinished"}),
		created + done + eventBytes(object{"type": "response.output_text.delta", "delta": "no index"}),
		created + done + done,
		created + done + "data: {\"type\":",
		created + eventBytes(object{"type": "response.output_item.done", "output_index": 0, "item": object{"type": "reasoning", "id": "rs_1", "status": "completed"}}),
		done,
	} {
		r := newRelay(context.Background(), mustPlan(t, fixtureServer(), fixtureRequest(), 7), nil)
		if err := r.consume(upstreamResponse("text/event-stream", strings.NewReader(raw))); err == nil || r.recovered || r.terminal != "" {
			t.Fatal("unfinished stream completed", err)
		}
	}
}

func TestSkippedCallsReindexStreamAndPreserveEventOrder(t *testing.T) {
	bad := uniqueNative(1, "undeclared", object{})
	good := uniqueNative(2, "exec_command", object{"cmd": "pwd"})
	text := terminalText("next")
	raw := eventBytes(object{"type": "response.created", "response": object{"id": "resp_fixture", "status": "in_progress", "output": []any{}}})
	for index, item := range []object{bad, good, text} {
		raw += eventBytes(object{"type": "response.output_item.added", "output_index": index, "item": item})
		raw += eventBytes(object{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	raw += eventBytes(object{"type": "response.completed", "response": completed(bad, good, text)})
	var out bytes.Buffer
	r := newRelay(context.Background(), mustPlan(t, fixtureServer(), fixtureRequest(), 7), func(data []byte) error { _, err := out.Write(data); return err })
	if err := r.consume(upstreamResponse("text/event-stream", strings.NewReader(raw))); err != nil {
		t.Fatal(err)
	}
	var added []string
	if err := readSSE(bytes.NewReader(out.Bytes()), func(event object) (bool, error) {
		if str(event, "type") == "response.output_item.added" {
			index, err := indexOf(event)
			if err != nil || index != len(added) {
				t.Error("stream indexes out of order", index, err)
			}
			added = append(added, str(event["item"].(object), "type"))
		}
		return str(event, "type") == "response.completed", nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(added, ",") != "function_call,message" || strings.Contains(out.String(), "undeclared") || strings.Contains(out.String(), "run_officejs") {
		t.Fatal(out.String())
	}
}

func TestKeepaliveStopsOnCancellationAndDoesNotStartRequestAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	go func() {
		_, _ = io.WriteString(writer, eventBytes(object{"type": "response.created", "response": object{"id": "resp_fixture", "status": "in_progress", "output": []any{}}}))
	}()
	r := newRelay(ctx, mustPlan(t, fixtureServer(), fixtureRequest(), 7), func(raw []byte) error {
		if strings.Contains(string(raw), "event: response.in_progress") {
			cancel()
		}
		return nil
	})
	response := upstreamResponse("text/event-stream", reader)
	response.Body = reader // Closing the response must release the pending read.
	if err := r.consumeStreamEvery(response, time.Millisecond); !errors.Is(err, context.Canceled) || r.keepalives != 1 || r.terminal != "" {
		t.Fatal("keepalive did not stop with request", err, r.keepalives)
	}
	if _, err := writer.Write([]byte("after cancellation")); err == nil {
		t.Fatal("stream reader left open")
	}
}

func TestTerminalBlockWithoutBlankLineWinsOverTrailingDisconnect(t *testing.T) {
	raw := strings.TrimRight(eventBytes(object{"type": "response.completed", "response": completed(terminalText("done"))}), "\n")
	r := newRelay(context.Background(), mustPlan(t, fixtureServer(), fixtureRequest(), 7), nil)
	if err := r.consume(upstreamResponse("text/event-stream", afterTerminalError{strings.NewReader(raw)})); err != nil || r.recovered || r.terminal != "response.completed" {
		t.Fatal("complete terminal was lost", err)
	}
}
