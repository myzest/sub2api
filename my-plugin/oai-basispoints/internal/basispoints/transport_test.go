package basispoints

import (
	"bytes"
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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

func transportClient(t *testing.T, s *Server) pluginv1.TransportPluginClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pluginv1.RegisterTransportPluginServer(server, s)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pluginv1.NewTransportPluginClient(conn)
}
func forward(t *testing.T, client pluginv1.TransportPluginClient, start *pluginv1.ForwardRequestStart, body []byte) []*pluginv1.ForwardResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: start}}); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		for len(body) > 0 {
			n := min(len(body), 32<<10)
			if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: body[:n]}}); err != nil {
				sent <- err
				return
			}
			body = body[n:]
		}
		err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}})
		stream.CloseSend()
		sent <- err
	}()
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
func bodyOf(frames []*pluginv1.ForwardResponse) []byte {
	var b bytes.Buffer
	for _, f := range frames {
		b.Write(f.GetBodyChunk())
	}
	return b.Bytes()
}
func bpsStart(body []byte) *pluginv1.ForwardRequestStart {
	return &pluginv1.ForwardRequestStart{Method: "POST", Url: "https://chatgpt.com/backend-api/codex/responses", Platform: "openai", AccountType: "oauth", AccountId: 7, HasBody: true, ContentLength: int64(len(body)), Headers: headersToProto(http.Header{"Authorization": []string{"Bearer fixture-secret-token"}, "Chatgpt-Account-Id": []string{"fixture-account"}})}
}
func routeFixture(s *Server, url string) {
	s.cfg.RouteEnabled = true
	s.cfg.AccountIDs = []int64{7}
	s.responsesURL = url
}

func TestForwardBPSFullToolRoundTrip(t *testing.T) {
	native := nativeItem("exec_command", object{"cmd": "pwd"})
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		request, err := decodeObject(raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-secret-token" || r.Header.Get("X-Basispoints-Auth-Mode") != "chatgpt" || r.Header.Get("Chatgpt-Account-Id") != "fixture-account" || request["tools"] != nil || request["reasoning_effort"] != "high" || request["model_selection"] != "explicit" {
			t.Error("BPS request adaptation failed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			io.WriteString(w, nativeStream(native))
			io.WriteString(w, eventBytes(object{"type": "response.completed", "response": completed(native)}))
			return
		}
		input := request["input"].([]any)
		if digest(input[len(input)-2]) != digest(native) || input[len(input)-1].(object)["call_id"] != "call_native_1" {
			t.Error("second request lost native state")
		}
		io.WriteString(w, eventBytes(object{"type": "response.completed", "response": completed(terminalText("Done"))}))
	}))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL)
	api := transportClient(t, s)
	source := fixtureRequest()
	raw := encoded(source)
	frames := forward(t, api, bpsStart(raw), raw)
	if frames[0].GetStart().StatusCode != 200 || frames[len(frames)-1].GetEnd() == nil {
		t.Fatal(frames)
	}
	var result object
	if err := readSSE(bytes.NewReader(bodyOf(frames)), func(e object) (bool, error) {
		if str(e, "type") == "response.completed" {
			result = e["response"].(object)
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	call := result["output"].([]any)[0].(object)
	source["input"] = []any{message("user", "hello"), call, object{"type": "function_call_output", "call_id": call["call_id"], "output": "/workspace"}}
	source["stream"] = false
	raw = encoded(source)
	frames = forward(t, api, bpsStart(raw), raw)
	if frames[len(frames)-1].GetEnd() == nil {
		t.Fatal(frames)
	}
	result, err := decodeObject(bodyOf(frames))
	if err != nil || str(result, "status") != "completed" || !strings.Contains(string(bodyOf(frames)), "Done") || requests.Load() != 2 {
		t.Fatal(string(bodyOf(frames)), err)
	}
}

func TestPassthroughHeadersHostProxyAndStreaming(t *testing.T) {
	s := fixtureServer()
	s.cfg.RouteEnabled = true
	s.cfg.AccountIDs = []int64{7}
	api := transportClient(t, s)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != "payload" || r.Host != "virtual.fixture" || strings.Join(r.Header.Values("X-Multi"), ",") != "a,b" {
			t.Errorf("passthrough changed request: %q %s", raw, r.Host)
		}
		w.Header().Add("X-Multi", "one")
		w.Header().Add("X-Multi", "two")
		w.WriteHeader(201)
		io.WriteString(w, "unchanged")
	}))
	defer up.Close()
	frames := forward(t, api, &pluginv1.ForwardRequestStart{Method: "POST", Url: up.URL, Host: "virtual.fixture", Platform: "openai", AccountType: "oauth", AccountId: 8, HasBody: true, ContentLength: 7, Headers: map[string]*pluginv1.HeaderValues{"x-multi": {Values: []string{"a", "b"}}}}, []byte("payload"))
	if frames[0].GetStart().StatusCode != 201 || strings.Join(frames[0].GetStart().Headers["X-Multi"].Values, ",") != "one,two" || string(bodyOf(frames)) != "unchanged" || frames[len(frames)-1].GetEnd() == nil {
		t.Fatal("passthrough changed response", frames)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "fixture.invalid" {
			t.Error("proxy target changed")
		}
		io.WriteString(w, "proxy-ok")
	}))
	defer proxy.Close()
	frames = forward(t, api, &pluginv1.ForwardRequestStart{Method: "GET", Url: "http://fixture.invalid/test", Platform: "openai", AccountType: "oauth", AccountId: 8, ProxyUrl: proxy.URL}, nil)
	if string(bodyOf(frames)) != "proxy-ok" {
		t.Fatal(frames)
	}
}

