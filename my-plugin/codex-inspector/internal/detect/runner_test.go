package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"local.sub2api/codex-inspector/internal/diag"
	"local.sub2api/codex-inspector/internal/kv"
	"local.sub2api/codex-inspector/internal/modeltrace"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/transport"
)

type fakeHost struct {
	accounts []*pluginv1.AccountInfo
	resolve  func(context.Context, int64) (*pluginv1.ResolveOutboundIdentityResponse, error)
	resolves atomic.Int32
}

func (f *fakeHost) ListAccounts(ctx context.Context, req *pluginv1.ListAccountsRequest, _ ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	if req.Platform != "openai" || req.AccountType != "oauth" {
		return nil, fmt.Errorf("invalid account query")
	}
	return &pluginv1.ListAccountsResponse{Accounts: f.accounts}, ctx.Err()
}
func (f *fakeHost) ResolveOutboundIdentity(ctx context.Context, req *pluginv1.ResolveOutboundIdentityRequest, _ ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	f.resolves.Add(1)
	if f.resolve != nil {
		return f.resolve(ctx, req.AccountId)
	}
	return &pluginv1.ResolveOutboundIdentityResponse{Found: true, AccountId: req.AccountId, Platform: "openai", AccountType: "oauth", Token: "TEST-SECRET", Headers: map[string]*pluginv1.HeaderValues{"Chatgpt-Account-Id": {Values: []string{strconv.FormatInt(req.AccountId, 10)}}}}, nil
}
func testAccount(id int64) *pluginv1.AccountInfo {
	return &pluginv1.AccountInfo{Id: id, Platform: "openai", AccountType: "oauth", Schedulable: true, Status: "active"}
}
func testTask() Task {
	return Task{ID: "task-1", CreatedAt: time.Now().Format(time.RFC3339), AccountIDs: []int64{1}, Models: []string{"gpt-6-astra"}, Repeats: 2, Concurrency: 1}
}
func testNumbers() string {
	rng := rand.New(rand.NewSource(19))
	values := make([]string, 300)
	for i := range values {
		values[i] = strconv.Itoa(rng.Intn(355) + 1)
	}
	return strings.Join(values, " ")
}

func newTestRunner(t *testing.T, handler http.HandlerFunc) *Runner {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	mem := kv.NewMemoryKV()
	pool := transport.NewPool()
	t.Cleanup(pool.CloseIdle)
	return &Runner{Store: &Store{KV: mem}, Lease: kv.Lease{KV: mem, NS: Namespace, Key: "lease", Jitter: func() time.Duration { return 0 }}, Pool: pool, Bank: modeltrace.LoadBank(), Host: &fakeHost{accounts: []*pluginv1.AccountInfo{testAccount(1)}}, Diag: diag.New("test", nil), Holder: "test-holder", URL: server.URL, ProbeTimeout: 2 * time.Second, RenewEvery: 20 * time.Millisecond}
}
func loadTask(t *testing.T, r *Runner) Task {
	t.Helper()
	task, ok, err := r.Store.LoadTask(context.Background(), "task-1")
	if !ok || err != nil {
		t.Fatalf("task missing: %v %v", ok, err)
	}
	return task
}

func TestRunnerNotLeaderDoesNothing(t *testing.T) {
	r := newTestRunner(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	if err := r.Store.KV.Set(context.Background(), Namespace, "lease", []byte("other"), time.Hour); err != nil {
		t.Fatal(err)
	}
	r.Run(context.Background(), testTask())
	if _, ok, _ := r.Store.LoadTask(context.Background(), "task-1"); ok {
		t.Fatal("task written as nonleader")
	}
	if ok, _ := r.Store.AlreadyExecuted(context.Background(), "task-1"); ok {
		t.Fatal("nonleader consumed task")
	}
}

func TestRunnerAlreadyExecutedRecheckedUnderLease(t *testing.T) {
	r := newTestRunner(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	if err := r.Store.MarkExecuted(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
	r.Run(context.Background(), testTask())
	if _, ok, _ := r.Store.LoadTask(context.Background(), "task-1"); ok {
		t.Fatal("duplicate execution wrote task")
	}
	if r.Host.(*fakeHost).resolves.Load() != 0 {
		t.Fatal("duplicate resolved credentials")
	}
}

func TestRunnerProbesAndScores(t *testing.T) {
	var calls atomic.Int32
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completedSSE(testNumbers()))
	})
	r.Run(context.Background(), testTask())
	task := loadTask(t, r)
	if !task.Done || len(task.Results) != 1 || task.Results[0].ValidRuns != 2 || task.Results[0].Failures != 0 || task.Results[0].Verdict == "" {
		t.Fatalf("bad task: %+v", task)
	}
	if calls.Load() != 2 {
		t.Fatalf("called %d times", calls.Load())
	}
	if ok, _ := r.Store.AlreadyExecuted(context.Background(), "task-1"); !ok {
		t.Fatal("marker missing")
	}
	r.Run(context.Background(), testTask())
	if calls.Load() != 2 {
		t.Fatal("repeated request")
	}
}

