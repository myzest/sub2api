package inspector

import (
	"strings"
	"testing"
)

func TestSSEFinalOutputAndStreamFailure(t *testing.T) {
	p, _ := findPrompt("iphone")
	for _, tc := range []struct {
		name, stream, want string
		failure            bool
	}{
		{"final replaces deltas", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"hello world\"}]}]}}\n\n", "hello world", false},
		{"premature done", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: [DONE]\n\n", "partial", true},
		{"broken JSON", "data: not JSON\n\n", "", true},
		{"stream error", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"part\"}\n\ndata: {\"type\":\"error\",\"message\":\"upstream failed\"}\n\n", "part", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := parseResponse(strings.NewReader(tc.stream), "text/event-stream", p)
			if a.Text != tc.want || (a.Error != "") != tc.failure {
				t.Fatalf("answer=%+v", a)
			}
		})
	}
}
func TestLargeOutputIsExplicitlyIncomplete(t *testing.T) {
	a := limitAnswer(Answer{Text: strings.Repeat("中", maxOutput/3+1)})
	if a.Error == "" || len(a.Text) > maxOutput || !strings.HasSuffix(a.Text, "中") {
		t.Fatal("invalid truncation")
	}
}
