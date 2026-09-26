package tzresolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"local.sub2api/codex-inspector/internal/diag"
	"local.sub2api/codex-inspector/internal/kv"
	"local.sub2api/codex-inspector/internal/pluginconfig"
	"local.sub2api/codex-inspector/internal/transport"
)

const defaultEndpoint = "https://ipinfo.io/json"
const negativeTTL = 10 * time.Minute
const maxResponseBytes = 64 * 1024
const cacheReadTimeout = 500 * time.Millisecond

type Resolver struct {
	KV       kv.KV
	Pool     *transport.Pool
	Diag     *diag.Recorder
	Endpoint string
	Timeout  time.Duration

	mu      sync.Mutex
	flights map[string]*lookup
}

type lookup struct {
	done  chan struct{}
	value string
}

// Target returns a whitelisted IANA timezone, including on invalid manually
// constructed configs or failed egress requests. Normal configs use DefaultTZ.
func (r *Resolver) Target(ctx context.Context, cfg pluginconfig.TimezoneConfig, accountID int64, proxyURL string) string {
	fallback := cfg.DefaultTZ
	if !pluginconfig.InWhitelist(fallback) {
		fallback = pluginconfig.Default().Timezone.DefaultTZ
	}
	allowed := func(tz string) string {
		if pluginconfig.InWhitelist(tz) {
			return tz
		}
		return fallback
	}
	account := strconv.FormatInt(accountID, 10)
	if tz, ok := cfg.Overrides[account]; ok {
		return allowed(tz)
	}
	switch cfg.Mode {
	case "custom":
		return allowed(cfg.CustomTZ)
	case "random_stable":
		h := fnv.New32a()
		_, _ = h.Write([]byte(account))
		if len(pluginconfig.Whitelist) > 0 {
			return pluginconfig.Whitelist[uint64(h.Sum32())%uint64(len(pluginconfig.Whitelist))]
		}
	case "egress_ip":
		if r != nil {
			return allowed(r.egress(ctx, cfg.EgressCacheHours, proxyURL))
		}
	}
	return fallback
}

func cacheKey(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	hash := sha256.Sum256([]byte(proxyURL))
	return hex.EncodeToString(hash[:])
}

// readCache handles storage failure as a hit with the negative sentinel, so a
// broken KV does not cause uncached GeoIP traffic on every model request.
func (r *Resolver) readCache(ctx context.Context, key string) (string, bool) {
	if r.KV == nil {
		r.tallyKVError()
		return "-", true
	}
	cacheCtx, cancel := context.WithTimeout(ctx, cacheReadTimeout)
	defer cancel()
	value, found, err := r.KV.Get(cacheCtx, "egress", key)
	if err != nil {
		r.tallyKVError()
		return "-", true
	}
	return string(value), found
}

func (r *Resolver) egress(ctx context.Context, hours int, proxyURL string) string {
	key := cacheKey(proxyURL)
	if value, found := r.readCache(ctx, key); found {
		return value
	}
	r.mu.Lock()
	if pending := r.flights[key]; pending != nil {
		r.mu.Unlock()
		select {
		case <-pending.done:
			return pending.value
		case <-ctx.Done():
			return "-"
		}
	}
	if r.flights == nil {
		r.flights = make(map[string]*lookup)
	}
	pending := &lookup{done: make(chan struct{})}
	r.flights[key] = pending
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.flights, key)
		close(pending.done)
		r.mu.Unlock()
	}()
	// Recheck after becoming leader; another completed flight may have populated
	// the KV between the first miss and taking the flight lock.
	if value, found := r.readCache(ctx, key); found {
		pending.value = value
		return value
	}
	value, err := r.query(ctx, proxyURL)
	ttl := time.Duration(hours) * time.Hour
	outcome := "ok"
	if err != nil {
		value, ttl, outcome = "-", negativeTTL, "fail"
	} else if hours < 1 || hours > 720 {
		ttl = 24 * time.Hour
	}
	if r.Diag != nil {
		event := diag.Event{Action: "tz_egress_lookup", Outcome: outcome}
		if err != nil {
			event.Error = err.Error()
		}
		r.Diag.Record(event)
	}
	// Preserve negative-cache behavior even when the lookup/caller timed out.
	cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := r.KV.Set(cacheCtx, "egress", key, []byte(value), ttl); err != nil {
		r.tallyKVError()
	}
	pending.value = value
	return value
}

func (r *Resolver) query(ctx context.Context, proxyURL string) (string, error) {
	if r.Pool == nil {
		return "", errors.New("egress transport pool unavailable")
	}
	client, err := r.Pool.Client(proxyURL)
	if err != nil {
		return "", errors.New("egress client creation failed")
	}
	endpoint, timeout := r.Endpoint, r.Timeout
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.New("invalid egress endpoint")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// Do not log net/url errors: these may contain the full proxy URL or its
		// password even when the HTTP endpoint itself is safe to print.
		if errors.Is(err, context.DeadlineExceeded) {
			return "", errors.New("egress lookup timed out")
		}
		if errors.Is(err, context.Canceled) {
			return "", errors.New("egress lookup canceled")
		}
		return "", errors.New("egress HTTP request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("egress endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", errors.New("read egress response failed")
	}
	if len(body) > maxResponseBytes {
		return "", errors.New("egress response exceeds 64 KiB")
	}
	var result struct {
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Timezone == "" {
		return "", errors.New("egress response has no valid timezone")
	}
	return result.Timezone, nil
}

func (r *Resolver) tallyKVError() {
	if r.Diag != nil {
		r.Diag.Tally("tz_egress_lookup", "kv_error")
	}
}
