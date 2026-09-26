package basispoints

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Fixtures only; never execute a collaboration tool or use an actual account.
func agentSourceFixture(name string) object {
	source := fixtureRequest()
	source["tools"] = []any{object{"type": "namespace", "name": "collaboration", "tools": []any{
		object{"type": "function", "name": name, "parameters": object{"type": "object", "required": []any{"message"}, "properties": object{
			"message": object{"type": "string", "encrypted": true},
		}}},
	}}}
	return source
}

func requirePlaintextAgentArgs(t *testing.T, call object) {
	t.Helper()
	if string(encoded(call["encrypted_function_args"])) != "[]" {
		t.Fatal("missing/null is not an explicit empty encryption list", call["encrypted_function_args"])
	}
}

func TestAgentPlaintextMetadataSurvivesStreamingAndReplay(t *testing.T) {
	const task = "核对源码🙂\r\nKeep \"quotes\", tabs\tand C:\\work\\file unchanged."
	for _, name := range []string{"spawn_agent", "send_message", "followup_task"} {
		for _, mode := range []string{"relay", "direct"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				source := agentSourceFixture(name)
				plan := mustPlan(t, fixtureServer(), source, 7)
				args := object{"message": task}
				if name == "spawn_agent" {
					args["task_name"], args["fork_turns"] = "worker", "none"
				} else {
					args["target"] = "worker"
				}
				native := nativeItem("collaboration."+name, args)
				if mode == "direct" {
					native["name"], native["arguments"] = "collaboration."+name, string(encoded(args))
				}
				wire := nativeStream(native) + eventBytes(object{"type": "response.completed", "response": completed(native)})
				var out bytes.Buffer
				r := newRelay(context.Background(), plan, func(data []byte) error { _, err := out.Write(data); return err })
				if err := r.consume(&http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}); err != nil {
					t.Fatal(err)
				}
				var client object
				marked := 0
				delta := ""
				err := readSSE(bytes.NewReader(out.Bytes()), func(e object) (bool, error) {
					switch str(e, "type") {
					case "response.output_item.added", "response.output_item.done":
						client = e["item"].(object)
						requirePlaintextAgentArgs(t, client)
						marked++
					case "response.function_call_arguments.delta":
						delta += str(e, "delta")
					case "response.completed":
						requirePlaintextAgentArgs(t, responseToolCalls(e["response"].(object))[0])
						marked++
						return true, nil
					}
					return false, nil
				})
				if err != nil || marked != 3 || client == nil || delta != str(client, "arguments") || str(client, "namespace") != "collaboration" || str(client, "name") != name {
					t.Fatal("streamed metadata or tool identity changed", err, marked)
				}
				decoded, err := decodeObject([]byte(str(client, "arguments")))
				if err != nil || digest(decoded) != digest(args) {
					t.Fatal("task text or arguments changed", err)
				}
				d := &requestDiagnostic{}
				d.output(r.response)
				if len(d.OutputTools) != 1 || d.OutputTools[0].ArgumentEncryption != "plaintext" {
					t.Fatal("parent encryption state not visible")
				}
				// Client child-message construction is a simulated fixture, not a
				// claim that an installed Codex client was invoked or verified.
				child := fixtureRequest()
				child["prompt_cache_key"] = "independent-child"
				childMessage := object{"type": "agent_message", "author": "/root", "recipient": "/root/worker", "content": []any{object{"type": "input_text", "text": decoded["message"]}}}
				child["input"] = []any{childMessage}
				childPlan := mustPlan(t, fixtureServer(), child, 7)
				items := childPlan.body["input"].([]any)
				if digest(items[len(items)-1]) != digest(childMessage) {
					t.Fatal("child task changed")
				}
				result := object{"type": "function_call_output", "call_id": client["call_id"], "output": "delivered"}
				restored, err := plan.store.restore(context.Background(), []any{client, result})
				if err != nil || digest(restored[0]) != digest(native) {
					t.Fatal("native replay changed", err)
				}
				plan.store.host = newFakeHost()
				restored, err = plan.store.restore(context.Background(), []any{client, result})
				if err != nil {
					t.Fatal(err)
				}
				inner, _, err := decodeRelayEnvelope(restored[0].(object))
				if err != nil || digest(inner["arguments"]) != digest(args) {
					t.Fatal("cache-miss history changed task", err)
				}
			})
		}
	}
}

