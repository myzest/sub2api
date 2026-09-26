package kv

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
)

// KV is scoped by the host to this plugin's identity.
type KV interface {
	Get(context.Context, string, string) ([]byte, bool, error)
	Set(context.Context, string, string, []byte, time.Duration) error
	Delete(context.Context, string, string) error
	List(context.Context, string, string, int) ([]string, error)
}

type hostKV struct{ client pluginv1.HostServiceClient }

func NewHostKV(c pluginv1.HostServiceClient) KV { return &hostKV{client: c} }
func (h *hostKV) Get(ctx context.Context, ns, key string) ([]byte, bool, error) {
	r, err := h.client.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: ns, Key: key})
	if err != nil {
		return nil, false, err
	}
	if r == nil {
		return nil, false, errors.New("empty KV response")
	}
	return r.Value, r.Found, nil
}
func (h *hostKV) Set(ctx context.Context, ns, key string, value []byte, ttl time.Duration) error {
	// Round positive sub-second TTLs up; zero would persist indefinitely.
	seconds := int64(ttl / time.Second)
	if ttl > 0 && ttl%time.Second != 0 {
		seconds++
	}
	_, err := h.client.KVSet(ctx, &pluginv1.KVSetRequest{Namespace: ns, Key: key, Value: value, TtlSeconds: seconds})
	return err
}
func (h *hostKV) Delete(ctx context.Context, ns, key string) error {
	_, err := h.client.KVDelete(ctx, &pluginv1.KVDeleteRequest{Namespace: ns, Key: key})
	return err
}
func (h *hostKV) List(ctx context.Context, ns, prefix string, limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	r, err := h.client.KVList(ctx, &pluginv1.KVListRequest{Namespace: ns, KeyPrefix: prefix, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("empty KV response")
	}
	return r.Keys, nil
}

type entry struct {
	value   []byte
	expires time.Time
}
type memoryKV struct {
	mu      sync.Mutex
	entries map[string]map[string]entry
	now     func() time.Time
}

func NewMemoryKV() KV { return &memoryKV{entries: map[string]map[string]entry{}, now: time.Now} }
func (m *memoryKV) Get(ctx context.Context, ns, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[ns][key]
	if ok && !e.expires.IsZero() && !m.now().Before(e.expires) {
		delete(m.entries[ns], key)
		ok = false
	}
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), e.value...), true, nil
}
func (m *memoryKV) Set(ctx context.Context, ns, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(value) > 256*1024 {
		return errors.New("KV value exceeds 256 KiB")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries[ns] == nil {
		m.entries[ns] = map[string]entry{}
	}
	e := entry{value: append([]byte(nil), value...)}
	if ttl > 0 {
		e.expires = m.now().Add(ttl)
	}
	m.entries[ns][key] = e
	return nil
}
func (m *memoryKV) Delete(ctx context.Context, ns, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries[ns], key)
	return nil
}
func (m *memoryKV) List(ctx context.Context, ns, prefix string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := []string{}
	for key, e := range m.entries[ns] {
		if !e.expires.IsZero() && !m.now().Before(e.expires) {
			delete(m.entries[ns], key)
			continue
		}
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}
