package tzresolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"local.sub2api/codex-inspector/internal/diag"
	"local.sub2api/codex-inspector/internal/kv"
	"local.sub2api/codex-inspector/internal/pluginconfig"
	"local.sub2api/codex-inspector/internal/transport"
)

type write struct {
	key, value string
	ttl        time.Duration
}
type spyKV struct {
	kv.KV
	mu             sync.Mutex
	writes         []write
	getErr, setErr error
}

func (s *spyKV) Get(ctx context.Context, ns, key string) ([]byte, bool, error) {
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	return s.KV.Get(ctx, ns, key)
}
func (s *spyKV) Set(ctx context.Context, ns, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	s.writes = append(s.writes, write{key, string(value), ttl})
	s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	return s.KV.Set(ctx, ns, key, value, ttl)
}
func (s *spyKV) snapshot() []write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]write(nil), s.writes...)
}

func fakeResolver(t *testing.T, handler http.HandlerFunc) (*Resolver, *atomic.Int32, *spyKV) {
	t.Helper()
	hits := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { hits.Add(1); handler(w, req) }))
	t.Cleanup(srv.Close)
	store := &spyKV{KV: kv.NewMemoryKV()}
	pool := transport.NewPool()
	t.Cleanup(pool.CloseIdle)
	return &Resolver{KV: store, Pool: pool, Diag: diag.New("t", nil), Endpoint: srv.URL, Timeout: time.Second}, hits, store
}

func TestOverrideWins(t *testing.T) {
	r := &Resolver{}
	cfg := pluginconfig.Default().Timezone
	cfg.Overrides = map[string]string{"42": "Asia/Tokyo"}
	for _, mode := range []string{"custom", "random_stable", "egress_ip", "invalid"} {
		cfg.Mode = mode
		if got := r.Target(context.Background(), cfg, 42, "invalid proxy"); got != "Asia/Tokyo" {
			t.Errorf("%s: %s", mode, got)
		}
	}
}

func TestCustomAndDefensiveFallbacks(t *testing.T) {
	r := &Resolver{}
	cfg := pluginconfig.Default().Timezone
	cfg.Mode = "custom"
	cfg.CustomTZ = "Europe/Paris"
	if got := r.Target(context.Background(), cfg, 1, ""); got != cfg.CustomTZ {
		t.Fatal(got)
	}
	for _, bad := range []pluginconfig.TimezoneConfig{{}, {Mode: "unknown"}, {Mode: "custom", CustomTZ: "Asia/Shanghai"}, {Mode: "egress_ip"}, {Overrides: map[string]string{"1": ""}}} {
		if got := r.Target(context.Background(), bad, 1, ""); !pluginconfig.InWhitelist(got) {
			t.Errorf("invalid fallback %q", got)
		}
	}
	var nilResolver *Resolver
	if got := nilResolver.Target(context.Background(), pluginconfig.Default().Timezone, 1, ""); got != "Asia/Singapore" {
		t.Fatal(got)
	}
}

func TestRandomStableIsDeterministic(t *testing.T) {
	r := &Resolver{}
	cfg := pluginconfig.Default().Timezone
	cfg.Mode = "random_stable"
	for _, id := range []int64{1, 2, 42, 9223372036854775807} {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strconv.FormatInt(id, 10)))
		want := pluginconfig.Whitelist[int(h.Sum32())%len(pluginconfig.Whitelist)]
		a, b := r.Target(context.Background(), cfg, id, ""), r.Target(context.Background(), cfg, id, "")
		if a != want || b != a || !pluginconfig.InWhitelist(a) {
			t.Fatalf("account %d: %s %s want %s", id, a, b, want)
		}
	}
}

func TestEgressCachesPositive(t *testing.T) {
	r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected request")
		}
		_, _ = io.WriteString(w, `{"timezone":"America/Los_Angeles"}`)
	})
	cfg := pluginconfig.Default().Timezone
	cfg.EgressCacheHours = 37
	for i := 0; i < 2; i++ {
		if got := r.Target(context.Background(), cfg, 1, ""); got != "America/Los_Angeles" {
			t.Fatal(got)
		}
	}
	if hits.Load() != 1 {
		t.Fatal("cache miss", hits.Load())
	}
	v, ok, err := store.Get(context.Background(), "egress", "direct")
	if err != nil || !ok || string(v) != "America/Los_Angeles" {
		t.Fatalf("cache %q %v %v", v, ok, err)
	}
	writes := store.snapshot()
	if len(writes) != 1 || writes[0].ttl != 37*time.Hour {
		t.Fatalf("TTL %v", writes)
	}
	events := r.Diag.Events()
	if len(events) != 1 || events[0].Action != "tz_egress_lookup" || events[0].Outcome != "ok" {
		t.Fatalf("events %+v", events)
	}
}

