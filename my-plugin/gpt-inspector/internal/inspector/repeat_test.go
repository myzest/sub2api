package inspector

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

func savedAnswer(t *testing.T, s *Server, account int64, batch string, item int) Answer {
	t.Helper()
	c := testCommand(s, "read_result")
	c.AccountID, c.BatchID, c.Item = account, batch, item
	var result struct{ Data string }
	if err := json.Unmarshal(invoke(t, s, c), &result); err != nil {
		t.Fatal(err)
	}
	blob, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	z, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	var answer Answer
	if err := json.NewDecoder(z).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

func TestRepeatedPromptRequestsAndResultsAreIndependent(t *testing.T) {
	for _, identical := range []bool{false, true} {
		t.Run(fmt.Sprintf("identical_output=%t", identical), func(t *testing.T) {
			var calls atomic.Int64
			sessions := make(chan string, 6)
			prompt, _ := findPrompt("iphone")
			s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				var body struct {
					Input    []struct{ Content []struct{ Text string } }
					CacheKey string `json:"prompt_cache_key"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Input) != 1 || len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Text != prompt.Text {
					t.Error("repeat changed the fixed question or reused history", err)
				}
				session := r.Header.Get("Session_id")
				if body.CacheKey != session || r.Header.Get("Conversation_id") != session {
					t.Error("request identity mismatch")
				}
				sessions <- session
				text := "相同文字回答"
				if !identical {
					text = fmt.Sprintf("独立文字回答 %d", n)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-%d\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", n, text)
			})
			seen := map[string]bool{}
			for batch := range 2 {
				c := testCommand(s, "start")
				c.Rounds = 3
				invoke(t, s, c)
				waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
				for i := range 3 {
					n := batch*3 + i + 1
					answer := savedAnswer(t, s, 7, c.ID, i)
					want := "相同文字回答"
					if !identical {
						want = fmt.Sprintf("独立文字回答 %d", n)
					}
					if answer.Text != want || answer.ResponseID != fmt.Sprintf("response-%d", n) || answer.Error != "" {
						t.Fatal("result was reused or changed", answer)
					}
				}
			}
			if calls.Load() != 6 || len(sessions) != 6 {
				t.Fatal("repeats did not send six requests", calls.Load())
			}
			for range 6 {
				session := <-sessions
				if seen[session] || !uuidPattern.MatchString(session) {
					t.Fatal("session reused", session)
				}
				seen[session] = true
			}
		})
	}
}
