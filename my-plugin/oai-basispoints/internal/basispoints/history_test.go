package basispoints

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

func TestReplayEmptyCustomInputRequiresOriginalString(t *testing.T) {
	s := fixtureServer()
	source := fixtureRequest()
	source["tools"] = []any{object{"type": "custom", "name": "read_probe"}}
	p := mustPlan(t, s, source, 7)
	native := nativeItem("read_probe", nil)
	native["arguments"] = string(encoded(object{"code": string(encoded(object{"name": "read_probe", "input": ""}))}))
	response, err := transformResponse(context.Background(), completed(native), p.tools, p.store, true)
	if err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(object)
	for _, tc := range []struct {
		name    string
		input   any
		missing bool
		valid   bool
	}{
		{"empty string", "", false, true},
		{"missing", nil, true, false},
		{"null", nil, false, false},
		{"number", 0, false, false},
		{"object", object{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replay := clone(call)
			replay["input"] = tc.input
			if tc.missing {
				delete(replay, "input")
			}
			source["input"] = []any{message("user", "hello"), replay, object{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": "done"}}
			_, err := s.prepare(context.Background(), encoded(source), 7, nil, s.config())
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	replay := clone(call)
	replay["namespace"] = object{"name": "invalid"}
	source["input"] = []any{message("user", "hello"), replay, object{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": "done"}}
	if _, err := s.prepare(context.Background(), encoded(source), 7, nil, s.config()); err == nil {
		t.Fatal("non-string namespace matched the original absent namespace")
	}
}

type unavailableReplayHost struct{ *fakeHost }

func (f unavailableReplayHost) KVGet(context.Context, *pluginv1.KVGetRequest, ...grpc.CallOption) (*pluginv1.KVGetResponse, error) {
	return nil, errors.New("fixture storage offline")
}

func TestMissingNativeRecordRequiresCompleteHistoryAndDoesNotWriteKV(t *testing.T) {
	handle := "ctc_bp_" + strings.Repeat("b", 32)
	call := object{"type": "custom_tool_call", "name": "exec", "namespace": "workspace", "call_id": handle, "input": toolProbeInput}
	result := object{"type": "custom_tool_call_output", "call_id": "call_" + strings.TrimPrefix(handle, "ctc_"), "output": "already executed"}
	s := fixtureServer()
	source := fixtureRequest()
	source["tool_choice"] = "none"
	source["input"] = []any{message("user", "hello"), call, result}
	d := &replayDiagnostic{}
	ctx := context.WithValue(context.Background(), replayDiagnosticContextKey{}, d)
	plan, err := s.prepare(ctx, encoded(source), 7, nil, s.config())
	if err != nil {
		t.Fatal(err)
	}
	if d.Missing != 1 || d.Rebuilt != 1 || d.NativeHits != 0 || d.Failure != "" || s.host.(*fakeHost).kvWrites != 0 {
		t.Fatal("fallback stored or misreported native state", d)
	}
	items := plan.body["input"].([]any)
	native := items[len(items)-2].(object)
	envelope, _, err := decodeRelayEnvelope(native)
	if err != nil || envelope["input"] != toolProbeInput || envelope["name"] != "workspace.exec" || items[len(items)-1].(object)["call_id"] != native["call_id"] {
		t.Fatal("complete historical payload changed", err)
	}
	withoutOutput := clone(result)
	delete(withoutOutput, "output")
	for _, history := range [][]any{{call}, {result}, {result, call}, {call, result, result}, {call, withoutOutput}} {
		source["input"] = history
		if _, err := s.prepare(ctx, encoded(source), 7, nil, s.config()); err == nil {
			t.Fatal("accepted incomplete/ambiguous history", history)
		}
	}
	s.host = unavailableReplayHost{s.host.(*fakeHost)}
	source["input"] = []any{call, result}
	if _, err := s.prepare(ctx, encoded(source), 7, nil, s.config()); err == nil || d.Failure != "storage_error" {
		t.Fatal("KV outage was treated as a miss", err, d)
	}
}

func TestImportedHistoryDoesNotAcceptWrappedPluginHandles(t *testing.T) {
	s := fixtureServer()
	source := fixtureRequest()
	source["tool_choice"] = "none"
	marker := "bp_" + strings.Repeat("a", 32)
	for _, id := range []string{"ctc_legacy_call", "fc_call_legacy_call", "fc_call_" + marker, "call_ctc_" + marker, "ctc_fc_call_" + marker} {
		t.Run(id, func(t *testing.T) {
			source["input"] = []any{
				message("user", "hello"),
				object{"type": "custom_tool_call", "name": "read_probe", "call_id": id, "input": "probe.txt"},
				object{"type": "custom_tool_call_output", "call_id": id, "output": "done"},
			}
			_, err := s.prepare(context.Background(), encoded(source), 8, nil, s.config())
			if (err == nil) != strings.HasSuffix(id, "legacy_call") {
				t.Fatalf("wrapped plugin history must fail; complete external history must remain supported: %v", err)
			}
		})
	}
	if s.host.(*fakeHost).kvWrites != 0 {
		t.Fatal("imported history must not create executable replay records")
	}
	if _, err := historicalTransportCall(object{"type": "custom_tool_call", "name": "read_probe", "namespace": object{}, "call_id": "ctc_legacy_call", "input": ""}); err == nil {
		t.Fatal("imported namespace must not be silently discarded")
	}
}

func TestImportedHistoryRequiresAnExplicitNonNullResult(t *testing.T) {
	plan := mustPlan(t, fixtureServer(), fixtureRequest(), 7)
	call := object{"type": "function_call", "name": "exec_command", "call_id": "old_call", "arguments": `{"cmd":"pwd"}`}
	for _, output := range []object{
		{"type": "function_call_output", "call_id": "old_call"},
		{"type": "function_call_output", "call_id": "old_call", "output": nil},
	} {
		if _, err := plan.store.restore(context.Background(), []any{call, output}); err == nil {
			t.Fatal("missing imported result became success")
		}
	}
	if _, err := plan.store.restore(context.Background(), []any{call, object{"type": "function_call_output", "call_id": "old_call", "output": ""}}); err != nil {
		t.Fatal("explicit empty result rejected", err)
	}
}
