package basispoints

import (
	"context"
	"strings"
	"testing"
)

func TestRelayEnvelopeCompatibilityPreservesCustomInputAndNativeReplay(t *testing.T) {
	const secret = "private-fixture-command"
	rawInput := toolProbeInput + "\n// " + secret
	inner := object{"name": "workspace.exec", "input": rawInput}
	fenced := "```json\r\n" + string(encoded(inner)) + "\r\n```"
	nested := object{"name": "functions.run_officejs", "arguments": object{"code": fenced}}
	twice := object{"name": "run_officejs", "arguments": string(encoded(object{"code": string(encoded(nested))}))}
	for _, code := range []any{inner, string(encoded(inner)), fenced, twice, "const call = " + string(encoded(inner)) + ";", "```js\n" + string(encoded(inner)) + "\n```", "Relay: " + string(encoded(inner)) + "; sideEffect()"} {
		s := fixtureServer()
		source := fixtureRequest()
		source["tools"] = []any{object{"type": "namespace", "name": "workspace", "tools": []any{object{"type": "custom", "name": "exec"}}}}
		plan := mustPlan(t, s, source, 7)
		var report relayDiagnostic
		plan.tools.observeRelay = func(d relayDiagnostic) { report = d }
		native := nativeItem("unused", nil)
		native["arguments"] = string(encoded(object{"code": code}))
		response, err := transformResponse(context.Background(), completed(native), plan.tools, plan.store, true)
		if err != nil {
			t.Fatal(err)
		}
		call := response["output"].([]any)[0].(object)
		if call["input"] != rawInput || call["name"] != "exec" || call["namespace"] != "workspace" {
			t.Fatal("custom input changed", call)
		}
		if report.State != "decoded" || strings.Contains(string(encoded(report)), secret) {
			t.Fatal("invalid or leaking diagnostic", report)
		}
		restored, err := plan.store.restore(context.Background(), []any{call, object{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": "done"}})
		if err != nil || digest(restored[0]) != digest(native) {
			t.Fatal("original native identity/arguments were not preserved", err)
		}
	}
}

func TestRelayEnvelopeRejectsAmbiguousOrDamagedPayloads(t *testing.T) {
	inner := object{"name": "exec_command", "arguments": object{"cmd": "pwd"}}
	if _, _, err := decodeRelayEnvelope(object{"arguments": object{"code": inner}}); err == nil {
		t.Fatal("outer Responses arguments must remain a JSON string")
	}
	jsonText := string(encoded(inner))
	tooDeep := inner
	for range 3 {
		tooDeep = object{"name": "run_officejs", "arguments": object{"code": tooDeep}}
	}
	for _, tc := range []struct {
		name string
		code any
		kind string
	}{
		{"trailing object", jsonText + jsonText, "multiple_objects"},
		{"damaged outer", `{"outer": broken, "inner":` + jsonText + `}`, "invalid_json"},
		{"truncated", `{"name":`, "incomplete_json"},
		{"duplicate", `{"name":"exec_command","name":"hidden"}`, "duplicate_key"},
		{"array in fence", "```json\n[]\n```", "invalid_json"},
		{"object array in fence", "```json\n[" + jsonText + "]\n```", "not_object"},
		{"assigned array", "const calls = [" + jsonText + "];", "not_object"},
		{"array", "[]", "not_object"},
		{"null", nil, "invalid_type"},
		{"wrappers", tooDeep, "wrapper_limit"},
		{"wrapper ambiguity", object{"name": "run_officejs", "arguments": object{"code": inner}, "input": "extra"}, "wrapper_fields"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := nativeItem("unused", nil)
			native["arguments"] = string(encoded(object{"code": tc.code}))
			_, d, err := decodeRelayEnvelope(native)
			if err == nil || d.State != "rejected" || d.ErrorKind != tc.kind {
				t.Fatalf("kind=%s diagnostic=%+v error=%v", tc.kind, d, err)
			}
			if strings.Contains(string(encoded(d)), "private") || strings.Contains(err.Error(), "private") {
				t.Fatal("code leaked in diagnostic")
			}
		})
	}
	plan := mustPlan(t, fixtureServer(), fixtureRequest(), 7)
	for _, code := range []any{object{"name": "undeclared", "arguments": object{}}, object{"name": "exec_command", "arguments": object{"cmd": 42}}} {
		native := nativeItem("unused", nil)
		native["arguments"] = string(encoded(object{"code": "```json\n" + string(encoded(code)) + "\n```"}))
		if _, err := plan.tools.convert(native); err == nil {
			t.Fatal("compatibility bypassed tool catalog or schema")
		}
	}
}

func TestRelayInvalidBackslashesPreserveCommandBytes(t *testing.T) {
	for _, tc := range []struct {
		code, expected string
		repairs        int
	}{
		{`{"name":"exec_command","arguments":{"cmd":"rg -n \( pattern"}}`, `rg -n \( pattern`, 1},
		{`const call = {"name":"exec_command","arguments":{"cmd":"rg \d+ C:\work\file"}};`, "rg \\d+ C:\\work\file", 2},
		{`{"name":"exec_command","arguments":{"cmd":"\u4e2d\n\t\"quoted\"\\end\/"}}`, "中\n\t\"quoted\"\\end/", 0},
		{`{"name":"exec_command","arguments":{"cmd":"\uZZZZ"}}`, `\uZZZZ`, 1},
	} {
		native := nativeItem("unused", nil)
		native["arguments"] = string(encoded(object{"code": tc.code}))
		inner, d, err := decodeRelayEnvelope(native)
		if err != nil || inner["arguments"].(object)["cmd"] != tc.expected || d.Fields[1].BackslashesRepaired != tc.repairs {
			t.Fatalf("command altered: %+v, diagnostic=%+v, err=%v", inner, d, err)
		}
	}
	// Outer Responses arguments are never repaired. Otherwise one repair
	// could change the meaning of code before the code-layer decoder sees it.
	_, d, err := decodeRelayEnvelope(object{"arguments": `{"code":"private\q"}`})
	if err == nil || d.ErrorKind != "invalid_escape" || d.Fields[0].BackslashesRepaired != 0 {
		t.Fatal("outer arguments were repaired", d, err)
	}
}
