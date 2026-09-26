package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"local.sub2api/codex-inspector/internal/kv"
)

type faultKV struct {
	kv.KV
	mu   sync.Mutex
	sets map[string]int
	fail func(string, []byte, int) bool
}

func (f *faultKV) Set(ctx context.Context, ns, key string, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	if f.sets == nil {
		f.sets = map[string]int{}
	}
	f.sets[key]++
	n := f.sets[key]
	fail := f.fail != nil && f.fail(key, value, n)
	f.mu.Unlock()
	if fail {
		return errors.New("storage sentinel secret https://u:pw@proxy")
	}
	return f.KV.Set(ctx, ns, key, value, ttl)
}

func TestMarkAndCheckExecuted(t *testing.T) {
	ctx := context.Background()
	s := &Store{KV: kv.NewMemoryKV()}
	if ok, err := s.AlreadyExecuted(ctx, "t1"); ok || err != nil {
		t.Fatal("not fresh")
	}
	if err := s.MarkExecuted(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AlreadyExecuted(ctx, "t1"); !ok || err != nil {
		t.Fatal("not marked")
	}
}

func TestSaveTaskSetsLatest(t *testing.T) {
	ctx := context.Background()
	s := &Store{KV: kv.NewMemoryKV()}
	if err := s.SaveTask(ctx, Task{ID: "t2", Models: []string{"gpt-6-astra"}}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadLatest(ctx)
	if err != nil || !ok || got.ID != "t2" {
		t.Fatalf("latest: %+v %v %v", got, ok, err)
	}
}

func TestSaveTaskReturnsLatestFailure(t *testing.T) {
	f := &faultKV{KV: kv.NewMemoryKV(), fail: func(key string, _ []byte, _ int) bool { return key == "latest" }}
	s := &Store{KV: f}
	if err := s.SaveTask(context.Background(), Task{ID: "t1"}); err == nil {
		t.Fatal("swallowed latest write failure")
	}
}

func TestTaskStorageBoundsAndJSON(t *testing.T) {
	task := Task{ID: strings.Repeat("a", 64), Repeats: 10, Concurrency: 16, StartedAt: time.Now(), HeartbeatAt: time.Now(), Done: true}
	for i := range 200 {
		task.AccountIDs = append(task.AccountIDs, int64(i+1))
		task.Results = append(task.Results, PairResult{AccountID: int64(i + 1), Model: "gpt-6-astra", Verdict: "claude-haiku-4-5-20251001", Probability: 0.123456789012345, TopHits: 10, ValidRuns: 10, Failures: 10, AvgLatencyMs: 180000, Reasons: []string{"identity_not_found", "insufficient_numbers", "http_429", "network_failed", "timeout", "sse_failed", "proxy_failed", "request_failed", "identity_failed", "cancelled"}, At: time.Now()})
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= maxTaskBytes {
		t.Fatalf("200-pair task too big: %d", len(data))
	}
	if strings.Contains(string(data), "AccountID") || !strings.Contains(string(data), "avg_latency_ms") {
		t.Fatal("non-snake-case fields")
	}
	if err := (&Store{KV: kv.NewMemoryKV()}).SaveTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	t.Logf("200-pair worst-case result: %d bytes", len(data))
	task.Results[0].Reasons = []string{strings.Repeat("x", maxTaskBytes)}
	if err := (&Store{KV: kv.NewMemoryKV()}).SaveTask(context.Background(), task); err == nil {
		t.Fatal("accepted oversized record")
	}
}

func TestLoadTaskRejectsCorruptRecord(t *testing.T) {
	ctx := context.Background()
	mem := kv.NewMemoryKV()
	s := &Store{KV: mem}
	for _, raw := range []string{"broken", fmt.Sprintf("{\"id\":%q}", "different")} {
		if err := mem.Set(ctx, Namespace, "task.a", []byte(raw), time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.LoadTask(ctx, "a"); err == nil {
			t.Fatal("accepted corrupt record")
		}
	}
}
