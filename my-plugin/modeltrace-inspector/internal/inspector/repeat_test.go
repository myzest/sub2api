package inspector

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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

func TestRepeatedRequestsAndResultsAreIndependent(t *testing.T) {
	for _, identical := range []bool{false, true} {
		t.Run(fmt.Sprintf("identical_output=%t", identical), func(t *testing.T) {
			var calls atomic.Int64
			sessions := make(chan string, 12)
			s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				var body struct {
					Input    []struct{ Content []struct{ Text string } }
					CacheKey string `json:"prompt_cache_key"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Input) != 1 || len(body.Input[0].Content) != 1 || !strings.Contains(body.Input[0].Content[0].Text, "355") {
					t.Error("request did not contain an independent numerical challenge", err)
				}
				session := r.Header.Get("Session_id")
				if body.CacheKey != session || r.Header.Get("Conversation_id") != session {
					t.Error("request identity mismatch")
				}
				sessions <- session
				seed := int(n)
				if identical {
					seed = 1
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-%d\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", n, fixtureNumbers(seed))
			})
			seen := map[string]bool{}
			responses := map[string]bool{}
			for range 2 {
				c := testCommand(s, "start")
				c.Rounds = 2
				invoke(t, s, c)
				waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
				for i := range 6 {
					answer := savedAnswer(t, s, 7, c.ID, i)
					n, err := strconv.Atoi(strings.TrimPrefix(answer.ResponseID, "response-"))
					if err != nil || responses[answer.ResponseID] {
						t.Fatal("response was reused", answer.ResponseID)
					}
					responses[answer.ResponseID] = true
					if identical {
						n = 1
					}
					if answer.Text != fixtureNumbers(n) || answer.Error != "" || answer.Quality == nil || !answer.Quality.Accepted {
						t.Fatal("result was reused, changed, or not inspected", answer)
					}
				}
			}
			if calls.Load() != 12 || len(sessions) != 12 {
				t.Fatal("rounds did not send twelve requests", calls.Load())
			}
			for range 12 {
				session := <-sessions
				if seen[session] || !uuidPattern.MatchString(session) {
					t.Fatal("session reused", session)
				}
				seen[session] = true
			}
		})
	}
}
