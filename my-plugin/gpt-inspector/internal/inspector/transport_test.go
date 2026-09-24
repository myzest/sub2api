package inspector

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

func transportClient(t *testing.T) pluginv1.TransportPluginClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pluginv1.RegisterTransportPluginServer(server, New())
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pluginv1.NewTransportPluginClient(conn)
}
func forward(t *testing.T, client pluginv1.TransportPluginClient, start *pluginv1.ForwardRequestStart, body string) []*pluginv1.ForwardResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: start}}); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte(body)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}); err != nil {
		t.Fatal(err)
	}
	stream.CloseSend()
	var frames []*pluginv1.ForwardResponse
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, r)
	}
	return frames
}
func TestForwardBodyHeadersProxyAndFailures(t *testing.T) {
	client := transportClient(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" || r.ContentLength != 7 || strings.Join(r.Header.Values("X-Multi"), ",") != "a,b" {
			t.Errorf("bad forwarded request: %q %d %v", body, r.ContentLength, r.Header)
		}
		w.Header().Add("X-Multi", "one")
		w.Header().Add("X-Multi", "two")
		w.WriteHeader(201)
		io.WriteString(w, "forwarded")
	}))
	defer up.Close()
	frames := forward(t, client, &pluginv1.ForwardRequestStart{Method: "POST", Url: up.URL, Platform: "openai", AccountType: "oauth", HasBody: true, ContentLength: 7, Headers: map[string]*pluginv1.HeaderValues{"X-Multi": {Values: []string{"a", "b"}}}}, "payload")
	if len(frames) < 3 || frames[0].GetStart().StatusCode != 201 || strings.Join(frames[0].GetStart().Headers["X-Multi"].Values, ",") != "one,two" || frames[len(frames)-1].GetEnd() == nil {
		t.Fatal("stream framing or duplicate headers lost", frames)
	}
	var text strings.Builder
	for _, f := range frames {
		text.Write(f.GetBodyChunk())
	}
	if text.String() != "forwarded" {
		t.Fatal(text.String())
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "fixture.invalid" {
			t.Error("proxy not used")
		}
		io.WriteString(w, "proxy-ok")
	}))
	defer proxy.Close()
	frames = forward(t, client, &pluginv1.ForwardRequestStart{Method: "GET", Url: "http://fixture.invalid/fixture", Platform: "openai", AccountType: "oauth", ProxyUrl: proxy.URL}, "")
	if frames[len(frames)-1].GetEnd() == nil {
		t.Fatal("proxy failed", frames)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "short")
	}))
	defer broken.Close()
	frames = forward(t, client, &pluginv1.ForwardRequestStart{Method: "GET", Url: broken.URL, Platform: "openai", AccountType: "oauth"}, "")
	last := frames[len(frames)-1].GetError()
	if last == nil || !last.RequestSent {
		t.Fatal("partial response could be replayed")
	}
	frames = forward(t, client, &pluginv1.ForwardRequestStart{Method: "GET", Url: "file:///secret", Platform: "openai", AccountType: "oauth"}, "")
	if frames[0].GetError() == nil || frames[0].GetError().RequestSent {
		t.Fatal("invalid request marked as sent")
	}
}
func TestForwardCancellationReachesUpstream(t *testing.T) {
	ended := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(ended)
	}))
	defer up.Close()
	client := transportClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{Method: "GET", Url: up.URL, Platform: "openai", AccountType: "oauth"}}})
	stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request leaked after cancellation")
	}
}
