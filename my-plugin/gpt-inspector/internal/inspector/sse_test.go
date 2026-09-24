package inspector

import (
	"io"
	"strings"
	"testing"
)

type endingErrorReader struct {
	text string
	err  error
}

func (r *endingErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.text)
	r.text = r.text[n:]
	if r.text == "" {
		return n, r.err
	}
	return n, nil
}

func TestSSEReadFailuresAndCompletionBoundary(t *testing.T) {
	p, _ := findPrompt("iphone")
	created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-before-break\",\"model\":\"fixture-model\"}}\n\n"
	complete := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"完整回答\"}]}]}}"
	for _, tc := range []struct {
		name, stream, wantText, errorContains string
	}{
		{"completed before socket closes", created + complete, "完整回答", ""},
		{"empty stream interruption", created, "", "unexpected EOF"},
		{"partial output interruption", created + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"部分回答\"}\n\n", "部分回答", "unexpected EOF"},
		{"event truncated by disconnect", created + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"cut", "", "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := parseResponse(&endingErrorReader{text: tc.stream, err: io.ErrUnexpectedEOF}, "text/event-stream", p)
			if a.Text != tc.wantText || a.ResponseID != "resp-before-break" || a.ReturnedModel != "fixture-model" {
				t.Fatalf("response metadata or content lost: %+v", a)
			}
			if tc.errorContains == "" && a.Error != "" || tc.errorContains != "" && !strings.Contains(a.Error, tc.errorContains) {
				t.Fatalf("wrong failure classification: %q", a.Error)
			}
		})
	}
}

func TestSSELimitsDistinguishEventSizeFromLongStream(t *testing.T) {
	p, _ := findPrompt("iphone")
	oversized := parseResponse(strings.NewReader("data: "+strings.Repeat("x", 16*1024*1024+1)), "text/event-stream", p)
	if !strings.Contains(oversized.Error, "16 MiB") || strings.Contains(oversized.Error, "或") {
		t.Fatalf("event size failure is ambiguous: %q", oversized.Error)
	}
	// A long stream of small keepalives does not consume answer storage.
	keepalives := strings.Repeat(":"+strings.Repeat(" ", 1021)+"\n\n", 32769)
	complete := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"finished\"}]}]}}\n\n"
	a := parseResponse(io.MultiReader(strings.NewReader(keepalives), strings.NewReader(complete)), "text/event-stream", p)
	if a.Error != "" || a.Text != "finished" {
		t.Fatalf("valid long stream was cut off: %+v", a)
	}
}

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

func TestSSEDoneSnapshotsPreserveContentWithoutDuplicatingDeltas(t *testing.T) {
	p, _ := findPrompt("pelican")
	stream := "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n" +
		"data: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":\"complete A\"}\n\n" +
		"data: {\"type\":\"response.content_part.done\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"complete A\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"content\":[{\"type\":\"output_text\",\"text\":\"complete A\"}]}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"content\":[{\"type\":\"output_text\",\"text\":\"complete B\"}],\"summary\":[{\"type\":\"summary_text\",\"text\":\"summary\"}]}}\n\n"
	a := parseResponse(strings.NewReader(stream), "text/event-stream", p)
	if a.Text != "complete Acomplete B" || a.Reasoning != "summary" || a.ErrorCode != "stream_incomplete" {
		t.Fatalf("done snapshots were lost or duplicated: %+v", a)
	}
}