func TestEgressNonWhitelistFallsBackButCaches(t *testing.T) {
	r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"timezone":"Asia/Shanghai"}`) })
	cfg := pluginconfig.Default().Timezone
	for i := 0; i < 2; i++ {
		if got := r.Target(context.Background(), cfg, 1, ""); got != cfg.DefaultTZ {
			t.Fatal(got)
		}
	}
	if hits.Load() != 1 {
		t.Fatal(hits.Load())
	}
	writes := store.snapshot()
	if len(writes) != 1 || writes[0].value != "Asia/Shanghai" || writes[0].ttl != 24*time.Hour {
		t.Fatalf("non-whitelist cache %+v", writes)
	}
	cfg.DefaultTZ = "Europe/Paris"
	if got := r.Target(context.Background(), cfg, 1, ""); got != "Europe/Paris" {
		t.Fatal("cached fallback must respect current config", got)
	}
}

func TestEgressFailureNegativeCache(t *testing.T) {
	r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	cfg := pluginconfig.Default().Timezone
	for i := 0; i < 2; i++ {
		if got := r.Target(context.Background(), cfg, 1, ""); got != cfg.DefaultTZ {
			t.Fatal(got)
		}
	}
	writes := store.snapshot()
	if hits.Load() != 1 || len(writes) != 1 || writes[0].value != "-" || writes[0].ttl != 10*time.Minute {
		t.Fatalf("negative caching %d %+v", hits.Load(), writes)
	}
	events := r.Diag.Events()
	if len(events) != 1 || events[0].Outcome != "fail" || !strings.Contains(events[0].Error, "500") {
		t.Fatalf("events %+v", events)
	}
}

func TestEgressKeyHashedAndNoProxyLeak(t *testing.T) {
	r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach endpoint directly") })
	r.Timeout = 100 * time.Millisecond
	proxy := "http://u:secret@127.0.0.1:1"
	cfg := pluginconfig.Default().Timezone
	if got := r.Target(context.Background(), cfg, 1, proxy); got != cfg.DefaultTZ {
		t.Fatal(got)
	}
	hash := sha256.Sum256([]byte(proxy))
	wantKey := hex.EncodeToString(hash[:])
	keys, err := store.List(context.Background(), "egress", "", 20)
	if err != nil || len(keys) != 1 || keys[0] != wantKey || hits.Load() != 0 {
		t.Fatalf("cache keys %v %v hits=%d", keys, err, hits.Load())
	}
	data, _ := json.Marshal(r.Diag.Events())
	for _, secret := range []string{"secret", proxy, "127.0.0.1"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("proxy leaked: %s", data)
		}
	}
	for _, w := range store.snapshot() {
		if strings.Contains(w.value, "secret") {
			t.Fatal("proxy leaked into cache")
		}
	}
}

func TestEgressUsesConfiguredProxy(t *testing.T) {
	hits := new(atomic.Int32)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Host != "geo.test.invalid" || r.Header.Get("Proxy-Authorization") == "" {
			t.Errorf("request did not traverse configured authenticated proxy")
		}
		_, _ = io.WriteString(w, `{"timezone":"Europe/Paris"}`)
	}))
	defer proxyServer.Close()
	proxy, _ := url.Parse(proxyServer.URL)
	proxy.User = url.UserPassword("user", "proxy-password")
	pool := transport.NewPool()
	defer pool.CloseIdle()
	r := &Resolver{KV: kv.NewMemoryKV(), Pool: pool, Diag: diag.New("t", nil), Endpoint: "http://geo.test.invalid/json"}
	if got := r.Target(context.Background(), pluginconfig.Default().Timezone, 1, proxy.String()); got != "Europe/Paris" || hits.Load() != 1 {
		t.Fatalf("got %s, hits %d", got, hits.Load())
	}
}

func TestEgressRejectsMalformedAndOversizedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"missing": `{}`, "empty": `{"timezone":""}`, "null": `null`, "null field": `{"timezone":null}`,
		"wrong type": `{"timezone":42}`, "array": `[]`, "invalid": `not JSON`, "trailing": `{"timezone":"Asia/Tokyo"} {}`,
		"oversized": `{"timezone":"Asia/Tokyo","padding":"` + strings.Repeat("a", maxResponseBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
			cfg := pluginconfig.Default().Timezone
			if got := r.Target(context.Background(), cfg, 1, ""); got != cfg.DefaultTZ {
				t.Fatal(got)
			}
			_ = r.Target(context.Background(), cfg, 1, "")
			writes := store.snapshot()
			if hits.Load() != 1 || len(writes) != 1 || writes[0].value != "-" {
				t.Fatalf("invalid response not negative cached: %v", writes)
			}
		})
	}
}

func TestEgressAcceptsExactly64KiBAndRejectsRedirect(t *testing.T) {
	base := `{"timezone":"Asia/Tokyo"}`
	r, _, _ := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, base+strings.Repeat(" ", maxResponseBytes-len(base)))
	})
	if got := r.Target(context.Background(), pluginconfig.Default().Timezone, 1, ""); got != "Asia/Tokyo" {
		t.Fatal(got)
	}
	r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://unreachable.invalid/")
		w.WriteHeader(302)
	})
	if got := r.Target(context.Background(), pluginconfig.Default().Timezone, 1, ""); got != "Asia/Singapore" || hits.Load() != 1 || store.snapshot()[0].value != "-" {
		t.Fatal(got)
	}
}