func TestRunnerSkipsAccountOnFatalStatus(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				fmt.Fprint(w, "TEST-SECRET upstream confidential body")
			})
			task := testTask()
			task.Models = append(task.Models, "gpt-6-sol")
			r.Run(context.Background(), task)
			got := loadTask(t, r)
			if calls.Load() != 1 || !got.Done || len(got.Results) != 2 {
				t.Fatalf("skip failed: calls %d %+v", calls.Load(), got)
			}
			for _, p := range got.Results {
				if len(p.Reasons) != 1 || p.Reasons[0] != "http_"+strconv.Itoa(status) || p.Failures != 2 {
					t.Fatalf("bad result %+v", p)
				}
			}
			raw, _ := json.Marshal(r.Diag.Events())
			saved, _ := json.Marshal(got)
			if strings.Contains(string(raw)+string(saved), "TEST-SECRET") {
				t.Fatal("leaked secret/body")
			}
		})
	}
}

func TestRunnerSkipsUnschedulableAndMissingAccounts(t *testing.T) {
	r := newTestRunner(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	h := r.Host.(*fakeHost)
	h.accounts[0].Schedulable = false
	task := testTask()
	task.AccountIDs = []int64{1, 2}
	task.Concurrency = 2
	r.Run(context.Background(), task)
	got := loadTask(t, r)
	if !got.Done || len(got.Results) != 2 || got.Results[0].Reasons[0] != "account_unschedulable" || got.Results[1].Reasons[0] != "account_not_found" || h.resolves.Load() != 0 {
		t.Fatalf("wrong skips %+v", got)
	}
}

func TestRunnerMissingIdentitySkipsRemainingModels(t *testing.T) {
	r := newTestRunner(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	h := r.Host.(*fakeHost)
	h.resolve = func(context.Context, int64) (*pluginv1.ResolveOutboundIdentityResponse, error) {
		return &pluginv1.ResolveOutboundIdentityResponse{Found: false}, nil
	}
	task := testTask()
	task.Models = append(task.Models, "gpt-6-sol")
	r.Run(context.Background(), task)
	got := loadTask(t, r)
	if !got.Done || len(got.Results) != 2 || h.resolves.Load() != 1 {
		t.Fatalf("wrong skip %+v", got)
	}
	for _, p := range got.Results {
		if p.Reasons[0] != "identity_not_found" {
			t.Fatalf("wrong reason %+v", p)
		}
	}
}

func TestRunnerRejectsUnusableOutput(t *testing.T) {
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, completedSSE("1 2 3")) })
	r.Run(context.Background(), testTask())
	p := loadTask(t, r).Results[0]
	if p.ValidRuns != 0 || p.Failures != 2 || p.Verdict != "" || len(p.Reasons) != 1 || p.Reasons[0] != "insufficient_numbers" {
		t.Fatalf("wrong validity %+v", p)
	}
}