func TestAgentDirectMetadataPreservedButWrapperMetadataNotCopied(t *testing.T) {
	for _, metadata := range []any{nil, []any{}, []any{"message"}} {
		plan := mustPlan(t, fixtureServer(), agentSourceFixture("spawn_agent"), 7)
		native := nativeItem("collaboration.spawn_agent", object{"message": "opaque-fixture"})
		native["name"], native["arguments"], native["encrypted_function_args"] = "collaboration.spawn_agent", string(encoded(object{"message": "opaque-fixture"})), metadata
		before := digest(native)
		client, err := plan.tools.convert(native)
		if err != nil {
			t.Fatal(err)
		}
		if metadata == nil {
			requirePlaintextAgentArgs(t, client)
		} else if digest(client["encrypted_function_args"]) != digest(metadata) {
			t.Fatal("direct encryption declaration changed")
		}
		if str(client, "arguments") != str(native, "arguments") || digest(native) != before {
			t.Fatal("direct arguments or native state changed")
		}
	}
	plan := mustPlan(t, fixtureServer(), agentSourceFixture("spawn_agent"), 7)
	native := nativeItem("collaboration.spawn_agent", object{"message": "plaintext fixture"})
	native["encrypted_function_args"] = []any{"code"}
	client, err := plan.tools.convert(native)
	if err != nil {
		t.Fatal(err)
	}
	requirePlaintextAgentArgs(t, client)
}

func TestAgentEncryptedInputFailsBeforeUpstreamWithoutChangingContents(t *testing.T) {
	for _, value := range []any{"plain-looking old task", "gAAAAABopaqueFixture", "", nil} {
		var requests atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(503) }))
		s := fixtureServer()
		routeFixture(s, up.URL+"/responses")
		source := fixtureRequest()
		source["input"] = []any{object{"type": "agent_message", "content": []any{object{"type": "encrypted_content", "encrypted_content": value}}}}
		body := encoded(source)
		frames := forward(t, transportClient(t, s), bpsStart(body), body)
		up.Close()
		d := s.lastRequest
		if requests.Load() != 0 || d.ResponseAttempts != 0 || d.ResponsesStarted || d.UpstreamStarted || d.ClientHTTPStatus != 400 || d.ErrorSource != "plugin_agent_message" || d.AgentInput == nil || d.AgentInput.EncryptedParts != 1 {
			t.Fatal("opaque agent message reached upstream or was misclassified")
		}
		result, err := decodeObject(bodyOf(frames))
		if err != nil || str(result["error"].(object), "code") != "bps_agent_encrypted_content" || frames[len(frames)-1].GetEnd() == nil {
			t.Fatal("missing actionable local rejection", err)
		}
		for _, frame := range frames {
			if frame.GetError() != nil {
				t.Fatal("local rejection broke transport")
			}
		}
		if digest(source) != digest(mustDecodeAgentFixture(t, body)) {
			t.Fatal("input changed")
		}
		if text, ok := value.(string); ok && text != "" && strings.Contains(string(bodyOf(frames)), text) {
			t.Fatal("error copied agent contents")
		}
	}
}

func mustDecodeAgentFixture(t *testing.T, body []byte) object {
	t.Helper()
	v, err := decodeObject(body)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAgentInspectionBoundedAndReasoningUnaffected(t *testing.T) {
	parts := []any{}
	for i := 0; i < 24; i++ {
		parts = append(parts, object{"type": "encrypted_content", "encrypted_content": "MUST_NOT_COPY"})
	}
	input := []any{object{"type": "agent_message", "content": parts}}
	d := inspectAgentInput(input)
	if d.Messages != 1 || d.EncryptedParts != 24 || len(d.EncryptedPaths) != toolTraceLimit || strings.Contains(string(encoded(d)), "MUST_NOT_COPY") {
		t.Fatal(d)
	}
	source := fixtureRequest()
	source["input"] = []any{object{"type": "reasoning", "summary": []any{}, "encrypted_content": "keep-reasoning"}, object{"type": "compaction_summary", "encrypted_content": "keep-compaction"}, object{"type": "agent_message", "content": []any{object{"type": "input_text", "text": "keep-task"}}}}
	p := mustPlan(t, fixtureServer(), source, 7)
	items := p.body["input"].([]any)
	if digest(items[len(items)-3:]) != digest(source["input"]) {
		t.Fatal("non-agent encrypted state or plaintext task changed")
	}
}