func TestEgressTimeoutIsNegativeCached(t *testing.T) {
	r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	r.Timeout = 30 * time.Millisecond
	cfg := pluginconfig.Default().Timezone
	if got := r.Target(context.Background(), cfg, 1, ""); got != cfg.DefaultTZ {
		t.Fatal(got)
	}
	_ = r.Target(context.Background(), cfg, 1, "")
	writes := store.snapshot()
	if hits.Load() != 1 || len(writes) != 1 || writes[0].value != "-" {
		t.Fatalf("timeout not cached: %d %v", hits.Load(), writes)
	}
}

func TestEgressKVFailureDoesNotQuery(t *testing.T) {
	r, hits, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not issue query when cache cannot be read")
	})
	store.getErr = errors.New("storage unavailable")
	cfg := pluginconfig.Default().Timezone
	_ = r.Target(context.Background(), cfg, 1, "")
	_ = r.Target(context.Background(), cfg, 1, "")
	events := r.Diag.Events()
	if hits.Load() != 0 || len(events) != 1 || events[0].Outcome != "kv_error" || events[0].Count != 2 {
		t.Fatalf("KV failure %d %+v", hits.Load(), events)
	}
}

func TestEgressCacheWriteFailureReturnsLookupAndTallies(t *testing.T) {
	r, _, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"timezone":"Asia/Tokyo"}`) })
	store.setErr = errors.New("storage unavailable")
	if got := r.Target(context.Background(), pluginconfig.Default().Timezone, 1, ""); got != "Asia/Tokyo" {
		t.Fatal(got)
	}
	events := r.Diag.Events()
	if len(events) != 2 {
		t.Fatalf("missing KV warning %+v", events)
	}
}

func TestEgressConcurrentMissesSingleFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r, hits, _ := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		_, _ = io.WriteString(w, `{"timezone":"Asia/Tokyo"}`)
	})
	cfg := pluginconfig.Default().Timezone
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- r.Target(context.Background(), cfg, 1, "") }()
	}
	close(start)
	<-entered
	close(release)
	wg.Wait()
	close(results)
	for got := range results {
		if got != "Asia/Tokyo" {
			t.Error(got)
		}
	}
	if hits.Load() != 1 || len(r.Diag.Events()) != 1 {
		t.Fatalf("cache stampede: %d requests", hits.Load())
	}
}

func TestEgressCachedValueNoNetwork(t *testing.T) {
	for _, value := range []string{"-", "Asia/Shanghai", "", "Asia/Tokyo"} {
		r := &Resolver{KV: kv.NewMemoryKV(), Diag: diag.New("t", nil)}
		_ = r.KV.Set(context.Background(), "egress", "direct", []byte(value), time.Hour)
		want := "Asia/Singapore"
		if value == "Asia/Tokyo" {
			want = value
		}
		if got := r.Target(context.Background(), pluginconfig.Default().Timezone, 1, ""); got != want || len(r.Diag.Events()) != 0 {
			t.Fatalf("cache value %q: %s", value, got)
		}
	}
}

func TestEgressMissingPoolAndInvalidProxyFailSafely(t *testing.T) {
	r, _, store := fakeResolver(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not call endpoint") })
	r.Pool = nil
	cfg := pluginconfig.Default().Timezone
	if got := r.Target(context.Background(), cfg, 1, ""); got != cfg.DefaultTZ || store.snapshot()[0].value != "-" {
		t.Fatal(got)
	}
	r.Pool = transport.NewPool()
	defer r.Pool.CloseIdle()
	if got := r.Target(context.Background(), cfg, 1, "unsupported://u:secret@localhost"); got != cfg.DefaultTZ {
		t.Fatal(got)
	}
	data, _ := json.Marshal(r.Diag.Events())
	if strings.Contains(string(data), "secret") {
		t.Fatal("leaked proxy error")
	}
}

type blockingKV struct {
	kv.KV
	deadlineObserved bool
	cause            error
}

func (b *blockingKV) Get(ctx context.Context, ns, key string) ([]byte, bool, error) {
	deadline, ok := ctx.Deadline()
	b.deadlineObserved = ok && time.Until(deadline) <= cacheReadTimeout
	<-ctx.Done()
	b.cause = ctx.Err()
	return nil, false, ctx.Err()
}

func TestEgressCacheReadHasIndependentDeadline(t *testing.T) {
	store := &blockingKV{KV: kv.NewMemoryKV()}
	r := &Resolver{KV: store, Diag: diag.New("t", nil)}
	started := time.Now()
	got := r.Target(context.Background(), pluginconfig.Default().Timezone, 1, "")
	if got != "Asia/Singapore" || !store.deadlineObserved || !errors.Is(store.cause, context.DeadlineExceeded) {
		t.Fatalf("unbounded cache read: %s %+v", got, store)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cache read took %s", elapsed)
	}
	events := r.Diag.Events()
	if len(events) != 1 || events[0].Outcome != "kv_error" {
		t.Fatalf("missing cache timeout diagnostic %+v", events)
	}
}
