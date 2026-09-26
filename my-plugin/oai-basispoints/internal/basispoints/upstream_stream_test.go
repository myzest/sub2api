package basispoints

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUpstreamRawErrorProjectionIsBoundedAndDoesNotCopyContext(t *testing.T) {
	message := "failure for 'fixture prompt' https://fixture.invalid/private?q=1 user@example.invalid"
	failure := object{"message": message, "code": "capacity_fixture", "type": "server_error", "param": "input", "input": "MUST_NOT_COPY_INPUT", "ctx": object{"authorization": "MUST_NOT_COPY_CONTEXT"}}
	for _, event := range []object{
		failure,
		{"type": "error", "error": failure},
		{"type": "response.failed", "response": object{"error": failure, "output": []any{"MUST_NOT_COPY_OUTPUT"}}},
	} {
		fields, state := upstreamFailureFields(event)
		if state != "captured_raw" || fields["message"] != message || fields["type"] != "server_error" {
			t.Fatal("upstream fields changed", state, fields)
		}
		if strings.Contains(string(encoded(fields)), "MUST_NOT_COPY") {
			t.Fatal("copied non-error context")
		}
	}
	for _, test := range []struct{ body, state string }{
		{"", "empty"}, {"<html>fixture</html>", "non_json"}, {"{", "invalid_json"},
		{strings.Repeat("x", maxErrorDiagnosticBytes+1), "too_large"},
		{string(encoded(object{"type": "error"})), "no_fields"},
		{string(encoded(object{"error": object{"type": "server_error"}})), "captured_raw"},
	} {
		_, state := readUpstreamDiagnostic(strings.NewReader(test.body))
		if state != test.state {
			t.Fatalf("want %s got %s", test.state, state)
		}
	}
	fields, state := upstreamFailureFields(object{"type": "error", "message": strings.Repeat("x", maxErrorDiagnosticBytes+1)})
	if len(fields) != 0 || state != "too_large" {
		t.Fatal("oversized error accepted")
	}
}

func TestForwardUpstreamErrorEndsTransportWithoutRetryOrToolRelease(t *testing.T) {
	message := "fixture rejection for 'raw input' https://fixture.invalid/path"
	failure := object{"code": "capacity_fixture", "type": "server_error", "message": message, "param": "input", "ctx": "MUST_NOT_COPY_CONTEXT"}
	for _, mode := range []string{"flat", "nested", "alias", "held_tool", "failed_sse", "failed_json", "error_json"} {
		for _, streaming := range []bool{false, true} {
			t.Run(mode+"/stream="+string(encoded(streaming)), func(t *testing.T) {
				var requests atomic.Int32
				native := nativeItem("exec_command", object{"cmd": "MUST_NOT_RELEASE_SCRIPT"})
				event := object{"type": "error", "error": failure}
				if mode == "flat" {
					event = object{"type": "error", "message": message, "code": "capacity_fixture", "param": "input"}
				}
				if mode == "alias" {
					event["type"] = "response.error"
				}
				terminal := completed(native)
				terminal["status"], terminal["error"] = "failed", failure
				terminal["usage"] = object{"input_tokens": 11, "output_tokens": 0, "total_tokens": 11}
				if mode == "failed_sse" {
					event = object{"type": "response.failed", "response": terminal}
				}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("X-Request-Id", "stream-error-fixture")
					if mode == "failed_json" || mode == "error_json" {
						w.Header().Set("Content-Type", "application/json")
						if mode == "error_json" {
							w.Write(encoded(object{"error": failure}))
						} else {
							w.Write(encoded(terminal))
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if mode == "held_tool" {
						io.WriteString(w, nativeStream(native))
					}
					io.WriteString(w, eventBytes(event))
					// A later completed event must never recover an explicit failure.
					io.WriteString(w, eventBytes(object{"type": "response.completed", "response": completed(native)}))
				}))
				defer up.Close()
				s := fixtureServer()
				routeFixture(s, up.URL+"/responses")
				source := fixtureRequest()
				source["stream"] = streaming
				body := encoded(source)
				frames := forward(t, transportClient(t, s), bpsStart(body), body)
				if requests.Load() != 1 || s.host.(*fakeHost).kvWrites != 0 || frames[len(frames)-1].GetEnd() == nil {
					t.Fatal("retried, stored a tool, or broke transport")
				}
				for _, frame := range frames {
					if frame.GetError() != nil {
						t.Fatal("gRPC transport error instead of upstream event")
					}
				}
				raw := bodyOf(frames)
				for _, forbidden := range []string{"MUST_NOT_COPY_CONTEXT", "MUST_NOT_RELEASE_SCRIPT", "run_officejs", "fixture-secret-token"} {
					if bytes.Contains(raw, []byte(forbidden)) {
						t.Fatal("copied unrelated field", forbidden)
					}
				}
				d := s.lastRequest
				if d.UpstreamErrorState != "captured_raw" || !strings.Contains(d.UpstreamError, message) || d.HTTPStatus != 200 || d.OutputToolCalls != 0 || d.CompletionRecovered {
					t.Fatal("failure diagnostic incomplete")
				}
				wantSource := "upstream_stream"
				if mode == "failed_json" || mode == "error_json" {
					wantSource = "upstream_response"
				}
				if d.ErrorSource != wantSource || d.UpstreamErrorEvent == "" {
					t.Fatal("wrong error attribution", d.ErrorSource)
				}
				if (mode == "held_tool" || mode == "failed_sse" || mode == "failed_json") && d.SkippedTools != 1 {
					t.Fatal("held tool not accounted for")
				}
				if !strings.Contains(string(raw), d.ID) {
					t.Fatal("diagnostic ID lost")
				}
				checkError := func(value object) {
					if str(value, "code") != "capacity_fixture" || str(value, "param") != "input" || !strings.HasPrefix(str(value, "message"), message) || strings.Count(str(value, "message"), d.ID) != 1 {
						t.Fatal("error fields changed or diagnostic ID duplicated", value)
					}
				}
				checkResponse := func(value object) {
					if mode == "failed_sse" || mode == "failed_json" {
						if str(value, "id") != str(terminal, "id") || string(encoded(value["usage"])) != string(encoded(terminal["usage"])) || str(value, "status") != "failed" {
							t.Fatal("observed failed response identity or usage changed")
						}
					}
					checkError(value["error"].(object))
				}
				if !streaming {
					wantHTTP := 502
					if mode == "failed_sse" || mode == "failed_json" {
						wantHTTP = 200
					}
					if d.ClientHTTPStatus != wantHTTP {
						t.Fatal("wrong client HTTP", d.ClientHTTPStatus)
					}
					value, err := decodeObject(raw)
					if err != nil {
						t.Fatal(err)
					}
					checkResponse(value)
				} else {
					sequence := int64(0)
					err := readSSE(bytes.NewReader(raw), func(e object) (bool, error) {
						n, ok := e["sequence_number"].(json.Number)
						if !ok {
							t.Fatal("missing sequence")
						}
						actual, _ := n.Int64()
						if actual != sequence {
							t.Fatal("sequence gap")
						}
						sequence++
						if str(e, "type") == "response.completed" {
							t.Fatal("explicit error became success")
						}
						if str(e, "type") == "response.failed" {
							checkResponse(e["response"].(object))
						} else if str(e, "type") == "error" {
							if mode == "flat" {
								checkError(e)
							} else {
								checkResponse(e)
							}
						}
						return str(e, "type") == "error" || str(e, "type") == "response.failed", nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
