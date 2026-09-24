package basispoints

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

type fakeHost struct {
	pluginv1.HostServiceClient
	mu                                    sync.Mutex
	values                                map[string][]byte
	accountCalls, identityCalls, kvWrites int
	paused                                bool
}

func newFakeHost() *fakeHost { return &fakeHost{values: map[string][]byte{}} }
func (f *fakeHost) KVGet(_ context.Context, r *pluginv1.KVGetRequest, _ ...grpc.CallOption) (*pluginv1.KVGetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[r.Namespace+"/"+r.Key]
	return &pluginv1.KVGetResponse{Found: ok, Value: append([]byte(nil), v...)}, nil
}
func (f *fakeHost) KVSet(_ context.Context, r *pluginv1.KVSetRequest, _ ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[r.Namespace+"/"+r.Key] = append([]byte(nil), r.Value...)
	f.kvWrites++
	return &pluginv1.KVSetResponse{}, nil
}
func (f *fakeHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountCalls++
	return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 7, Name: "fixture OAuth", Platform: "openai", AccountType: "oauth", Status: "active", Schedulable: !f.paused}, {Id: 8, Name: "another OAuth", Platform: "openai", AccountType: "oauth", Status: "active", Schedulable: true}, {Id: 9, Platform: "openai", AccountType: "oauth", Status: "disabled"}}}, nil
}
func (f *fakeHost) ResolveOutboundIdentity(_ context.Context, r *pluginv1.ResolveOutboundIdentityRequest, _ ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identityCalls++
	return &pluginv1.ResolveOutboundIdentityResponse{Found: true, AccountId: r.AccountId, Platform: "openai", AccountType: "oauth", Token: "fixture-secret-token", Headers: headersToProto(http.Header{"Chatgpt-Account-Id": []string{"fixture-account"}})}, nil
}
func fixtureServer() *Server { s := New(); s.host = newFakeHost(); return s }
func fixtureRequest() object {
	return object{"model": "gpt-5.6-sol", "stream": true, "input": "hello", "prompt_cache_key": "session-one", "reasoning": object{"effort": "high"}, "tools": []any{object{"type": "function", "name": "exec_command", "parameters": object{"type": "object", "properties": object{"cmd": object{"type": "string"}}, "required": []any{"cmd"}, "additionalProperties": false}}}}
}
func mustPlan(t *testing.T, s *Server, source object, account int64) *requestPlan {
	t.Helper()
	p, err := s.prepare(context.Background(), encoded(source), account, nil, s.config())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func nativeItem(name string, payload any) object {
	return object{"id": "fc_native_1", "call_id": "call_native_1", "name": "run_officejs", "type": "function_call", "status": "completed", "arguments": string(encoded(object{"code": string(encoded(object{"name": name, "arguments": payload})), "summary": "fixture", "destructive": false})), "opaque_server_state": "preserve-this"}
}
func completed(output ...any) object {
	return object{"id": "resp_fixture", "model": "gpt-5.6-sol", "status": "completed", "output": output, "usage": object{"input_tokens": 13, "output_tokens": 5, "output_tokens_details": object{"reasoning_tokens": 3}}}
}
func terminalText(text string) object {
	return object{"type": "message", "id": "msg_1", "status": "completed", "role": "assistant", "content": []any{object{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func TestRequestConversionUsesReferenceWireFields(t *testing.T) {
	s := fixtureServer()
	source := fixtureRequest()
	source["max_output_tokens"] = json.Number("123")
	source["text"] = object{"verbosity": "high"}
	source["instructions"] = "original instructions"
	source["metadata"] = object{"task_id": "caller-task", "turn_id": "caller-turn", "agent_iteration": json.Number("3"), "extra": "keep", "nested": object{"unsupported": true}}
	p := mustPlan(t, s, source, 7)
	if p.body["model"] != source["model"] || p.body["model_selection"] != "explicit" || p.body["reasoning_effort"] != "high" || p.body["store"] != false || p.body["stream"] != true || p.body["tools"] != nil || p.body["max_output_tokens"] != nil || p.body["text"] != nil || p.body["prompt_cache_key"] != "session-one" {
		t.Fatal(p.body)
	}
	if got := p.body["metadata"].(object); got["task_id"] != "caller-task" || got["turn_id"] != "caller-turn" || got["agent_iteration"] != "3" || got["extra"] != "keep" || got["nested"] != nil {
		t.Fatal("scalar metadata was lost", got)
	}
	if digest(p.body["context_management"]) != digest([]any{object{"type": "compaction", "compact_threshold": 200000}}) {
		t.Fatal("missing reference compaction default")
	}
	input := p.body["input"].([]any)
	instructions := string(encoded(input[:2]))
	if !strings.Contains(instructions, "original instructions") || !strings.Contains(instructions, "additionalProperties") || !strings.Contains(instructions, "required") {
		t.Fatal("instructions or schema lost")
	}
	source["stream"] = false
	source["context_management"] = []any{}
	p = mustPlan(t, s, source, 7)
	if p.body["stream"] != false || len(p.body["context_management"].([]any)) != 0 {
		t.Fatal("caller stream/context settings were overwritten")
	}
	for _, tc := range []struct {
		value, want string
		ultra, fail bool
	}{{"max", "xhigh", false, false}, {"ultra", "ultra", true, false}, {"ultra", "", false, true}, {"invalid", "", false, true}} {
		source["reasoning"] = object{"effort": tc.value}
		cfg := s.config()
		cfg.AllowUltra = tc.ultra
		p, err := s.prepare(context.Background(), encoded(source), 7, nil, cfg)
		if (err != nil) != tc.fail || err == nil && p.body["reasoning_effort"] != tc.want {
			t.Fatalf("effort %s: %v %v", tc.value, p, err)
		}
	}
	for _, raw := range []string{`{"input":"x","model":"gpt-5.6-sol","previous_response_id":"resp_old"}`, `{"input":"x","model":"gpt-5.6-sol","reasoning_effort":"high","reasoning":{"effort":"low"}}`} {
		if _, err := s.prepare(context.Background(), []byte(raw), 7, nil, s.config()); err == nil {
			t.Fatal("accepted unsupported request", raw)
		}
	}
}

func TestReferenceInputPassthroughKeepsNestedFields(t *testing.T) {
	source := fixtureRequest()
	ordinary := []any{
		object{"type": "message", "role": "user", "content": []any{object{"type": "input_image", "image_url": "fixture-image", "detail": "original"}, object{"type": "input_file", "file_id": "fixture-file"}}},
		object{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{object{"type": "output_text", "text": "hello", "annotations": []any{object{"type": "fixture_annotation"}}}}},
		object{"type": "input_text", "text": "leave the item shape unchanged"},
		object{"type": "fixture_extension", "nested": object{"list": []any{true, json.Number("42")}}},
		object{"type": "compaction_summary", "id": "keep-id", "encrypted_content": "fixture-state", "extra": true},
		object{"type": "compaction_trigger", "extra": "keep"},
	}
	input := []any{nil, object{"type": "reasoning", "summary": []any{}}, object{"type": "item_reference", "id": "old-item"}, object{"type": "reasoning", "id": "discard-id", "encrypted_content": "fixture-reasoning"}}
	for _, value := range ordinary {
		item := clone(value.(object))
		item["internal_chat_message_metadata_passthrough"] = object{"turn_id": "volatile"}
		input = append(input, item)
	}
	source["input"] = input
	source["tools"] = append(source["tools"].([]any), object{"type": "web_search"})
	p := mustPlan(t, fixtureServer(), source, 7)
	items := p.body["input"].([]any)
	want := append([]any{object{"type": "reasoning", "summary": []any{}, "encrypted_content": "fixture-reasoning"}}, ordinary...)
	if digest(items[1:]) != digest(want) {
		t.Fatal("input field loss or speculative conversion", items)
	}
	if len(p.tools.tools) != 1 {
		t.Fatal("non-client declaration became a relay tool")
	}
}

func TestTwoTurnNativeReplayAndIsolation(t *testing.T) {
	s := fixtureServer()
	source := fixtureRequest()
	p := mustPlan(t, s, source, 7)
	native := nativeItem("exec_command", object{"cmd": "pwd"})
	response, err := transformResponse(context.Background(), completed(native), p.tools, p.store, true)
	if err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(object)
	if call["name"] != "exec_command" || call["call_id"] == native["call_id"] || !handlePattern.MatchString(str(call, "call_id")) {
		t.Fatal(call)
	}
	// Simulate the host stripping IDs and rewriting call_ / fc_ prefixes.
	replay := clone(call)
	delete(replay, "id")
	replay["call_id"] = "call_" + strings.TrimPrefix(str(call, "call_id"), "fc_")
	result := object{"type": "function_call_output", "call_id": call["call_id"], "output": "/tmp/project"}
	source["input"] = []any{message("user", "hello"), replay, result}
	p2 := mustPlan(t, s, source, 7)
	input := p2.body["input"].([]any)
	if digest(input[len(input)-2]) != digest(native) {
		t.Fatal("native item not restored byte-equivalently", input)
	}
	gotResult := input[len(input)-1].(object)
	if gotResult["call_id"] != native["call_id"] || gotResult["output"] != "/tmp/project" {
		t.Fatal(gotResult)
	}
	// The persisted record survives a plugin restart.
	s2 := New()
	s2.host = s.host
	mustPlan(t, s2, source, 7)
	for _, account := range []int64{8, 9} {
		if _, err := s.prepare(context.Background(), encoded(source), account, nil, s.config()); err == nil {
			t.Fatal("cross-account replay accepted")
		}
	}
	source["prompt_cache_key"] = "other-session"
	if _, err := s.prepare(context.Background(), encoded(source), 7, nil, s.config()); err == nil {
		t.Fatal("cross-session replay accepted")
	}
	source["prompt_cache_key"] = "session-one"
	replay["arguments"] = `{"cmd":"different command"}`
	if _, err := s.prepare(context.Background(), encoded(source), 7, nil, s.config()); err == nil {
		t.Fatal("modified replay accepted")
	}
	replay["arguments"] = call["arguments"]
	f := s.host.(*fakeHost)
	f.mu.Lock()
	for k, v := range f.values {
		var rec replayRecord
		json.Unmarshal(v, &rec)
		rec.ExpiresAt = time.Now().Unix() - 1
		f.values[k] = encoded(rec)
	}
	f.mu.Unlock()
	if _, err := s.prepare(context.Background(), encoded(source), 7, nil, s.config()); err == nil {
		t.Fatal("expired replay accepted")
	}
}

func TestNamespaceCustomAndFunctionSchemaValidation(t *testing.T) {
	s := fixtureServer()
	source := fixtureRequest()
	source["tools"] = []any{object{"type": "namespace", "name": "workspace", "tools": source["tools"]}, object{"type": "namespace", "name": "patches", "tools": []any{object{"type": "custom", "name": "apply_patch", "format": object{"type": "text"}}}}}
	p := mustPlan(t, s, source, 7)
	for _, tc := range []struct {
		kind, name string
		value      any
	}{{"function", "workspace.exec_command", object{"cmd": "pwd"}}, {"custom", "patches.apply_patch", "*** patch ***"}} {
		native := nativeItem(tc.name, tc.value)
		if tc.kind == "custom" {
			native["arguments"] = string(encoded(object{"code": string(encoded(object{"name": tc.name, "input": tc.value}))}))
		}
		client, err := p.tools.convert(native)
		if err != nil {
			t.Fatal(err)
		}
		if client["namespace"] == nil || strings.Contains(str(client, "name"), ".") {
			t.Fatal("namespace lost", client)
		}
		if err := p.store.save(context.Background(), native, client); err != nil {
			t.Fatal(err)
		}
		out, err := p.store.restore(context.Background(), []any{client, object{"type": str(client, "type") + "_output", "call_id": client["call_id"], "output": "ok"}})
		if err != nil || out[1].(object)["type"] != "function_call_output" {
			t.Fatal(out, err)
		}
	}
	for _, payload := range []object{{"wrong": true}, {"cmd": 42}, {"cmd": "pwd", "extra": true}} {
		if _, err := p.tools.convert(nativeItem("workspace.exec_command", payload)); err == nil {
			t.Fatal("schema bypass", payload)
		}
	}
}

func TestInvalidNativeToolsNeverBecomeClientCalls(t *testing.T) {
	p := mustPlan(t, fixtureServer(), fixtureRequest(), 7)
	cases := []object{nativeItem("undeclared", object{}), nativeItem("run_officejs", object{}), nativeItem("exec_command", object{"cmd": 1})}
	for _, code := range []string{`console.log("oops")`, `{"name":"exec_command","arguments":{"cmd":"pwd"}}; sideEffect()`, `{"name":"exec_command","name":"undeclared","arguments":{}}`, `{"name":"exec_command","arguments":{},"extra":"x"}`} {
		v := nativeItem("x", nil)
		v["arguments"] = string(encoded(object{"code": code}))
		cases = append(cases, v)
	}
	other := nativeItem("exec_command", object{"cmd": "pwd"})
	other["name"] = "native_search"
	cases = append(cases, other)
	for _, v := range cases {
		if _, err := transformResponse(context.Background(), completed(v), p.tools, p.store, true); err == nil {
			t.Fatal("unsafe tool accepted", v)
		}
	}
	v := nativeItem("exec_command", object{"cmd": "pwd"})
	if _, err := transformResponse(context.Background(), completed(v, v), p.tools, p.store, true); err == nil {
		t.Fatal("parallel calls accepted")
	}
}

func TestIdentityUsesHeadersOrJWTWithoutLeakingOtherHeaders(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString(encoded(object{"https://api.openai.com/auth": object{"chatgpt_account_id": "claim-account", "chatgpt_user_id": "claim-user"}}))
	h, err := bpsHeaders(http.Header{"Authorization": []string{"Bearer e30." + claims + ".sig"}, "Cookie": []string{"unrelated=cookie"}, "Originator": []string{"codex-tui"}})
	if err != nil || h.Get("Chatgpt-Account-Id") != "claim-account" || h.Get("X-Openai-Account-User-Id") != "claim-user" || h.Get("Cookie") != "" || h.Get("Originator") != "" || h.Get("X-Basispoints-Auth-Mode") != "chatgpt" {
		t.Fatal(h, err)
	}
	if _, err := bpsHeaders(http.Header{"Authorization": []string{"Bearer opaque"}}); err == nil {
		t.Fatal("missing ChatGPT account identity accepted")
	}
}

func TestSchemaCannotFetchRemoteReferences(t *testing.T) {
	source := fixtureRequest()
	source["tools"].([]any)[0].(object)["parameters"] = object{"$ref": "http://127.0.0.1:9/secret"}
	if _, err := readTools(source); err == nil {
		t.Fatal("external schema accepted")
	}
}
