package basispoints

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func uniqueNative(index int, name string, args object) object {
	item := nativeItem(name, args)
	item["id"], item["call_id"] = fmt.Sprintf("fc_native_%d", index), fmt.Sprintf("call_native_%d", index)
	return item
}

func TestMixedToolsKeepValidCallsAndSerialFirstValid(t *testing.T) {
	for _, parallel := range []bool{true, false} {
		source := fixtureRequest()
		source["parallel_tool_calls"] = parallel
		plan := mustPlan(t, fixtureServer(), source, 7)
		bad := uniqueNative(1, "undeclared", object{})
		first := uniqueNative(2, "exec_command", object{"cmd": "pwd"})
		second := uniqueNative(3, "exec_command", object{"cmd": "ls"})
		result, err := transformResponse(context.Background(), completed(bad, first, terminalText("working"), second), plan.tools, plan.store, true)
		if err != nil {
			t.Fatal(err)
		}
		calls := responseToolCalls(result)
		want := 1
		if parallel {
			want = 2
		}
		if len(calls) != want || plan.tools.skippedTools != 3-want || digest(plan.tools.outputIndices[:2]) != digest([]int{1, 2}) {
			t.Fatal("mixed/serial call filtering failed", result, plan.tools.outputIndices)
		}
		if calls[0]["arguments"] != `{"cmd":"pwd"}` {
			t.Fatal("did not retain the first valid call")
		}
		if _, err := transformResponse(context.Background(), completed(first, first), plan.tools, plan.store, true); err == nil {
			t.Fatal("serial filtering hid a duplicate identity")
		}
	}
}

func TestDirectDeclaredNativeCallsReplayOriginalType(t *testing.T) {
	for _, custom := range []bool{false, true} {
		source := fixtureRequest()
		native := uniqueNative(1, "exec_command", nil)
		native["name"], native["arguments"] = "codex_client__exec_command", `{"cmd":"pwd"}`
		if custom {
			source["tools"] = []any{object{"type": "namespace", "name": "workspace", "tools": []any{object{"type": "custom", "name": "exec"}}}}
			native["type"], native["name"], native["namespace"], native["input"] = "custom_tool_call", "exec", "workspace", toolProbeInput
			delete(native, "arguments")
		}
		plan := mustPlan(t, fixtureServer(), source, 7)
		response, err := transformResponse(context.Background(), completed(native), plan.tools, plan.store, true)
		if err != nil {
			t.Fatal(err)
		}
		client := responseToolCalls(response)[0]
		output := object{"type": str(client, "type") + "_output", "call_id": client["call_id"], "output": "done"}
		restored, err := plan.store.restore(context.Background(), []any{client, output})
		if err != nil || digest(restored[0]) != digest(native) || restored[1].(object)["type"] != str(native, "type")+"_output" || restored[1].(object)["call_id"] != native["call_id"] {
			t.Fatal("direct call replay changed native identity/type", restored, err)
		}
	}
}

func TestNativePlanMapsBothDirectionsAndAcknowledgesDisplayOutput(t *testing.T) {
	source := fixtureRequest()
	source["tools"] = []any{object{"type": "function", "name": "update_plan", "parameters": object{"type": "object", "required": []any{"plan"}}}}
	plan := mustPlan(t, fixtureServer(), source, 7)
	native := uniqueNative(1, "update_plan", nil)
	native["name"] = "update_plan"
	native["arguments"] = `{"summary":"Reading files","plan":[{"description":"Read README","status":"doing"},{"title":"Report","status":"not-started"}]}`
	response, err := transformResponse(context.Background(), completed(native), plan.tools, plan.store, true)
	if err != nil {
		t.Fatal(err)
	}
	client := responseToolCalls(response)[0]
	args, _ := decodeObject([]byte(str(client, "arguments")))
	steps := args["plan"].([]any)
	if args["explanation"] != "Reading files" || steps[0].(object)["step"] != "Read README" || steps[0].(object)["status"] != "in_progress" || steps[1].(object)["status"] != "pending" {
		t.Fatal(args)
	}
	result := object{"type": "function_call_output", "call_id": client["call_id"], "output": "Plan updated"}
	restored, err := plan.store.restore(context.Background(), []any{client, result})
	if err != nil || digest(restored[0]) != digest(native) || restored[1].(object)["output"] != `{"status":"ok"}` {
		t.Fatal(restored, err)
	}
	// Complete history is still usable without a cached original.
	plan.store.host = newFakeHost()
	restored, err = plan.store.restore(context.Background(), []any{client, result})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := restored[0].(object)
	args, _ = decodeObject([]byte(str(rebuilt, "arguments")))
	if rebuilt["name"] != "update_plan" || args["plan"].([]any)[0].(object)["description"] != "Read README" {
		t.Fatal(rebuilt)
	}
	if output := normalizeNativeToolOutput(object{"name": "run_officejs"}, "unsupported call: run_officejs"); !strings.Contains(output.(string), "Retry once") {
		t.Fatal(output)
	}
}

func TestTurnIterationCountsResultRoundsAndEffortAliases(t *testing.T) {
	input := []any{message("user", "read"), object{"type": "function_call"}, object{"type": "function_call_output"}, object{"type": "custom_tool_call_output"}}
	turn, iteration := turnState(input)
	if iteration != "2" {
		t.Fatal(iteration)
	}
	later := append(append([]any(nil), input...), object{"type": "function_call"}, object{"type": "function_call_output"})
	nextTurn, nextIteration := turnState(later)
	if turn != nextTurn || nextIteration != "3" {
		t.Fatal("turn identity changed across result rounds")
	}
	for _, alias := range []string{" X-HIGH ", "extra-high", "extra_high", "max"} {
		source := fixtureRequest()
		source["reasoning_effort"], source["reasoning"] = alias, object{"effort": "xhigh"}
		if mustPlan(t, fixtureServer(), source, 7).body["reasoning_effort"] != "xhigh" {
			t.Fatal(alias)
		}
	}
	source := fixtureRequest()
	source["reasoning"] = object{"effort": "unknown"}
	if _, err := fixtureServer().prepare(context.Background(), encoded(source), 7, nil, defaultConfig()); err == nil {
		t.Fatal("unknown effort silently downgraded")
	}
}
