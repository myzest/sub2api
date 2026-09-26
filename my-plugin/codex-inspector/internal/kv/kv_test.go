package kv

import (
	"context"
	"google.golang.org/grpc"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"testing"
	"time"
)

func TestMemoryTTLCopyAndList(t *testing.T) {
	m := NewMemoryKV().(*memoryKV)
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	ctx := context.Background()
	value := []byte("a")
	_ = m.Set(ctx, "n", "pref.b", value, time.Second)
	value[0] = 'x'
	_ = m.Set(ctx, "n", "pref.a", []byte("a"), 0)
	_ = m.Set(ctx, "n", "other", nil, 0)
	keys, _ := m.List(ctx, "n", "pref.", 1)
	if len(keys) != 1 || keys[0] != "pref.a" {
		t.Fatal(keys)
	}
	got, ok, _ := m.Get(ctx, "n", "pref.b")
	if !ok || string(got) != "a" {
		t.Fatal(got)
	}
	got[0] = 'z'
	got, _, _ = m.Get(ctx, "n", "pref.b")
	if string(got) != "a" {
		t.Fatal("alias")
	}
	now = now.Add(time.Second)
	if _, ok, _ = m.Get(ctx, "n", "pref.b"); ok {
		t.Fatal("expired")
	}
	if _, ok, _ = m.Get(ctx, "n", "pref.a"); !ok {
		t.Fatal("permanent expired")
	}
	if err := m.Set(ctx, "n", "large", make([]byte, 256*1024+1), 0); err == nil {
		t.Fatal("accepted oversize")
	}
}

type ttlClient struct {
	pluginv1.HostServiceClient
	ttl   int64
	limit int32
}

func (c *ttlClient) KVSet(_ context.Context, r *pluginv1.KVSetRequest, _ ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	c.ttl = r.TtlSeconds
	return &pluginv1.KVSetResponse{}, nil
}
func (c *ttlClient) KVList(_ context.Context, r *pluginv1.KVListRequest, _ ...grpc.CallOption) (*pluginv1.KVListResponse, error) {
	c.limit = r.Limit
	return &pluginv1.KVListResponse{}, nil
}
func TestHostTTLRoundsUp(t *testing.T) {
	client := &ttlClient{}
	k := NewHostKV(client)
	for _, tc := range []struct {
		ttl  time.Duration
		want int64
	}{{0, 0}, {time.Nanosecond, 1}, {time.Second, 1}, {time.Second + 1, 2}} {
		if err := k.Set(context.Background(), "n", "k", nil, tc.ttl); err != nil || client.ttl != tc.want {
			t.Fatalf("ttl=%v got=%d err=%v", tc.ttl, client.ttl, err)
		}
	}
	_, _ = k.List(context.Background(), "n", "", 0)
	if client.limit != 1000 {
		t.Fatal(client.limit)
	}
}
func TestLeaseAcquireRenewAndHeld(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryKV().(*memoryKV)
	now := time.Now()
	m.now = func() time.Time { return now }
	l := Lease{KV: m, NS: "detect", Key: "lease", Jitter: func() time.Duration { return 0 }}
	if ok, e := l.Acquire(ctx, "one", time.Minute); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if ok, e := l.Acquire(ctx, "two", time.Minute); e != nil || ok {
		t.Fatal(ok, e)
	}
	now = now.Add(30 * time.Second)
	if ok, e := l.Acquire(ctx, "one", time.Minute); e != nil || !ok {
		t.Fatal(ok, e)
	}
	now = now.Add(31 * time.Second)
	if ok, e := l.Holds(ctx, "one"); e != nil || !ok {
		t.Fatal(ok, e)
	}
	now = now.Add(30 * time.Second)
	if ok, e := l.Acquire(ctx, "two", time.Minute); e != nil || !ok {
		t.Fatal(ok, e)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := l.Acquire(canceled, "two", time.Minute); e == nil {
		t.Fatal("ignored cancel")
	}
}
