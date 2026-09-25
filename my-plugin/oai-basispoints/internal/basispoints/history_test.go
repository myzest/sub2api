package basispoints

import (
	"context"
	"strings"
	"testing"
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
