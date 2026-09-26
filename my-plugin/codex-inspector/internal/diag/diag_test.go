package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"local.sub2api/codex-inspector/internal/kv"
	"strings"
	"testing"
	"time"
)

func TestRingAndTally(t *testing.T) {
	now := time.Now()
	r := New("host", func() time.Time { return now })
	for i := 0; i < 501; i++ {
		r.Record(Event{Action: fmt.Sprint(i)})
	}
	events := r.Events()
	if len(events) != 500 || events[0].Action != "500" || events[499].Action != "1" {
		t.Fatal("ring")
	}
	r = New("host", func() time.Time { return now })
	r.Tally("tz", "ok")
	r.Tally("tz", "ok")
	r.Tally("tz", "ok")
	r.Tally("tz", "error")
	events = r.Events()
	if len(events) != 2 || events[1].Count != 3 {
		t.Fatal(events)
	}
	now = now.Add(61 * time.Second)
	r.Tally("tz", "ok")
	if len(r.Events()) != 3 {
		t.Fatal(r.Events())
	}
}
func TestPublishAndMerge(t *testing.T) {
	ctx := context.Background()
	k := kv.NewMemoryKV()
	now := time.Now()
	r := New("a", func() time.Time { return now })
	r.Record(Event{Action: "one"})
	if ok, e := r.Publish(ctx, k); !ok || e != nil {
		t.Fatal(ok, e)
	}
	if ok, e := r.Publish(ctx, k); ok || e != nil {
		t.Fatal(ok, e)
	}
	s := New("b", func() time.Time { return now.Add(time.Second) })
	s.Record(Event{Action: "two"})
	_, _ = s.Publish(ctx, k)
	events, n, e := Merge(ctx, k)
	if e != nil || n != 2 || len(events) != 2 || events[0].Action != "two" {
		t.Fatal(events, n, e)
	}
	now = now.Add(TTL / 2)
	if ok, e := r.Publish(ctx, k); e != nil || !ok {
		t.Fatal("TTL refresh", ok, e)
	}
}
func TestPublishBoundsPayload(t *testing.T) {
	r := New("h", nil)
	for i := 0; i < 500; i++ {
		r.Record(Event{Action: fmt.Sprint(i), Model: strings.Repeat("x", 1000)})
	}
	k := kv.NewMemoryKV()
	if _, e := r.Publish(context.Background(), k); e != nil {
		t.Fatal(e)
	}
	keys, _ := k.List(context.Background(), NS, "", 1000)
	raw, _, _ := k.Get(context.Background(), NS, keys[0])
	var es []Event
	if len(raw) > maxPayloadBytes || json.Unmarshal(raw, &es) != nil || len(es) >= 500 || es[0].Action != "499" {
		t.Fatal("bad truncation")
	}
}
func TestSanitizeError(t *testing.T) {
	if got := SanitizeError("Cookie: one=private-one; two=private-two\nAuthorization: Basic encoded-secret"); strings.Contains(got, "private") || strings.Contains(got, "encoded-secret") {
		t.Fatal(got)
	}
	input := "proxy http://alice:password123@localhost:1234/private?token=urltoken Bearer abc.secret.token cookie=session123 token=tok123 password=pass123"
	s := SanitizeError(input)
	for _, secret := range []string{"alice", "password123", "localhost", "urltoken", "abc.secret.token", "session123", "tok123", "pass123"} {
		if strings.Contains(s, secret) {
			t.Fatalf("leak %s in %q", secret, s)
		}
	}
	if len([]rune(SanitizeError(strings.Repeat("中", 201)))) != 200 {
		t.Fatal("limit")
	}
}
