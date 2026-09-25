package basispoints

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestClientToolTraceRecordsFactsWithoutContentsOrPolicyInference(t *testing.T) {
	input := []any{
		message("user", "private-prompt"),
		object{"type": "custom_tool_call", "name": "exec", "namespace": "functions", "call_id": "private-call-one", "input": "private-command"},
		object{"type": "custom_tool_call_output", "call_id": "private-call-one", "output": "Security policy blocked localhost: private-error-body"},
		object{"type": "function_call", "name": "open_in_codex", "namespace": "mcp__codex_app", "call_id": "private-call-two", "arguments": `{"path":"private-path"}`},
		object{"type": "function_call_output", "call_id": "private-call-two", "output": object{
			"isError": false, "structuredContent": object{"status": "queued", "path": "private-path"},
			"content": []any{object{"type": "text", "text": `{"status":"queued","token":"private-token"}`}},
		}},
	}
	d := &requestDiagnostic{}
	d.input(encoded(object{"input": input}))
	if d.ClientToolHistoryCount != 4 || len(d.ClientToolHistory) != 4 {
		t.Fatal(d.ClientToolHistory)
	}
	if got := d.ClientToolHistory[1]; got.Name != "functions.exec" || len(got.Signals) != 0 || got.InputIndex != 2 {
		t.Fatal("natural language was treated as a policy verdict", got)
	}
	got := d.ClientToolHistory[3]
	if got.Name != "mcp__codex_app.open_in_codex" || !slices.Contains(got.Signals, "output.structuredContent.status=queued") || !slices.Contains(got.Signals, "output.isError=false") {
		t.Fatal("missing client-reported acknowledgement", got)
	}
	if strings.Contains(string(encoded(d)), "private-") {
		t.Fatal("diagnostic retained tool contents, arguments or IDs", string(encoded(d)))
	}
}

func TestClientToolTraceBoundsAndUnambiguousPairing(t *testing.T) {
	input := []any{
		message("user", "previous turn"),
		object{"type": "function_call", "name": "previous_tool", "call_id": "old"},
		object{"type": "function_call_output", "call_id": "old", "output": `{"status":"failed"}`},
		message("user", "current turn"),
	}
	for i := range 20 {
		input = append(input, object{"type": "custom_tool_call", "namespace": "functions", "name": "exec", "call_id": fmt.Sprint(i)})
	}
	d := &requestDiagnostic{}
	d.input(encoded(object{"input": input}))
	if d.ClientToolHistoryCount != 20 || len(d.ClientToolHistory) != 16 || d.ClientToolHistory[0].InputIndex != 8 || d.ClientToolHistory[15].InputIndex != 23 {
		t.Fatal("history window or bounded ordering is incorrect", d.ClientToolHistory)
	}
	input = append(input,
		object{"type": "function_call", "name": "different_tool", "call_id": "0"},
		object{"type": "custom_tool_call_output", "call_id": "0", "output": `{"status":"queued"}`},
		object{"type": "function_call_output", "call_id": "1", "output": `{"exit_code":1}`},
		object{"type": "custom_tool_call_output", "call_id": "missing"},
	)
	d = &requestDiagnostic{}
	d.input(encoded(object{"input": input}))
	for _, row := range d.ClientToolHistory[len(d.ClientToolHistory)-3:] {
		if row.Name != "" {
			t.Fatal("ambiguous, mismatched or orphan result was linked", row)
		}
	}
	if d.ClientToolHistory[15].OutputFormat != "missing" {
		t.Fatal("missing output was presented as a result")
	}
}

func TestClientToolTraceOnlyAcceptsBoundedMachineFields(t *testing.T) {
	cases := []struct {
		output any
		want   []string
	}{
		{`{"status":"queued"}`, []string{"output.status=queued"}},
		{`{"status":"private-value","isError":"private-value","exit_code":"private-value"}`, nil},
		{`{"status":"queued","status":"failed"}`, nil},
		{`prefix {"status":"blocked"}`, nil},
		{strings.Repeat(" ", 64<<10) + `{"status":"blocked"}`, nil},
		{object{"exit_code": json.Number("4294967295"), "is_error": true, "body": object{"status": "failed"}}, []string{"output.is_error=true", "output.exit_code=4294967295"}},
		{object{"exit_code": json.Number("4294967296")}, nil},
		{`{"content":[{"type":"text","text":"{\"status\":\"queued\",\"url\":\"private-url\"}"}]}`, []string{"output.content[0].text.status=queued"}},
	}
	for _, tc := range cases {
		d := &requestDiagnostic{}
		d.input(encoded(object{"input": []any{object{"type": "custom_tool_call_output", "output": tc.output}}}))
		if got := d.ClientToolHistory[0].Signals; !slices.Equal(got, tc.want) {
			t.Fatalf("signals %v, want %v", got, tc.want)
		}
	}
	parts := []any{}
	for range 100 {
		parts = append(parts, object{"type": "text", "text": `{"status":"queued"}`})
	}
	d := &requestDiagnostic{}
	d.input(encoded(object{"input": []any{object{"type": "custom_tool_call_output", "output": parts}}}))
	if len(d.ClientToolHistory[0].Signals) != 8 {
		t.Fatal("result field limit not applied")
	}
}

func TestPreviewWrapperAndClientResultAreTransparent(t *testing.T) {
	s := fixtureServer()
	source := fixtureRequest()
	description := "Nested tools include tools.mcp__codex_app__open_in_codex; follow the documented permissions."
	source["tools"] = nil
	source["input"] = []any{
		object{"type": "additional_tools", "tools": []any{object{"type": "namespace", "name": "functions", "tools": []any{object{"type": "custom", "name": "exec", "description": description}}}}},
		message("user", "preview this file"),
	}
	plan := mustPlan(t, s, source, 7)
	if !strings.Contains(string(encoded(plan.body)), description) || len(plan.tools.tools) != 1 {
		t.Fatal("wrapper documentation lost or fabricated nested tools")
	}
	code := `text(await tools.mcp__codex_app__open_in_codex({target:{type:"file",path:"/workspace/index.html"}}));`
	native := nativeItem("functions.exec", nil)
	native["arguments"] = string(encoded(object{"code": string(encoded(object{"name": "functions.exec", "input": code}))}))
	converted, err := transformResponse(context.Background(), completed(native), plan.tools, plan.store, true)
	if err != nil {
		t.Fatal(err)
	}
	call := responseToolCalls(converted)[0]
	if call["input"] != code || call["namespace"] != "functions" || call["name"] != "exec" {
		t.Fatal("nested invocation changed", call)
	}
	output := object{"isError": false, "structuredContent": object{"status": "queued"}}
	source["input"] = append(source["input"].([]any), call, object{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": output})
	d := &requestDiagnostic{}
	d.input(encoded(source))
	d.output(converted)
	plan = mustPlan(t, s, source, 7)
	items := plan.body["input"].([]any)
	if digest(items[len(items)-1].(object)["output"]) != digest(output) {
		t.Fatal("acknowledgement was rewritten into a success/failure verdict")
	}
	if len(d.OutputTools) != 1 || d.OutputTools[0].Name != "functions.exec" || d.OutputToolCalls != 1 {
		t.Fatal("output diagnostic guessed a nested tool", d.OutputTools)
	}
}