func TestRunnerDeduplicatesAndSerializesAccounts(t *testing.T) {
	var mu sync.Mutex
	live := map[string]int{}
	maxPer := 0
	total, maxTotal, calls := 0, 0, 0
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		id := req.Header.Get("Chatgpt-Account-Id")
		mu.Lock()
		live[id]++
		total++
		calls++
		maxPer = max(maxPer, live[id])
		maxTotal = max(maxTotal, total)
		mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		fmt.Fprint(w, completedSSE(testNumbers()))
		mu.Lock()
		live[id]--
		total--
		mu.Unlock()
	})
	r.Host.(*fakeHost).accounts = append(r.Host.(*fakeHost).accounts, testAccount(2))
	task := testTask()
	task.AccountIDs = []int64{1, 1, 2}
	task.Models = []string{"gpt-6-astra", "gpt-6-astra", "gpt-6-sol"}
	task.Concurrency = 3
	r.Run(context.Background(), task)
	got := loadTask(t, r)
	mu.Lock()
	defer mu.Unlock()
	if !got.Done || len(got.Results) != 4 || calls != 8 || maxPer != 1 || maxTotal < 2 {
		t.Fatalf("concurrency: done=%v results=%d calls=%d maxPer=%d maxTotal=%d", got.Done, len(got.Results), calls, maxPer, maxTotal)
	}
}

func TestRunnerStorageFailureStopsWithoutRequests(t *testing.T) {
	for _, key := range []string{"task.task-1", "executed.task-1", "latest"} {
		t.Run(key, func(t *testing.T) {
			r := newTestRunner(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
			f := &faultKV{KV: r.Store.KV, fail: func(k string, _ []byte, _ int) bool { return k == key }}
			r.Store.KV = f
			r.Lease.KV = f
			r.Run(context.Background(), testTask())
			if r.Host.(*fakeHost).resolves.Load() != 0 {
				t.Fatal("resolved despite failed storage")
			}
			if key != "executed.task-1" {
				if ok, _ := r.Store.AlreadyExecuted(context.Background(), "task-1"); ok {
					t.Fatal("initial write failure consumed marker")
				}
			}
			raw, _ := json.Marshal(r.Diag.Events())
			if strings.Contains(string(raw), "sentinel") || strings.Contains(string(raw), "pw@") {
				t.Fatal("raw storage error leaked")
			}
		})
	}
}

func TestRunnerProgressFailureLeavesInterrupted(t *testing.T) {
	var calls atomic.Int32
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, completedSSE(testNumbers()))
	})
	r.RenewEvery = time.Second
	f := &faultKV{KV: r.Store.KV, fail: func(k string, _ []byte, n int) bool { return k == "task.task-1" && n > 1 }}
	r.Store.KV = f
	r.Lease.KV = f
	task := testTask()
	task.Models = append(task.Models, "gpt-6-sol")
	r.Run(context.Background(), task)
	got := loadTask(t, r)
	if got.Done || calls.Load() != 2 {
		t.Fatalf("continued after failed progress: %+v calls=%d", got, calls.Load())
	}
}

func TestRenewFailsWhenLeaseTaken(t *testing.T) {
	r := newTestRunner(t, func(http.ResponseWriter, *http.Request) {})
	if err := r.Store.KV.Set(context.Background(), Namespace, "lease", []byte("other"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if r.renew(context.Background()) {
		t.Fatal("renewed another holder")
	}
}

func TestRunnerLeaseLossCancelsProbeAndStopsRenewal(t *testing.T) {
	started := make(chan struct{})
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		close(started)
		select {
		case <-req.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	f := &faultKV{KV: r.Store.KV}
	r.Store.KV = f
	r.Lease.KV = f
	done := make(chan struct{})
	go func() { r.Run(context.Background(), testTask()); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	if err := f.Set(context.Background(), Namespace, "lease", []byte("other"), time.Hour); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel")
	}
	if task := loadTask(t, r); task.Done {
		t.Fatal("lost lease marked done")
	}
	f.mu.Lock()
	before := f.sets["task.task-1"]
	f.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	f.mu.Lock()
	after := f.sets["task.task-1"]
	f.mu.Unlock()
	if before != after {
		t.Fatal("renewal goroutine wrote after Run returned")
	}
}

func TestRunnerCallerCancellationNeverMarksDone(t *testing.T) {
	started := make(chan struct{})
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		close(started)
		select {
		case <-req.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx, testTask()); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run failed to stop")
	}
	if got := loadTask(t, r); got.Done {
		t.Fatal("cancelled task marked done")
	}
}

func TestRunnerUsesAccountProxy(t *testing.T) {
	var calls atomic.Int32
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Host != "probe.invalid" || req.Header.Get("Authorization") != "Bearer TEST-SECRET" {
			t.Error("probe did not traverse account proxy with outbound identity")
		}
		fmt.Fprint(w, completedSSE(testNumbers()))
	})
	proxyURL := r.URL
	r.URL = "http://probe.invalid/responses"
	r.Host.(*fakeHost).resolve = func(ctx context.Context, id int64) (*pluginv1.ResolveOutboundIdentityResponse, error) {
		return &pluginv1.ResolveOutboundIdentityResponse{Found: true, AccountId: id, Platform: "openai", AccountType: "oauth", Token: "TEST-SECRET", ProxyUrl: proxyURL}, nil
	}
	r.Run(context.Background(), testTask())
	if got := loadTask(t, r); !got.Done || got.Results[0].ValidRuns != 2 || calls.Load() != 2 {
		t.Fatalf("proxy path failed: %+v calls=%d", got, calls.Load())
	}
}

