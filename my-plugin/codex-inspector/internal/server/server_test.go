package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"local.sub2api/codex-inspector/internal/detect"
	"local.sub2api/codex-inspector/internal/kv"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/pluginconfig"
)

func newTestServer(t *testing.T, k kv.KV) *Server {
	t.Helper()
	s := New()
	s.kv.swap(k)
	t.Cleanup(s.Close)
	return s
}

type fakeHost struct {
	pluginv1.HostServiceClient
	calls atomic.Int64
}

func (f *fakeHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	f.calls.Add(1)
	return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 1, Name: "测试账号", Schedulable: true, MetadataJson: []byte(`{"proxy":"http://name:secret@proxy.test","cookie":"hidden"}`)}}}, nil
}
func attachFakeHost(s *Server) *fakeHost {
	f := &fakeHost{}
	s.host.Store(&hostBinding{f})
	s.hostReady.Store(true)
	return f
}
func taskConfig(s *Server) pluginconfig.Config {
	c := pluginconfig.Default()
	c.Detect.TaskID = "d" + s.instance + "-test"
	c.Detect.CreatedAt = s.now().Format(time.RFC3339)
	c.Detect.AccountIDs = []int64{1}
	return c
}
func TestHealthAlwaysHealthyBeforeHostReady(t *testing.T) {
	s := New()
	defer s.Close()
	r, e := s.Health(context.Background(), &pluginv1.HealthRequest{})
	if e != nil || !r.Healthy || !strings.Contains(r.StatusJson, "degraded") {
		t.Fatal(r, e)
	}
}
func TestHealthCachesAndOmitsMetadata(t *testing.T) {
	s := newTestServer(t, kv.NewMemoryKV())
	f := attachFakeHost(s)
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		r, e := s.Health(context.Background(), nil)
		if e != nil || !r.Healthy || strings.Contains(r.StatusJson, "secret") || strings.Contains(r.StatusJson, "proxy.test") || strings.Contains(r.StatusJson, "metadata_json") {
			t.Fatal(r, e)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatal(f.calls.Load())
	}
	now = now.Add(5 * time.Second)
	_, _ = s.Health(context.Background(), nil)
	if f.calls.Load() != 2 {
		t.Fatal(f.calls.Load())
	}
}
func TestConfigValidationAndApplyHaveNoTaskSideEffects(t *testing.T) {
	mem := kv.NewMemoryKV()
	s := newTestServer(t, mem)
	attachFakeHost(s)
	c := taskConfig(s)
	for i := 0; i < 3; i++ {
		r, e := s.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: c.Marshal()})
		if e != nil || !r.Applied {
			t.Fatal(r, e)
		}
	}
	s.waitBackground()
	keys, _ := mem.List(context.Background(), "detect", "", 1000)
	if len(keys) > 0 {
		t.Fatal("Apply consumed task", keys)
	}
	bad, _ := s.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{"timezone":{"custom_tz":"Asia/Taipei"}}`)})
	if bad.Applied {
		t.Fatal("bad timezone accepted")
	}
	if s.cfg.Load().Detect.TaskID != c.Detect.TaskID {
		t.Fatal("invalid config replaced valid config")
	}
}
func TestDetectRejectsStaleAndOtherInstance(t *testing.T) {
	s := newTestServer(t, kv.NewMemoryKV())
	attachFakeHost(s)
	for _, offset := range []time.Duration{-40 * time.Minute, 40 * time.Minute} {
		c := taskConfig(s)
		c.Detect.CreatedAt = s.now().Add(offset).Format(time.RFC3339)
		r, e := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: c.Marshal()})
		if e != nil || r.Success {
			t.Fatal(r, e)
		}
	}
	c := taskConfig(s)
	c.Detect.TaskID = "doldinstance-test"
	r, _ := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: c.Marshal()})
	if r.Success {
		t.Fatal("old process command accepted")
	}
	keys, _ := s.kv.List(context.Background(), "detect", "", 1000)
	if len(keys) != 0 {
		t.Fatal(keys)
	}
}
func TestDetectExplicitCommandDeduplicatesAndPreservesScope(t *testing.T) {
	s := newTestServer(t, kv.NewMemoryKV())
	attachFakeHost(s)
	started := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int64
	s.runTask = func(ctx context.Context, task detect.Task) {
		calls.Add(1)
		close(started)
		select {
		case <-finish:
		case <-ctx.Done():
			return
		}
		store := &detect.Store{KV: s.kv}
		_ = store.MarkExecuted(ctx, task.ID)
		task.Done = true
		task.HeartbeatAt = s.now()
		_ = store.SaveTask(ctx, task)
	}
	c := taskConfig(s)
	r, e := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: c.Marshal()})
	if e != nil || !r.Success {
		t.Fatal(r, e)
	}
	<-started
	r, e = s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: c.Marshal()})
	if e != nil || !r.Success {
		t.Fatal(r, e)
	}
	other := c
	other.Detect.TaskID += "2"
	r, _ = s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: other.Marshal()})
	if r.Success {
		t.Fatal("parallel batch accepted")
	}
	close(finish)
	s.waitBackground()
	r, _ = s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: c.Marshal()})
	if !r.Success || calls.Load() != 1 {
		t.Fatal("task replay", r, calls.Load())
	}
	if _, ok, _ := s.kv.Get(context.Background(), "detect", "lease"); ok {
		t.Fatal("hook should not acquire lease")
	}
	h, _ := s.Health(context.Background(), nil)
	var status map[string]any
	if json.Unmarshal([]byte(h.StatusJson), &status) != nil || status["detect"].(map[string]any)["state"] != "done" {
		t.Fatal(h.StatusJson)
	}
}
func TestDetectState(t *testing.T) {
	now := time.Now()
	for i, tc := range []struct {
		task detect.Task
		want string
	}{{detect.Task{Done: true}, "done"}, {detect.Task{HeartbeatAt: now.Add(-30 * time.Second)}, "running"}, {detect.Task{HeartbeatAt: now.Add(-91 * time.Second)}, "interrupted"}, {detect.Task{}, "interrupted"}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if got := detectState(tc.task, now); got != tc.want {
				t.Fatal(got)
			}
		})
	}
}
