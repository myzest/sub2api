package basispoints

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func markedNativeFixture(summary string, code any, metadata any) object {
	native := nativeItem("unused", nil)
	native["arguments"] = string(encoded(object{"summary": summary, "code": code, "extended_summary": metadata, "destructive": false, "references": []any{}}))
	return native
}

func codeToolFixture() object {
	return object{"type": "function", "name": "run_code", "parameters": object{
		"type": "object", "additionalProperties": false, "required": []any{"code", "title"},
		"properties": object{"code": object{"type": "string"}, "title": object{"type": "string"}, "count": object{"type": "integer"}},
	}}
}

func rawCatalogFixture() object {
	source := fixtureRequest()
	source["tools"] = []any{object{"type": "namespace", "name": "client", "tools": []any{
		object{"type": "custom", "name": "exec"}, codeToolFixture(),
		object{"type": "function", "name": "plain", "parameters": object{"type": "object"}},
	}}}
	return source
}

func TestMarkedTransportsPreserveCodeAndOriginalReplay(t *testing.T) {
	// Generated fixtures only. They are data, never executed as commands.
	for _, text := range []string{"", " \r\n\tconst s = 'C:\\work\\file'; text(\"a\\b\");\n", "中文🙂\r\n\"quote\"\\q\\uXX", "{\"name\":\"different\",\"input\":\"data not another call\"}"} {
		for _, mode := range []string{"custom", "function_code"} {
			s := fixtureServer()
			p := mustPlan(t, s, rawCatalogFixture(), 7)
			marker, metadata := rawCustomPrefix+"client.exec", ""
			if mode == "function_code" {
				marker, metadata = rawFunctionCodePrefix+"client.run_code", string(encoded(object{"title": "fixture", "count": json.Number("9007199254740993")}))
			}
			native := markedNativeFixture(marker, text, metadata)
			var report relayDiagnostic
			p.tools.observeRelay = func(d relayDiagnostic) { report = d }
			response, err := transformResponse(context.Background(), completed(native), p.tools, p.store, true)
			if err != nil {
				t.Fatal(err)
			}
			call := response["output"].([]any)[0].(object)
			if mode == "custom" {
				if _, exists := call["encrypted_function_args"]; exists {
					t.Fatal("function encryption metadata added to custom call")
				}
				if call["input"] != text || str(call, "type") != "custom_tool_call" {
					t.Fatal("custom bytes changed")
				}
			} else {
				requirePlaintextAgentArgs(t, call)
				args, err := decodeObject([]byte(str(call, "arguments")))
				if err != nil || args["code"] != text || args["count"] != json.Number("9007199254740993") {
					t.Fatal("function code or number changed", err)
				}
			}
			if report.Transport != mode || report.State != "decoded" || strings.Contains(string(encoded(report)), "work") {
				t.Fatal("incorrect/leaking diagnostic")
			}
			resultKind := "function_call_output"
			if mode == "custom" {
				resultKind = "custom_tool_call_output"
			}
			restored, err := p.store.restore(context.Background(), []any{call, object{"type": resultKind, "call_id": call["call_id"], "output": "ack"}})
			if err != nil || digest(restored[0]) != digest(native) {
				t.Fatal("native replay changed", err)
			}
		}
	}
}

func TestMarkedTransportsRejectWrongCatalogSchemaAndMetadata(t *testing.T) {
	p := mustPlan(t, fixtureServer(), rawCatalogFixture(), 7)
	validMetadata := string(encoded(object{"title": "fixture"}))
	for _, tc := range []struct {
		marker         string
		code, metadata any
	}{
		{rawCustomPrefix + "client.missing", "x", ""},
		{rawCustomPrefix + "codex_client__client.exec", "x", ""},
		{rawCustomPrefix + "client.run_code", "x", ""},
		{rawCustomPrefix + "client.exec ", "x", ""},
		{rawCustomPrefix + "client/exec", "x", ""},
		{rawCustomPrefix + "client.exec", object{}, ""},
		{rawCustomPrefix + "client.exec", strings.Repeat("x", maxRawTransportBytes+1), ""},
		{rawFunctionCodePrefix + "client.exec", "x", validMetadata},
		{rawFunctionCodePrefix + "client.plain", "x", validMetadata},
		{rawFunctionCodePrefix + "client.run_code", "x", "{}"},
		{rawFunctionCodePrefix + "client.run_code", "x", "[]"},
		{rawFunctionCodePrefix + "client.run_code", "x", "null"},
		{rawFunctionCodePrefix + "client.run_code", "x", validMetadata + "{}"},
		{rawFunctionCodePrefix + "client.run_code", "x", "{\"title\":\"a\",\"title\":\"b\"}"},
		{rawFunctionCodePrefix + "client.run_code", "x", string(encoded(object{"title": "x", "code": nil}))},
		{rawFunctionCodePrefix + "client.run_code", "x", string(encoded(object{"title": "x", "extra": true}))},
		{rawFunctionCodePrefix + "client.run_code", "x", object{"title": "x"}},
	} {
		if _, err := p.tools.convert(markedNativeFixture(tc.marker, tc.code, tc.metadata)); err == nil {
			t.Fatalf("invalid marked transport accepted: %s", tc.marker)
		}
	}
	unmarked := markedNativeFixture("ordinary call", "text('not JSON');", "")
	if _, err := p.tools.convert(unmarked); err == nil {
		t.Fatal("unmarked script dispatched")
	}
	native := markedNativeFixture(rawCustomPrefix+"client.exec", "x", "")
	delete(native, "call_id")
	if _, err := p.tools.convert(native); err == nil {
		t.Fatal("missing native identity accepted")
	}
	source := rawCatalogFixture()
	source["tool_choice"] = "none"
	if _, err := mustPlan(t, fixtureServer(), source, 7).tools.convert(markedNativeFixture(rawCustomPrefix+"client.exec", "x", "")); err == nil {
		t.Fatal("tool_choice none bypassed")
	}
	source = rawCatalogFixture()
	source["tool_choice"] = object{"type": "function", "name": "run_code", "namespace": "client"}
	if _, err := mustPlan(t, fixtureServer(), source, 7).tools.convert(markedNativeFixture(rawCustomPrefix+"client.exec", "x", "")); err == nil {
		t.Fatal("forced tool bypassed")
	}
}

