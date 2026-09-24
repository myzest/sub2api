package inspector

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

func TestLargeResultChunksReassembleExactly(t *testing.T) {
	s, h := testServer(t, nil)
	noise := make([]byte, 700*1024)
	rand.Read(noise)
	content := base64.StdEncoding.EncodeToString(noise)
	b := &Batch{ID: newID(), Selection: Selection{AccountID: 7}, Items: []Item{{PromptID: "pelican", State: "completed"}}}
	p, _ := findPrompt("pelican")
	parts, hash, err := s.saveAnswerLocked(b, 0, Answer{Prompt: p, Text: content})
	if err != nil || parts < 2 {
		t.Fatal("expected multiple chunks", parts, err)
	}
	b.Items[0].Parts, b.Items[0].SHA256 = parts, hash
	s.records[7] = &Record{AccountID: 7, Latest: b}
	c := testCommand(s, "read_result")
	c.BatchID = b.ID
	value, err := s.readResult(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(value.(map[string]any)["data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	z, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(z)
	z.Close()
	if err != nil {
		t.Fatal(err)
	}
	var result Answer
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Text != content {
		t.Fatal("large result changed during chunking")
	}
	for _, v := range h.values {
		if len(v) > 256*1024 {
			t.Fatal("host KV limit exceeded")
		}
	}
}

func TestKVListingAndClearBeyondOnePage(t *testing.T) {
	s, h := testServer(t, nil)
	for i := 0; i < 1503; i++ {
		h.values[fmt.Sprintf("a.%d.meta", i+1)] = []byte(`{}`)
	}
	if err := s.store.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.store.sizes) != 1503 {
		t.Fatalf("only loaded %d keys", len(s.store.sizes))
	}
	invoke(t, s, testCommand(s, "clear_all"))
	waitFor(t, func() bool { return getSnapshot(s).Busy == "" })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.values) != 0 || getSnapshot(s).Bytes != 0 {
		t.Fatal("clear only deleted first page")
	}
}
func TestRecoveryRetainsInterruptedResultsAndDropsOrphans(t *testing.T) {
	s, h := testServer(t, nil)
	batchIDs := map[int64]string{7: newID(), 9: newID()}
	for id, batch := range batchIDs {
		oldID := newID()
		r := Record{AccountID: id, Name: "fixture", Latest: &Batch{ID: oldID, State: "completed"}, Pending: &Batch{ID: batch, State: "running", Items: []Item{{PromptID: "iphone", State: "completed", Parts: 1}, {PromptID: "japan_pm", State: "running"}}}}
		h.values[recordKey(id)], _ = json.Marshal(r)
		h.values[resultKey(id, oldID, 0, 0)] = []byte("old result")
		h.values[resultKey(id, batch, 0, 0)] = []byte("committed result")
		h.values[resultKey(id, batch, 1, 0)] = []byte("uncommitted partial write")
	}
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, batch := range batchIDs {
		rr := s.records[id]
		if rr.Latest.State != "interrupted" || rr.Pending != nil || rr.Latest.Items[1].State != "interrupted" || h.values[resultKey(id, batch, 0, 0)] == nil {
			t.Fatal("recovery mixed accounts or lost their committed answers", rr)
		}
	}
	if len(h.values) != 4 {
		t.Fatal("recovery kept orphans")
	}
}
func TestClearTombstoneFinishesAfterRestart(t *testing.T) {
	s, h := testServer(t, nil)
	h.values["control.clear"] = []byte("0")
	h.values["a.7.meta"] = []byte("corrupt data")
	h.values["a.7.b.orphan"] = []byte("blob")
	if err := s.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.values) != 0 {
		t.Fatal("pending clear was not recovered")
	}
}
func TestCorruptResultNeverDisplaysPartialHTML(t *testing.T) {
	s, _ := testServer(t, nil)
	id := newID()
	r := &Record{AccountID: 7, Latest: &Batch{ID: id, Items: []Item{{Parts: 1, SHA256: strings.Repeat("0", 64)}}}}
	s.records[7] = r
	if err := s.store.set(context.Background(), resultKey(7, id, 0, 0), []byte("bad")); err != nil {
		t.Fatal(err)
	}
	c := testCommand(s, "read_result")
	c.BatchID = id
	if _, err := s.readResult(context.Background(), c); err == nil {
		t.Fatal("corrupt result accepted")
	}
}

func TestFailedFinalCommitCannotBeOverwrittenByNewBatch(t *testing.T) {
	s, h := testServer(t, func(w http.ResponseWriter, r *http.Request) { completed(w, "saved answer") })
	old := testCommand(s, "start")
	invoke(t, s, old)
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	latest := testCommand(s, "start")
	h.mu.Lock()
	h.beforeSet = func(req *pluginv1.KVSetRequest) error {
		var rec Record
		if req.Key == recordKey(7) && json.Unmarshal(req.Value, &rec) == nil && rec.Pending == nil && rec.Latest != nil && rec.Latest.ID == latest.ID {
			return fmt.Errorf("fixture final commit failure")
		}
		return nil
	}
	h.mu.Unlock()
	invoke(t, s, latest)
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	if !strings.Contains(getSnapshot(s).Error, "提交最新批次失败") {
		t.Fatal("storage failure was hidden")
	}
	h.mu.Lock()
	h.beforeSet = nil
	h.mu.Unlock()
	next := testCommand(s, "start")
	raw, _ := json.Marshal(Config{Command: &next})
	reply, err := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: raw})
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	if err != nil || reply.Success {
		t.Fatal("new batch overwrote the uncommitted result", reply, err)
	}
	for _, batchID := range []string{old.ID, latest.ID} {
		read := testCommand(s, "read_result")
		read.BatchID = batchID
		invoke(t, s, read)
	}
	// Restart promotes the persisted pending batch and reclaims the previous one.
	restarted := New()
	restarted.host = h
	restarted.store = &store{host: h, sizes: map[string]int64{}}
	restarted.bootstrap()
	if !getSnapshot(restarted).Ready || restarted.records[7].Latest.ID != latest.ID || restarted.records[7].Pending != nil {
		t.Fatal("failed commit was not recovered", getSnapshot(restarted))
	}
	for key := range h.values {
		if strings.HasPrefix(key, batchPrefix(7, old.ID)) {
			t.Fatal("old batch survived recovery")
		}
	}
}
