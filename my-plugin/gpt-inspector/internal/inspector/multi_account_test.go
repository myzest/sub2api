package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

func rejectCommand(t *testing.T, s *Server, c Command, message string) {
	t.Helper()
	raw, _ := json.Marshal(Config{Command: &c})
	reply, err := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: raw})
	if err != nil || reply.Success || !strings.Contains(reply.Message, message) {
		t.Fatalf("expected rejection %q, got %v / %v", message, reply, err)
	}
}

func TestConcurrentAccountsStopAndClearIndependently(t *testing.T) {
	for _, action := range []string{"stop", "clear_account", "clear_all"} {
		t.Run(action, func(t *testing.T) {
			started, canceled := make(chan string, 4), make(chan string, 4)
			release := make(chan struct{})
			var released sync.Once
			s, h := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				account := r.Header.Get("Chatgpt-Account-Id")
				want := "Bearer fixture-access-token"
				if account == "fixture-chatgpt-account-B" {
					want += "-B"
				} else if account != "fixture-chatgpt-account" {
					t.Error("unknown account identity", account)
				}
				if r.Header.Get("Authorization") != want {
					t.Error("account identity mixed across requests")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				w.(http.Flusher).Flush()
				started <- account
				select {
				case <-r.Context().Done():
					canceled <- account
				case <-release:
					completed(w, account)
				}
			})
			defer released.Do(func() { close(release) })
			a := testCommand(s, "start")
			a.Prompts = []string{"iphone", "japan_pm"}
			b := a
			b.ID, b.AccountID = newID(), 9
			invoke(t, s, a)
			waitFor(t, func() bool { return len(started) == 2 })
			invoke(t, s, b)
			waitFor(t, func() bool { return len(started) == 4 })
			if len(getSnapshot(s).Active) != 2 {
				t.Fatal("second account did not start while first was blocked")
			}
			duplicate := a
			duplicate.ID = newID()
			rejectCommand(t, s, duplicate, "此账号已有测试批次")
			stale := testCommand(s, "stop")
			stale.BatchID = newID()
			rejectCommand(t, s, stale, "运行批次已变化")
			command := testCommand(s, action)
			command.BatchID = a.ID
			invoke(t, s, command)
			wantCanceled := 2
			if action == "clear_all" {
				wantCanceled = 4
			}
			waitFor(t, func() bool {
				return activeBatch(s, 7) == nil && getSnapshot(s).Busy == "" && len(canceled) == wantCanceled
			})
			if action != "clear_all" {
				if activeBatch(s, 9) == nil {
					t.Fatal("operation on A stopped B")
				}
				for range 2 {
					if account := <-canceled; account != "fixture-chatgpt-account" {
						t.Fatal("operation on A canceled B's request", account)
					}
				}
			}
			released.Do(func() { close(release) })
			waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
			var rec Record
			_ = json.Unmarshal(invoke(t, s, testCommand(s, "read_batch")), &rec)
			if action == "stop" {
				if rec.Latest == nil || rec.Latest.State != "interrupted" || rec.Latest.Items[0].Parts == 0 {
					t.Fatal("stop did not preserve A's partial output", rec)
				}
			} else if rec.Latest != nil || rec.Pending != nil {
				t.Fatal("cleared account came back", rec)
			}
			if action != "clear_all" {
				read := testCommand(s, "read_batch")
				read.AccountID = 9
				_ = json.Unmarshal(invoke(t, s, read), &rec)
				if rec.Latest == nil || rec.Latest.State != "completed" || rec.Pending != nil {
					t.Fatal("B did not finish independently", rec)
				}
				for i := range 2 {
					if savedAnswer(t, s, 9, b.ID, i).Text != "fixture-chatgpt-account-B" {
						t.Fatal("B's result was replaced by A's output")
					}
				}
			}
			if action != "stop" {
				h.mu.Lock()
				defer h.mu.Unlock()
				for key := range h.values {
					if action == "clear_all" || strings.HasPrefix(key, accountPrefix(7)) {
						t.Fatal("cleared result survived", key)
					}
				}
			}
		})
	}
}

func TestLegacyStopConfigLoadsButCannotStopAnyAccount(t *testing.T) {
	s, _ := testServer(t, nil)
	c := testCommand(s, "stop")
	c.AccountID = 0
	raw, _ := json.Marshal(Config{Command: &c})
	if r, err := s.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: raw}); err != nil || !r.Valid {
		t.Fatal("old saved stop config prevented upgrade", r, err)
	}
	if r, err := s.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: raw}); err != nil || !r.Applied {
		t.Fatal("old saved stop config prevented startup", r, err)
	}
	rejectCommand(t, s, c, "请选择要停止的账号批次")
}