func TestRawTransportCatalogIncludesPositionalToolsAndHistory(t *testing.T) {
	source := rawCatalogFixture()
	tools := source["tools"]
	delete(source, "tools")
	source["input"] = []any{object{"type": "message", "role": "user", "content": "fixture"}, object{"type": "additional_tools", "tools": tools}}
	p := mustPlan(t, fixtureServer(), source, 7)
	text := string(encoded(p.body))
	for _, marker := range []string{rawCustomPrefix + "client.exec", rawFunctionCodePrefix + "client.run_code"} {
		if !strings.Contains(text, marker) {
			t.Fatal("missing positional marker", marker)
		}
	}
	if strings.Contains(text, "code field must contain a serialized JSON object, never JavaScript") {
		t.Fatal("conflicting legacy instruction")
	}
	old := object{"type": "custom_tool_call", "name": "exec", "namespace": "client", "call_id": "call_external", "input": "\r\n raw \\q \" quote"}
	native, err := historicalTransportCall(old)
	if err != nil {
		t.Fatal(err)
	}
	inner, d, err := decodeRelayEnvelope(native)
	if err != nil || d.Transport != "custom" || inner["input"] != old["input"] {
		t.Fatal("fallback history changed raw bytes", err)
	}
}

func TestForwardInvalidEnvelopeEndsAsFailedWithoutReplay(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		var requests atomic.Int32
		native := markedNativeFixture("legacy", "{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"fixture\"broken}}", "")
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, nativeStream(native))
			io.WriteString(w, eventBytes(object{"type": "response.completed", "response": completed(native)}))
		}))
		s := fixtureServer()
		routeFixture(s, up.URL+"/responses")
		source := fixtureRequest()
		source["stream"] = streaming
		body := encoded(source)
		frames := forward(t, transportClient(t, s), bpsStart(body), body)
		up.Close()
		if requests.Load() != 1 || frames[len(frames)-1].GetEnd() == nil || s.host.(*fakeHost).kvWrites != 0 {
			t.Fatal("invalid call replayed, saved or transport failed")
		}
		for _, f := range frames {
			if f.GetError() != nil {
				t.Fatal("generic gRPC transport error")
			}
		}
		var result object
		if streaming {
			sequence := 0
			err := readSSE(bytes.NewReader(bodyOf(frames)), func(e object) (bool, error) {
				if string(encoded(e["sequence_number"])) != string(encoded(sequence)) {
					t.Error("sequence gap")
				}
				sequence++
				if str(e, "type") == "response.failed" {
					result = e["response"].(object)
					return true, nil
				}
				return false, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		} else {
			var err error
			result, err = decodeObject(bodyOf(frames))
			if err != nil {
				t.Fatal(err)
			}
		}
		if str(result, "status") != "failed" || str(result, "id") != "resp_fixture" || len(result["output"].([]any)) != 0 {
			t.Fatal("invalid failed terminal")
		}
		if digest(result["usage"]) != digest(completed()["usage"]) {
			t.Fatal("terminal usage lost")
		}
		if !strings.Contains(str(result["error"].(object), "message"), s.lastRequest.ID) || s.lastRequest.ErrorSource != "tool_relay" {
			t.Fatal("diagnostic provenance lost")
		}
		if str(result["error"].(object), "type") != "invalid_request_error" {
			t.Fatal("deterministic bridge error could trigger host account failover")
		}
		if strings.Contains(string(bodyOf(frames)), "run_officejs") || strings.Contains(string(bodyOf(frames)), "fixture\\\"broken") {
			t.Fatal("native body leaked")
		}
	}
}

func TestMarkedCustomStreamReleasesOneCallWithoutJSONRepair(t *testing.T) {
	p := mustPlan(t, fixtureServer(), rawCatalogFixture(), 7)
	code := "text(\"literal\\q\");\r\n"
	native := markedNativeFixture(rawCustomPrefix+"client.exec", code, "")
	var output bytes.Buffer
	r := newRelay(context.Background(), p, func(b []byte) error { output.Write(b); return nil })
	raw := nativeStream(native) + eventBytes(object{"type": "response.completed", "response": completed(native)})
	if err := r.consume(upstreamResponse("text/event-stream", strings.NewReader(raw))); err != nil {
		t.Fatal(err)
	}
	call := r.response["output"].([]any)[0].(object)
	if call["input"] != code || str(call, "namespace") != "client" || strings.Contains(output.String(), "run_officejs") {
		t.Fatal("marked stream corrupted")
	}
}
