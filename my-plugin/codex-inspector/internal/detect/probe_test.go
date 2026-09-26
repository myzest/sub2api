package detect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func sseData(value any) string { raw, _ := json.Marshal(value); return "data: " + string(raw) + "\n\n" }
func completedSSE(text string) string {
	return sseData(map[string]string{"type": "response.output_text.delta", "delta": text}) + sseData(map[string]string{"type": "response.completed"})
}

func TestParseSSE(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		ok                bool
	}{
		{"delta", sseData(map[string]string{"type": "response.output_text.delta", "delta": "12 "}) + completedSSE("34"), "12 34", true},
		{"failed", sseData(map[string]string{"type": "response.failed"}), "", false},
		{"no completed", sseData(map[string]string{"type": "response.output_text.delta", "delta": "1"}), "", false},
		{"error", sseData(map[string]string{"type": "error"}), "", false},
		{"incomplete", sseData(map[string]string{"type": "response.incomplete"}), "", false},
		{"malformed", "data: {not json}\n\n", "", false},
		{"done only", "data: [DONE]\n\n", "", false},
		{"crlf comments", ": ping\r\nevent: anything\r\n" + strings.ReplaceAll(completedSSE("1 2"), "\n", "\r\n"), "1 2", true},
		{"multiline", "data: {\"type\":\"response.output_text.delta\",\n" + "data: \"delta\":\"1 2\"}\n\n" + sseData(map[string]string{"type": "response.completed"}), "1 2", true},
		{"fallback", "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"1 2\"},{\"type\":\"refusal\",\"text\":\"ignored\"}]}]}}\n\n", "1 2", true},
		{"failed status", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n", "", false},
		{"final event without newline", strings.TrimSpace(completedSSE("1 2")), "1 2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSSE(strings.NewReader(tc.input))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q %v want %q %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseSSELimit(t *testing.T) {
	for _, input := range []string{
		completedSSE(strings.Repeat("1", maxSSEBytes)),
		strings.Repeat(": heartbeat\n", maxSSEBytes/10) + completedSSE("1"),
		"data: " + strings.Repeat(" ", maxSSEBytes+10),
	} {
		if _, ok := parseSSE(strings.NewReader(input)); ok {
			t.Fatal("accepted oversized stream")
		}
	}
}

func TestBuildProbeRaisesAstraVersion(t *testing.T) {
	id := Identity{Token: "secret", Headers: http.Header{"Version": {"0.150.0"}, "User-Agent": {"codex-tui/0.150.0"}, "Chatgpt-Account-Id": {"account"}, "Cookie": {"secret-cookie"}}}
	req, err := buildProbeRequest(context.Background(), CodexResponsesURL, id, "gpt-6-astra", "q \"quoted\"")
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Version") != "0.153.4" || !strings.Contains(req.Header.Get("User-Agent"), "codex-tui/0.153.4") {
		t.Fatalf("bad version: %v", req.Header)
	}
	if id.Headers.Get("Version") != "0.150.0" {
		t.Fatal("mutated identity")
	}
	if req.Header.Get("Authorization") != "Bearer secret" || req.Header.Get("Chatgpt-Account-Id") != "account" || req.Header.Get("Cookie") != "" {
		t.Fatal("identity/header mismatch")
	}
	data, _ := io.ReadAll(req.Body)
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["instructions"]; ok {
		t.Fatal("unexpected instructions")
	}
	if len(body) != 4 || body["store"] != false || body["stream"] != true || body["model"] != "gpt-6-astra" {
		t.Fatalf("unexpected body %s", data)
	}
}

func TestBuildProbePreservesNewerVersion(t *testing.T) {
	id := Identity{Token: "t", Headers: http.Header{"Version": {"0.200.1"}, "User-Agent": {"codex-tui/0.200.1 (OS)"}}}
	req, err := buildProbeRequest(context.Background(), CodexResponsesURL, id, "gpt-6-astra", "q")
	if err != nil || req.Header.Get("Version") != "0.200.1" || req.Header.Get("User-Agent") != "codex-tui/0.200.1 (OS)" {
		t.Fatal("lowered newer version")
	}
}