func TestRunnerBadProxyErrorContainsOnlyFixedCode(t *testing.T) {
	r := newTestRunner(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	r.Host.(*fakeHost).resolve = func(ctx context.Context, id int64) (*pluginv1.ResolveOutboundIdentityResponse, error) {
		return &pluginv1.ResolveOutboundIdentityResponse{Found: true, AccountId: id, Platform: "openai", AccountType: "oauth", Token: "TEST-SECRET", ProxyUrl: "bad://user:PASSWORD@host"}, nil
	}
	r.Run(context.Background(), testTask())
	got := loadTask(t, r)
	if got.Results[0].Reasons[0] != "proxy_failed" {
		t.Fatalf("wrong reason: %+v", got)
	}
	data, _ := json.Marshal(r.Diag.Events())
	saved, _ := json.Marshal(got)
	for _, sensitive := range []string{"PASSWORD", "TEST-SECRET", "user:", "bad://"} {
		if strings.Contains(string(data)+string(saved), sensitive) {
			t.Fatal("proxy/token leaked")
		}
	}
}

func TestRunnerRenewsLeaseDuringSingleProbe(t *testing.T) {
	started, release, renewed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startOnce, renewOnce sync.Once
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
			fmt.Fprint(w, completedSSE(testNumbers()))
		case <-req.Context().Done():
		}
	})
	f := &faultKV{KV: r.Store.KV, fail: func(key string, _ []byte, n int) bool {
		if key == "lease" && n >= 2 {
			renewOnce.Do(func() { close(renewed) })
		}
		return false
	}}
	r.Store.KV = f
	r.Lease.KV = f
	done := make(chan struct{})
	go func() { r.Run(context.Background(), testTask()); close(done) }()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe not started")
	}
	select {
	case <-renewed:
	case <-time.After(time.Second):
		t.Fatal("single long probe did not renew lease")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not complete")
	}
	if got := loadTask(t, r); !got.Done {
		t.Fatal("renewed task not complete")
	}
}

type oneLeaseReadFailure struct {
	kv.KV
	armed atomic.Bool
}

func (f *oneLeaseReadFailure) Get(ctx context.Context, ns, key string) ([]byte, bool, error) {
	if key == "lease" && f.armed.CompareAndSwap(true, false) {
		return nil, false, errors.New("transient lease read failure")
	}
	return f.KV.Get(ctx, ns, key)
}

func TestRunnerTransientFencingFailureCannotFinishTask(t *testing.T) {
	var fence *oneLeaseReadFailure
	var calls atomic.Int32
	r := newTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		fence.armed.Store(true)
		fmt.Fprint(w, completedSSE(testNumbers()))
	})
	fence = &oneLeaseReadFailure{KV: r.Store.KV}
	r.Store.KV = fence
	r.Lease.KV = fence
	r.RenewEvery = time.Second
	r.Run(context.Background(), testTask())
	if got := loadTask(t, r); got.Done || calls.Load() != 1 {
		t.Fatalf("fencing failure finished task: %+v calls=%d", got, calls.Load())
	}
}
