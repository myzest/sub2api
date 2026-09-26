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

// Fixture-only forwarding checks, not proof that BPS accepts arbitrary ciphertext.
func TestAgentEncryptedInputPreservedWithoutLocalRejection(t *testing.T) {
	for _, value := range []any{"plain-looking old task", "gAAAAABopaqueFixture", "", nil} {
		source := fixtureRequest()
		source["input"] = []any{object{"type": "agent_message", "author": "/root/worker", "recipient": "/root", "content": []any{
			object{"type": "input_text", "text": "status prefix"}, object{"type": "encrypted_content", "encrypted_content": value, "extension": object{"unchanged": true}},
		}}}
		original := digest(source)
		p := mustPlan(t, fixtureServer(), source, 7)
		items := p.body["input"].([]any)
		if digest(items[len(items)-1:]) != digest(source["input"]) || digest(source) != original {
			t.Fatal("agent encrypted content was changed, dropped or decoded")
		}
	}
}

func TestAgentHistoryForwardedAndUpstreamErrorsRetained(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, rejected := range []bool{false, true} {
			var requests atomic.Int32
			received := make(chan object, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				body, err := decodeObject(data)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				select {
				case received <- body:
				default:
					t.Error("unexpected extra upstream request")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if rejected {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, eventBytes(object{"type": "error", "error": object{"code": "invalid_encrypted_content", "message": "Encrypted function output content could not be decrypted or decoded.", "type": "invalid_request_error"}}))
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(encoded(completed(terminalText("fixture response"))))
				}
			}))
			s := fixtureServer()
			routeFixture(s, up.URL+"/responses")
			source := fixtureRequest()
			input := []any{message("user", "fixture task")}
			for i := 0; i < 25; i++ {
				part := object{"type": "input_text", "text": "plaintext fixture"}
				if i < 16 {
					part = object{"type": "encrypted_content", "encrypted_content": "opaque fixture \r\n\t你好🙂"}
				}
				input = append(input, object{"type": "agent_message", "author": "/root/worker", "recipient": "/root", "content": []any{object{"type": "input_text", "text": "prefix"}, part}})
			}
			source["input"], source["stream"] = input, stream
			body := encoded(source)
			frames := forward(t, transportClient(t, s), bpsStart(body), body)
			up.Close()
			d := s.lastRequest
			if requests.Load() != 1 || d.ResponseAttempts != 1 || !d.ResponsesStarted || d.HTTPStatus != 200 || d.AgentInput == nil || d.AgentInput.Messages != 25 || d.AgentInput.EncryptedParts != 16 || d.AgentInput.Handling != "preserved" {
				t.Fatal("input was locally rejected or resent", d)
			}
			select {
			case actual := <-received:
				items := actual["input"].([]any)
				if digest(items[len(items)-len(input):]) != digest(input) {
					t.Fatal("history fields or order changed")
				}
			default:
				t.Fatal("no upstream request captured")
			}
			if d.ErrorSource == "plugin_agent_message" || frames[len(frames)-1].GetEnd() == nil {
				t.Fatal("legacy local rejection or incomplete delivery")
			}
			for _, frame := range frames {
				if frame.GetError() != nil {
					t.Fatal("transport error instead of explicit upstream result")
				}
			}
			if rejected {
				if d.ErrorSource != "upstream_stream" || d.Terminal != "error" || !strings.Contains(d.UpstreamError, "invalid_encrypted_content") {
					t.Fatal("upstream rejection hidden", d)
				}
			} else if d.Error != "" || d.Terminal != "response.completed" {
				t.Fatal("fake upstream success misclassified", d)
			}
			if strings.Contains(string(encoded(d.AgentInput)), "opaque fixture") {
				t.Fatal("diagnostic copied ciphertext")
			}
		}
	}
}

func TestEncryptedAgentHistoryCoexistsWithCatalogAndToolReplay(t *testing.T) {
	for _, mode := range []string{"native_hit", "native_missing", "external"} {
		t.Run(mode, func(t *testing.T) {
			s := fixtureServer()
			source := agentSourceFixture("spawn_agent")
			root := message("user", "hello")
			carrier := object{"type": "additional_tools", "role": "developer", "tools": source["tools"]}
			delete(source, "tools")
			source["input"] = []any{root, carrier}
			seed := mustPlan(t, s, source, 7)
			args := object{"message": "already delivered fixture"}
			native := nativeItem("collaboration.spawn_agent", args)
			response, err := transformResponse(context.Background(), completed(native), seed.tools, seed.store, true)
			if err != nil {
				t.Fatal(err)
			}
			call := responseToolCalls(response)[0]
			if mode != "native_hit" {
				s.host = newFakeHost()
			}
			if mode == "external" {
				call = clone(call)
				call["id"], call["call_id"] = "fc_legacy", "legacy"
			}
			agent := func(text string) object {
				return object{"type": "agent_message", "author": "/root/worker", "recipient": "/root", "content": []any{object{"type": "encrypted_content", "encrypted_content": text}}}
			}
			before, after := agent("before fixture"), agent("after fixture")
			source["input"] = []any{root, carrier, before, call, object{"type": "function_call_output", "call_id": call["call_id"], "output": "already executed"}, after}
			original := digest(source)
			writes := s.host.(*fakeHost).kvWrites
			d := &replayDiagnostic{}
			ctx := context.WithValue(context.Background(), replayDiagnosticContextKey{}, d)
			plan, err := s.prepare(ctx, encoded(source), 7, nil, s.config())
			if err != nil {
				t.Fatal(err)
			}
			items := plan.body["input"].([]any)
			if len(items) != 7 || digest(items[3]) != digest(before) || digest(items[6]) != digest(after) || digest(source) != original {
				t.Fatal("mixed replay changed opaque contents, positions or source history")
			}
			replayed := items[4].(object)
			if str(items[2].(object), "role") != "developer" || items[5].(object)["call_id"] != replayed["call_id"] || s.host.(*fakeHost).kvWrites != writes {
				t.Fatal("catalog or replay pairing changed, or prepare wrote new tool state")
			}
			switch mode {
			case "native_hit":
				if d.NativeHits != 1 || digest(replayed) != digest(native) {
					t.Fatal("native record not preserved", d)
				}
			case "native_missing":
				if d.Missing != 1 || d.Rebuilt != 1 {
					t.Fatal("missing history was not rebuilt", d)
				}
			case "external":
				if d.Imported != 1 || d.Missing != 0 {
					t.Fatal("external history was not imported", d)
				}
			}
			if mode != "native_hit" {
				inner, _, err := decodeRelayEnvelope(replayed)
				if err != nil || digest(inner["arguments"]) != digest(args) {
					t.Fatal("rebuilt history changed the delivered arguments", err)
				}
			}
		})
	}
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
