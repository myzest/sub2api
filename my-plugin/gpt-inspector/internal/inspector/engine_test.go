package inspector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

type fakeHost struct {
	pluginv1.HostServiceClient
	mu        sync.Mutex
	values    map[string][]byte
	calls     atomic.Int64
	beforeSet func(*pluginv1.KVSetRequest) error
}

func (h *fakeHost) KVGet(_ context.Context, r *pluginv1.KVGetRequest, _ ...grpc.CallOption) (*pluginv1.KVGetResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.values[r.Key]
	return &pluginv1.KVGetResponse{Found: ok, Value: append([]byte(nil), v...)}, nil
}
func (h *fakeHost) KVSet(_ context.Context, r *pluginv1.KVSetRequest, _ ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	if len(r.Value) > 256*1024 {
		return nil, fmt.Errorf("host KV limit exceeded")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.beforeSet != nil {
		if err := h.beforeSet(r); err != nil {
			return nil, err
		}
	}
	h.values[r.Key] = append([]byte(nil), r.Value...)
	return &pluginv1.KVSetResponse{}, nil
}
func (h *fakeHost) KVDelete(_ context.Context, r *pluginv1.KVDeleteRequest, _ ...grpc.CallOption) (*pluginv1.KVDeleteResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.values, r.Key)
	return &pluginv1.KVDeleteResponse{}, nil
}
func (h *fakeHost) KVList(_ context.Context, r *pluginv1.KVListRequest, _ ...grpc.CallOption) (*pluginv1.KVListResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var keys []string
	for k := range h.values {
		if strings.HasPrefix(k, r.KeyPrefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > int(r.Limit) {
		keys = keys[:r.Limit]
	}
	return &pluginv1.KVListResponse{Keys: keys}, nil
}
func (h *fakeHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	h.calls.Add(1)
	return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 7, Name: "fixture account", Platform: "openai", AccountType: "oauth", Status: "active", Schedulable: true}, {Id: 8, Name: "paused account", Platform: "openai", AccountType: "oauth", Status: "active", Schedulable: false}, {Id: 9, Name: "fixture B", Platform: "openai", AccountType: "oauth", Status: "active", Schedulable: true}}}, nil
}
func (h *fakeHost) ResolveOutboundIdentity(_ context.Context, r *pluginv1.ResolveOutboundIdentityRequest, _ ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	token, account := "fixture-access-token", "fixture-chatgpt-account"
	if r.AccountId == 9 {
		token, account = "fixture-access-token-B", "fixture-chatgpt-account-B"
	}
	return &pluginv1.ResolveOutboundIdentityResponse{Found: r.AccountId == 7 || r.AccountId == 9, AccountId: r.AccountId, Platform: "openai", AccountType: "oauth", Token: token, Headers: map[string]*pluginv1.HeaderValues{
		"Version": {Values: []string{"0.144.0"}}, "User-Agent": {Values: []string{"codex_cli_rs/0.144.0"}},
		"Originator": {Values: []string{"codex_cli_rs"}}, "Chatgpt-Account-Id": {Values: []string{account}},
		"Openai-Beta": {Values: []string{"responses=experimental"}}, "Session_id": {Values: []string{"host-fixed-session"}},
	}}, nil
}
func testServer(t *testing.T, handler http.HandlerFunc) (*Server, *fakeHost) {
	t.Helper()
	h := &fakeHost{values: map[string][]byte{}}
	s := New()
	s.host = h
	s.store = &store{host: h, sizes: map[string]int64{}}
	s.ready = true
	s.catalogs[7] = []Model{{ID: "fixture-model", Efforts: []string{"none", "high"}}}
	s.catalogs[9] = s.catalogs[7]
	s.accounts, _ = s.listAccounts(context.Background())
	s.publishLocked()
	if handler != nil {
		up := httptest.NewServer(handler)
		t.Cleanup(up.Close)
		s.modelsURL = up.URL + "/models"
		s.responsesURL = up.URL + "/responses"
	}
	return s, h
}
func testCommand(s *Server, action string) Command {
	return Command{ID: newID(), Instance: s.instance, IssuedAt: time.Now().Unix(), Action: action, Selection: Selection{AccountID: 7, Model: "fixture-model", Effort: "high", Prompts: []string{"iphone"}, Rounds: 1}}
}
func invoke(t *testing.T, s *Server, c Command) json.RawMessage {
	t.Helper()
	raw, _ := json.Marshal(Config{Command: &c})
	r, err := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: raw})
	if err != nil || !r.Success {
		t.Fatalf("command %s: %v / %v", c.Action, r, err)
	}
	var x struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.StatusJson), &x); err != nil {
		t.Fatal(err)
	}
	return x.Data
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func getSnapshot(s *Server) Snapshot {
	var x Snapshot
	_ = json.Unmarshal([]byte(s.snapshot.Load().(string)), &x)
	return x
}
func activeBatch(s *Server, id int64) *Batch {
	for _, b := range getSnapshot(s).Active {
		if b.AccountID == id {
			return b
		}
	}
	return nil
}
func completed(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	e := map[string]any{"type": "response.completed", "response": map[string]any{"id": "fixture-response", "model": "fixture-model", "status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}}
	b, _ := json.Marshal(e)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func TestParallelPromptsFreshSessionsFailureAndLatestBatch(t *testing.T) {
	var calls atomic.Int64
	var inFlight atomic.Int64
	var maximum atomic.Int64
	var mu sync.Mutex
	sessions := map[string]bool{}
	promptCalls, promptActive := map[string]int{}, map[string]int{}
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	var released sync.Once
	s, h := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		active := inFlight.Add(1)
		defer inFlight.Add(-1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["previous_response_id"] != nil || body["store"] != false || body["tool_choice"] != "none" || len(body["tools"].([]any)) != 0 || len(body["input"].([]any)) != 1 {
			t.Error("history or tools leaked into request")
		}
		for key, want := range map[string]string{"Authorization": "Bearer fixture-access-token", "User-Agent": "codex_cli_rs/0.144.0", "Originator": "codex_cli_rs", "Version": "0.144.0", "Chatgpt-Account-Id": "fixture-chatgpt-account", "Openai-Beta": "responses=experimental"} {
			if r.Header.Get(key) != want {
				t.Errorf("resolved Codex identity header %s was not preserved", key)
			}
		}
		session := r.Header.Get("Session_id")
		promptText := body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		mu.Lock()
		if active > maximum.Load() {
			maximum.Store(active)
		}
		promptCalls[promptText]++
		attempt := promptCalls[promptText]
		promptActive[promptText]++
		if promptActive[promptText] > 1 {
			t.Error("rounds of the same prompt overlapped")
		}
		if sessions[session] || session == "host-fixed-session" || !uuidPattern.MatchString(session) {
			t.Error("session reused")
		}
		sessions[session] = true
		mu.Unlock()
		defer func() { mu.Lock(); promptActive[promptText]--; mu.Unlock() }()
		if body["prompt_cache_key"] != session || r.Header.Get("Conversation_id") != session {
			t.Error("request identity mismatch")
		}
		if n <= 2 {
			arrived <- struct{}{}
			<-release
		}
		japan, _ := findPrompt("japan_pm")
		if promptText == japan.Text && attempt == 1 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"fixture rate limit"}}`)
			return
		}
		completed(w, fmt.Sprintf("原始回答 %d", n))
	})
	defer released.Do(func() { close(release) })
	c := testCommand(s, "start")
	c.Prompts = []string{"iphone", "japan_pm"}
	c.Rounds = 2
	requestCtx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(Config{Command: &c})
	r, err := s.TestConfig(requestCtx, &pluginv1.TestConfigRequest{ConfigJson: raw})
	if err != nil || !r.Success {
		t.Fatal(r, err)
	}
	cancel()
	waitFor(t, func() bool { return len(arrived) == 2 })
	if active := activeBatch(s, 7); active == nil || active.Items[0].State != "running" || active.Items[1].State != "running" || active.Items[2].State != "queued" {
		t.Fatal("selected prompts did not start together", active)
	}
	released.Do(func() { close(release) })
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	if calls.Load() != 4 || maximum.Load() != 2 {
		t.Fatalf("calls=%d max concurrency=%d", calls.Load(), maximum.Load())
	}
	var rec Record
	_ = json.Unmarshal(invoke(t, s, testCommand(s, "read_batch")), &rec)
	if rec.Pending != nil || rec.Latest.State != "completed" || rec.Latest.Items[1].State != "failed" || rec.Latest.Items[3].State != "completed" {
		t.Fatalf("unexpected record %+v", rec)
	}
	read := testCommand(s, "read_result")
	read.BatchID = c.ID
	read.Item = 0
	var result struct {
		Data string `json:"data"`
	}
	_ = json.Unmarshal(invoke(t, s, read), &result)
	blob, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil || len(blob) == 0 {
		t.Fatal("result missing", err)
	}
	before := getSnapshot(s).Bytes
	var actual int64
	h.mu.Lock()
	for _, v := range h.values {
		actual += int64(len(v))
	}
	h.mu.Unlock()
	if before != actual {
		t.Fatal("storage count drift", before, actual)
	}
	// Repeating an acknowledged start must not issue new requests.
	invoke(t, s, c)
	if calls.Load() != 4 {
		t.Fatal("duplicate execution")
	}
	newBatch := testCommand(s, "start")
	invoke(t, s, newBatch)
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for key := range h.values {
		if strings.Contains(key, c.ID) {
			t.Fatal("previous batch survived", key)
		}
	}
}

func TestClearCancelsAndPreventsLateWrites(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	var released sync.Once
	s, h := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		completed(w, "late result")
	})
	defer released.Do(func() { close(release) })
	c := testCommand(s, "start")
	c.Prompts = []string{"pelican", "iphone", "japan_pm"}
	invoke(t, s, c)
	waitFor(t, func() bool { return len(started) == 3 })
	invoke(t, s, testCommand(s, "clear_all"))
	released.Do(func() { close(release) })
	waitFor(t, func() bool { x := getSnapshot(s); return len(x.Active) == 0 && x.Busy == "" && x.Bytes == 0 })
	invoke(t, s, c) // stale saved command cannot recreate data after clear
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.values) != 0 {
		t.Fatal("clear left data", h.values)
	}
}

func TestStopCancelsAllParallelRequestsAndKeepsPartialAnswers(t *testing.T) {
	started, stopped := make(chan struct{}, 3), make(chan struct{}, 3)
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	})
	c := testCommand(s, "start")
	stop := testCommand(s, "stop")
	stop.BatchID = c.ID
	defer func() { invoke(t, s, stop) }()
	c.Prompts, c.Rounds = []string{"pelican", "iphone", "japan_pm"}, 2
	unlimited := 0
	c.TimeoutMinutes = &unlimited
	invoke(t, s, c)
	waitFor(t, func() bool { return len(started) == 3 })
	invoke(t, s, stop)
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 && len(stopped) == 3 })
	var rec Record
	_ = json.Unmarshal(invoke(t, s, testCommand(s, "read_batch")), &rec)
	if rec.Latest.State != "interrupted" || *rec.Latest.TimeoutMinutes != 0 {
		t.Fatal("parallel stop or unlimited setting lost", rec)
	}
	for i, item := range rec.Latest.Items {
		if item.State != "interrupted" || (i < 3 && item.Parts == 0) || (i >= 3 && item.Parts != 0) {
			t.Fatal("unexpected stopped item", i, item)
		}
		if i < 3 && !strings.Contains(item.Error, "测试已停止") {
			t.Fatal("manual stop mislabeled as timeout", item.Error)
		}
	}
}

func TestSlowPelicanDoesNotBlockOtherPromptsOrTheirRounds(t *testing.T) {
	release := make(chan struct{})
	var released sync.Once
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []struct{ Content []struct{ Text string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		pelican, _ := findPrompt("pelican")
		if body.Input[0].Content[0].Text == pelican.Text {
			<-release
		}
		completed(w, "independent answer")
	})
	defer released.Do(func() { close(release) })
	c := testCommand(s, "start")
	c.Prompts, c.Rounds = []string{"pelican", "iphone", "japan_pm"}, 2
	invoke(t, s, c)
	waitFor(t, func() bool { return getSnapshot(s).Results == 4 })
	active := activeBatch(s, 7)
	if active == nil || active.Items[0].State != "running" || active.Items[3].State != "queued" {
		t.Fatal("slow prompt blocked others or overlapped its own rounds", active)
	}
	read := testCommand(s, "read_result")
	read.BatchID, read.Item = c.ID, 1
	invoke(t, s, read) // A finished text answer is readable before the pelican ends.
	released.Do(func() { close(release) })
	waitFor(t, func() bool { x := getSnapshot(s); return len(x.Active) == 0 && x.Results == 6 })
}

func TestConfigAndHealthCannotStartRequests(t *testing.T) {
	s, h := testServer(t, nil)
	c := testCommand(s, "start")
	raw, _ := json.Marshal(Config{Command: &c})
	before := h.calls.Load()
	for i := 0; i < 3; i++ {
		if r, _ := s.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: raw}); !r.Applied {
			t.Fatal(r)
		}
		if r, _ := s.Health(context.Background(), &pluginv1.HealthRequest{}); !r.Healthy {
			t.Fatal(r)
		}
	}
	if h.calls.Load() != before || len(getSnapshot(s).Active) != 0 {
		t.Fatal("passive method has side effects")
	}
	c.Effort = ""
	raw, _ = json.Marshal(Config{Command: &c})
	if r, _ := s.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: raw}); r.Valid {
		t.Fatal("blank effort accepted")
	}
	c.Effort = "none"
	raw, _ = json.Marshal(Config{Command: &c})
	if r, _ := s.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: raw}); !r.Valid {
		t.Fatal("explicit none rejected")
	}
	c.Instance = newID()
	raw, _ = json.Marshal(Config{Command: &c})
	if r, _ := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: raw}); r.Success {
		t.Fatal("stale instance accepted")
	}
}

func TestCatalogUsesAccountIdentityWithoutDefaults(t *testing.T) {
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_version") != "0.144.0" || r.Header.Get("Authorization") != "Bearer fixture-access-token" {
			t.Error("identity missing")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"slug":"fixture-model","default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"none"},{"effort":"high"}]}]}`)
	})
	var models []Model
	_ = json.Unmarshal(invoke(t, s, testCommand(s, "models")), &models)
	if len(models) != 1 || strings.Join(models[0].Efforts, ",") != "none,high" {
		t.Fatal(models)
	}
}
