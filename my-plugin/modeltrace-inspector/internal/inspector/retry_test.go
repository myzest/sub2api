package inspector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func brokenStream(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)+100))
	fmt.Fprint(w, payload) // Closing before Content-Length produces unexpected EOF.
}

func TestEmptyStreamRetriesFreshSessionAndSavesHistory(t *testing.T) {
	var calls atomic.Int64
	sessions := make(chan string, 6)
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		sessions <- r.Header.Get("Session_id")
		if calls.Add(1) == 1 {
			brokenStream(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"first-response\"}}\n\n")
			return
		}
		completed(w, fixtureNumbers(7))
	})
	c := testCommand(s, "start")
	invoke(t, s, c)
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	if calls.Load() != 4 {
		t.Fatal("expected three probes and one retry", calls.Load())
	}
	rec := readRecord(t, s, 7)
	retryItem := -1
	for i, item := range rec.Latest.Items {
		if item.Attempt == 2 {
			retryItem = i
		}
	}
	if retryItem < 0 {
		t.Fatal("retry history missing from index")
	}
	a := savedAnswer(t, s, 7, c.ID, retryItem)
	if a.Text != fixtureNumbers(7) || a.Error != "" || len(a.Attempts) != 2 {
		t.Fatalf("empty stream was not recovered: %+v", a)
	}
	first, second := a.Attempts[0], a.Attempts[1]
	if first.SessionID == second.SessionID || a.SessionID != second.SessionID || rec.Latest.Items[retryItem].SessionID != second.SessionID {
		t.Fatal("retry reused session or lost request identity", a)
	}
	if first.ResponseID != "first-response" || first.ErrorCode != "stream_interrupted" || !strings.Contains(first.Error, "unexpected EOF") || second.Error != "" {
		t.Fatal("attempt history hid the interrupted request", a.Attempts)
	}
	seen := map[string]bool{}
	for range 4 {
		id := <-sessions
		if seen[id] {
			t.Fatal("session reused across probes or retry")
		}
		seen[id] = true
	}
}

func TestEmptyStreamRetryLimit(t *testing.T) {
	var calls atomic.Int64
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		brokenStream(w, ": keepalive\n\n")
	})
	c := testCommand(s, "start")
	a := s.ask(context.Background(), c.Selection, fixturePrompt(), newID(), nil)
	if calls.Load() != 3 || len(a.Attempts) != 3 || a.ErrorCode != "stream_interrupted" || a.Text != "" {
		t.Fatalf("retry limit lost: calls=%d answer=%+v", calls.Load(), a)
	}
}

func TestPartialAndNonTransientFailuresAreNotReplayed(t *testing.T) {
	for _, tc := range []struct {
		name, event, wantText, wantReasoning, code string
	}{
		{"partial text", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"已收到的文字\"}\n\n", "已收到的文字", "", "stream_interrupted"},
		{"partial reasoning", "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"已收到的摘要\"}\n\n", "", "已收到的摘要", "stream_interrupted"},
		{"partial done snapshot", "data: {\"type\":\"response.output_text.done\",\"text\":\"已收到的全文\"}\n\n", "已收到的全文", "", "stream_interrupted"},
		{"explicit failure", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"upstream failure\"}}}\n\n", "", "", "upstream_error"},
		{"invalid event", "data: invalid JSON\n\n", "", "", "invalid_event"},
		{"oversized event", "data: " + strings.Repeat("x", maxEventSize+1), "", "", "event_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.HasPrefix(tc.name, "partial") {
					brokenStream(w, tc.event)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, tc.event)
				}
			})
			c := testCommand(s, "start")
			a := s.ask(context.Background(), c.Selection, fixturePrompt(), newID(), nil)
			if calls.Load() != 1 || a.Text != tc.wantText || a.Reasoning != tc.wantReasoning || a.ErrorCode != tc.code {
				t.Fatalf("partial answer was discarded or nontransient failure replayed: calls=%d answer=%+v", calls.Load(), a)
			}
		})
	}
}

func TestRetryBackoffHonorsCancellationAndParentDeadline(t *testing.T) {
	var calls atomic.Int64
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		brokenStream(w, ": waiting\n\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c := testCommand(s, "start")
	started := time.Now()
	a := s.ask(ctx, c.Selection, fixturePrompt(), newID(), nil)
	if calls.Load() != 1 || len(a.Attempts) != 1 || a.ErrorCode != "canceled" || time.Since(started) > time.Second {
		t.Fatalf("backoff ignored cancellation: calls=%d answer=%+v", calls.Load(), a)
	}
}
