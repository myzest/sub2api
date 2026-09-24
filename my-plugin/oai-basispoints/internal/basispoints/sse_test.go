package basispoints

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func eventBytes(event object) string {
	return "event: " + str(event, "type") + "\ndata: " + string(encoded(event)) + "\n\n"
}
func upstreamResponse(kind string, body io.Reader) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{kind}}, Body: io.NopCloser(body)}
}
func nativeStream(native object) string {
	start := clone(native)
	start["status"], start["arguments"] = "in_progress", ""
	return eventBytes(object{"type": "response.created", "response": object{"id": "resp_fixture", "status": "in_progress", "output": []any{}}}) +
		eventBytes(object{"type": "response.output_item.added", "output_index": 0, "item": start}) +
		eventBytes(object{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": native["id"], "delta": native["arguments"]}) +
		eventBytes(object{"type": "response.output_item.done", "output_index": 0, "item": native})
}

type byteReader struct{ raw []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.raw) == 0 {
		return 0, io.EOF
	}
	p[0] = r.raw[0]
	r.raw = r.raw[1:]
	return 1, nil
}

type afterTerminalError struct{ raw *strings.Reader }

func (r afterTerminalError) Read(p []byte) (int, error) {
	n, err := r.raw.Read(p)
	if err == io.EOF {
		return 0, errors.New("socket reset after completion")
	}
	return n, err
}

func TestSSEHoldsToolThenEmitsValidatedCallAtOriginalIndex(t *testing.T) {
	s := fixtureServer()
	p := mustPlan(t, s, fixtureRequest(), 7)
	native := nativeItem("exec_command", object{"cmd": "pwd"})
	var emitted bytes.Buffer
	r := newRelay(context.Background(), p, func(b []byte) error { _, err := emitted.Write(b); return err })
	_, err := readSSEUntilEOF(nativeStream(native), r)
	if err == nil {
		t.Fatal("missing terminal accepted")
	}
	if strings.Contains(emitted.String(), "run_officejs") || strings.Contains(emitted.String(), "cmd") {
		t.Fatal("native tool leaked before completion", emitted.String())
	}
	raw := nativeStream(native) + eventBytes(object{"type": "response.completed", "response": completed(native)}) + "data: [DONE]\n\n"
	emitted.Reset()
	r = newRelay(context.Background(), p, func(b []byte) error { _, err := emitted.Write(b); return err })
	if err := r.consume(upstreamResponse("text/event-stream", &byteReader{[]byte(strings.ReplaceAll(raw, "\n", "\r\n"))})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(emitted.String(), "run_officejs") || strings.Contains(emitted.String(), "opaque_server_state") || !strings.Contains(emitted.String(), "exec_command") {
		t.Fatal("bad converted stream", emitted.String())
	}
	var events []object
	if err := readSSE(bytes.NewReader(emitted.Bytes()), func(e object) (bool, error) {
		events = append(events, e)
		return str(e, "type") == "response.completed", nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatal("unexpected tool event sequence", len(events))
	}
	for i, e := range events {
		if string(encoded(e["sequence_number"])) != string(encoded(i)) {
			t.Fatal("non-monotonic sequence numbers", events)
		}
	}
	call := events[4]["item"].(object)
	terminalCall := r.response["output"].([]any)[0].(object)
	if digest(call) != digest(terminalCall) || digest(r.response["usage"]) != digest(completed()["usage"]) {
		t.Fatal("terminal tool or usage mismatch")
	}
}
func readSSEUntilEOF(s string, r *relay) (bool, error) {
	err := readSSE(strings.NewReader(s), r.event)
	return r.response != nil, err
}

func TestSSETextIsEmittedBeforeTerminal(t *testing.T) {
	source := fixtureRequest()
	source["tools"] = []any{}
	p := mustPlan(t, fixtureServer(), source, 7)
	var output bytes.Buffer
	r := newRelay(context.Background(), p, func(b []byte) error { output.Write(b); return nil })
	added := terminalText("")
	added["status"] = "in_progress"
	added["content"] = []any{}
	data := eventBytes(object{"type": "response.output_item.added", "output_index": 0, "item": added}) + eventBytes(object{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg_1", "content_index": 0, "delta": "Hello"})
	if err := readSSE(strings.NewReader(data), r.event); err == nil {
		t.Fatal("early EOF passed")
	}
	if !strings.Contains(output.String(), "Hello") || strings.Contains(output.String(), "response.completed") {
		t.Fatal("text was buffered or completion invented")
	}
}

func TestStreamFailuresIncompleteAndUnknownToolsNeverComplete(t *testing.T) {
	p := mustPlan(t, fixtureServer(), fixtureRequest(), 7)
	for _, name := range []string{"failed", "incomplete"} {
		native := nativeItem("exec_command", object{"cmd": "pwd"})
		result := completed(native)
		result["status"] = name
		result["error"] = object{"message": "secret-token raw user input"}
		var out bytes.Buffer
		r := newRelay(context.Background(), p, func(b []byte) error { out.Write(b); return nil })
		raw := nativeStream(native) + eventBytes(object{"type": "response." + name, "response": result})
		if err := r.consume(upstreamResponse("text/event-stream", strings.NewReader(raw))); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "run_officejs") || strings.Contains(out.String(), "response.completed") || strings.Contains(out.String(), "secret-token") {
			t.Fatal("failed stream leaked or completed", out.String())
		}
	}
	for _, bad := range []string{"data: [DONE]\n\n", "data: {\n\n", "event: response.completed\ndata: {\"type\":\"response.failed\"}\n\n", eventBytes(object{"type": "response.completed", "response": object{"status": "in_progress", "output": []any{}}})} {
		r := newRelay(context.Background(), p, nil)
		if err := r.consume(upstreamResponse("text/event-stream", strings.NewReader(bad))); err == nil {
			t.Fatal("invalid terminal accepted", bad)
		}
	}
	bad := nativeItem("unknown", object{})
	var out bytes.Buffer
	r := newRelay(context.Background(), p, func(b []byte) error { out.Write(b); return nil })
	if err := r.consume(upstreamResponse("text/event-stream", strings.NewReader(nativeStream(bad)+eventBytes(object{"type": "response.completed", "response": completed(bad)})))); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if strings.Contains(out.String(), "unknown") || strings.Contains(out.String(), "response.completed") {
		t.Fatal("unknown tool released")
	}
}

func TestJSONFallbackAndPostTerminalSocketError(t *testing.T) {
	source := fixtureRequest()
	source["tools"] = []any{}
	p := mustPlan(t, fixtureServer(), source, 7)
	var output bytes.Buffer
	r := newRelay(context.Background(), p, func(b []byte) error { output.Write(b); return nil })
	res := completed(terminalText("BPS_OK"))
	if err := r.consume(upstreamResponse("application/json", bytes.NewReader(encoded(res)))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "response.output_text.delta") || r.terminal != "response.completed" {
		t.Fatal("JSON not translated to SSE")
	}
	raw := eventBytes(object{"type": "response.completed", "response": res})
	r = newRelay(context.Background(), p, nil)
	if err := r.consume(upstreamResponse("text/event-stream", afterTerminalError{strings.NewReader(raw)})); err != nil {
		t.Fatal("post-terminal disconnect invalidated result", err)
	}
}

func TestSSERejectsMismatchedNativeFinal(t *testing.T) {
	p := mustPlan(t, fixtureServer(), fixtureRequest(), 7)
	native := nativeItem("exec_command", object{"cmd": "pwd"})
	different := nativeItem("exec_command", object{"cmd": "ls"})
	r := newRelay(context.Background(), p, nil)
	if err := r.consume(upstreamResponse("text/event-stream", strings.NewReader(nativeStream(native)+eventBytes(object{"type": "response.completed", "response": completed(different)})))); err == nil {
		t.Fatal("mismatched completed call accepted")
	}
}