func TestErrorsPreserveStatusAndRequestSent(t *testing.T) {
	s := fixtureServer()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(403)
		io.WriteString(w, "fixture-secret-token private prompt")
	}))
	defer up.Close()
	routeFixture(s, up.URL)
	api := transportClient(t, s)
	raw := encoded(fixtureRequest())
	frames := forward(t, api, bpsStart(raw), raw)
	if frames[0].GetStart().StatusCode != 403 || strings.Contains(string(bodyOf(frames)), "fixture-secret-token") || frames[0].GetStart().Headers["Retry-After"].Values[0] != "42" {
		t.Fatal("upstream failure status or privacy changed", frames)
	}
	start := bpsStart(raw)
	start.Url += "/compact"
	frames = forward(t, api, start, raw)
	if frames[0].GetStart().StatusCode != 400 || calls.Load() != 1 {
		t.Fatal("unsupported endpoint reached upstream")
	}
	start = bpsStart(raw)
	start.ProxyUrl = "bad-proxy"
	frames = forward(t, api, start, raw)
	if e := frames[0].GetError(); e == nil || e.RequestSent {
		t.Fatal("unsent invalid proxy marked sent")
	}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	s.responsesURL = closed.URL
	frames = forward(t, api, bpsStart(raw), raw)
	if e := frames[0].GetError(); e == nil || !e.RequestSent {
		t.Fatal("RoundTrip failure allowed replay", frames)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer broken.Close()
	s.responsesURL = broken.URL
	frames = forward(t, api, bpsStart(raw), raw)
	if e := frames[len(frames)-1].GetError(); e == nil || !e.RequestSent {
		t.Fatal("incomplete stream became completed", frames)
	}
}

func TestCancellationReachesBPS(t *testing.T) {
	started, ended := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, eventBytes(object{"type": "response.created", "response": object{"output": []any{}, "status": "in_progress"}}))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(ended)
	}))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL)
	api := transportClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := api.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := encoded(fixtureRequest())
	stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: bpsStart(raw)}})
	stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: raw}})
	stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not reached")
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reach BPS")
	}
}

func TestHealthApplyArePassiveAndProbeIsExplicitIdempotent(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		items, _ := body["input"].([]any)
		if inspectAgentInput(items).Messages != 0 || !strings.Contains(string(encoded(items)), "BPS_OK") {
			t.Error("text probe used client agent history instead of its fixed prompt")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(encoded(completed(terminalText("BPS_OK"))))
	}))
	defer up.Close()
	s := fixtureServer()
	s.responsesURL = up.URL
	s.lastRequest = &requestDiagnostic{ID: "unrelated-client-route", Origin: "route", Error: "bps_agent_encrypted_content", ErrorSource: "plugin_agent_message"}
	cfg := s.config()
	cfg.Command = &Command{ID: newID(), Instance: s.instance, IssuedAt: time.Now().Unix(), Action: "probe", AccountID: 7, Model: "gpt-5.6-sol", Effort: "low"}
	for i := 0; i < 3; i++ {
		s.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: encoded(cfg)})
		s.Health(context.Background(), &pluginv1.HealthRequest{})
	}
	if calls.Load() != 0 {
		t.Fatal("passive lifecycle contacted upstream")
	}
	for i := 0; i < 2; i++ {
		reply, _ := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: encoded(cfg)})
		if !reply.Success {
			t.Fatal(reply)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := s.Health(context.Background(), &pluginv1.HealthRequest{})
		var snapshot Snapshot
		json.Unmarshal([]byte(r.StatusJson), &snapshot)
		if snapshot.Probe != nil && snapshot.Probe.State != "running" {
			if snapshot.Probe.State != "succeeded" || calls.Load() != 1 || strings.Contains(r.StatusJson, "fixture-secret-token") {
				t.Fatal(r)
			}
			if snapshot.Probe.ID != cfg.Command.ID || snapshot.Probe.StartedAt == 0 || snapshot.Probe.FinishedAt < snapshot.Probe.StartedAt || snapshot.LastRequest == nil || snapshot.LastRequest.ID != "unrelated-client-route" || snapshot.LastRequest.Error != "bps_agent_encrypted_content" {
				t.Fatal("probe attribution or independent route record changed")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("probe did not finish")
}

func TestConfigRejectsAmbiguityAndDefaultsDisabled(t *testing.T) {
	c, err := parseConfig([]byte(`{}`))
	if err != nil || c.RouteEnabled || len(c.AccountIDs) != 0 {
		t.Fatal(c, err)
	}
	for _, raw := range []string{`null`, `{"route_enabled":true}`, `{"route_enabled":false,"route_enabled":true}`, `{"extra":"secret"}`, `{"timeout_seconds":0}`, `{"models":[]}`, `{} {}`} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatal("invalid config accepted", raw)
		}
	}
}
