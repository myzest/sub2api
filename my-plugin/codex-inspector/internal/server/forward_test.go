package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"local.sub2api/codex-inspector/internal/kv"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/pluginconfig"
)

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
	in  []*pluginv1.ForwardRequest
	out []*pluginv1.ForwardResponse
}

func (f *fakeStream) Context() context.Context {
	if f.ctx == nil {
		return context.Background()
	}
	return f.ctx
}
func (f *fakeStream) Recv() (*pluginv1.ForwardRequest, error) {
	if len(f.in) == 0 {
		return nil, io.EOF
	}
	v := f.in[0]
	f.in = f.in[1:]
	return v, nil
}
func (f *fakeStream) Send(v *pluginv1.ForwardResponse) error { f.out = append(f.out, v); return nil }
func frames(url, body string, chunk int) []*pluginv1.ForwardRequest {
	start := &pluginv1.ForwardRequestStart{Method: "POST", Url: url, Platform: "openai", AccountType: "oauth", AccountId: 1, HasBody: len(body) > 0, ContentLength: int64(len(body))}
	start.Headers = map[string]*pluginv1.HeaderValues{"content-type": {Values: []string{"application/json"}}, "content-length": {Values: []string{"1"}}}
	out := []*pluginv1.ForwardRequest{{Frame: &pluginv1.ForwardRequest_Start{Start: start}}}
	if chunk <= 0 {
		chunk = 32768
	}
	for len(body) > 0 {
		n := min(chunk, len(body))
		out = append(out, &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte(body[:n])}})
		body = body[n:]
	}
	return append(out, &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}})
}
func environmentBody() string {
	raw, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "metadata": map[string]any{"unknown": "keep"}, "input": []any{map[string]any{"type": "message", "id": "keep-id", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context><timezone>Asia/Shanghai</timezone><current_date>2026-09-25</current_date></environment_context>", "unknown": 17}}}}})
	return string(raw)
}
func TestForwardRewritesAndRecomputesContentLength(t *testing.T) {
	var body atomic.Value
	var length atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		length.Store(r.ContentLength)
		w.Header().Set("X-Test", "preserved")
		w.WriteHeader(201)
		w.Write([]byte("response payload"))
	}))
	defer up.Close()
	s := newTestServer(t, kv.NewMemoryKV())
	c := pluginconfig.Default()
	c.Timezone.Mode = "custom"
	c.Timezone.CustomTZ = "America/Los_Angeles"
	s.cfg.Store(&c)
	s.now = func() time.Time { return time.Date(2026, 9, 26, 2, 0, 0, 0, time.UTC) }
	f := &fakeStream{in: frames(up.URL, environmentBody(), 7)}
	if e := s.Forward(f); e != nil {
		t.Fatal(e)
	}
	got := body.Load().(string)
	if !strings.Contains(got, "<timezone>America/Los_Angeles</timezone>") || !strings.Contains(got, "<current_date>2026-09-25</current_date>") || !strings.Contains(got, "keep-id") || length.Load() != int64(len(got)) {
		t.Fatal(got, length.Load())
	}
	if len(f.out) != 3 || f.out[0].GetStart().StatusCode != 201 || f.out[2].GetEnd() == nil || string(f.out[1].GetBodyChunk()) != "response payload" {
		t.Fatal(f.out)
	}
}
func TestForwardDisabledAndMalformedPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		enabled    bool
	}{{"disabled", environmentBody(), false}, {"malformed", "{oops", true}, {"unchanged", "{ \"input\": [] }", true}} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b, _ := io.ReadAll(r.Body); received <- string(b) }))
			defer up.Close()
			s := newTestServer(t, kv.NewMemoryKV())
			c := pluginconfig.Default()
			c.Enabled = tc.enabled
			c.Timezone.Mode = "custom"
			s.cfg.Store(&c)
			f := &fakeStream{in: frames(up.URL, tc.body, 5)}
			if e := s.Forward(f); e != nil {
				t.Fatal(e)
			}
			if got := <-received; got != tc.body {
				t.Fatalf("body changed %q", got)
			}
			if f.out[len(f.out)-1].GetEnd() == nil {
				t.Fatal(f.out)
			}
		})
	}
}
func TestForwardDialFailureNotSentAndNoLeak(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := listener.Addr().String()
	listener.Close()
	s := newTestServer(t, kv.NewMemoryKV())
	f := &fakeStream{in: frames("http://"+address, "", 7)}
	if e = s.Forward(f); e != nil {
		t.Fatal(e)
	}
	r := f.out[0].GetError()
	if r == nil || r.RequestSent || strings.Contains(r.Message, address) {
		t.Fatal(r)
	}
}
func TestForwardRejectsIncompleteAndUnexpectedBody(t *testing.T) {
	s := newTestServer(t, kv.NewMemoryKV())
	f := frames("http://127.0.0.1:1", "abc", 1)
	f = f[:len(f)-1]
	stream := &fakeStream{in: f}
	if e := s.Forward(stream); e != nil {
		t.Fatal(e)
	}
	if stream.out[0].GetError() == nil || stream.out[0].GetError().RequestSent {
		t.Fatal(stream.out)
	}
	f = frames("http://127.0.0.1:1", "abc", 1)
	f[0].GetStart().HasBody = false
	stream = &fakeStream{in: f}
	_ = s.Forward(stream)
	if stream.out[0].GetError() == nil {
		t.Fatal("accepted unexpected body")
	}
}
