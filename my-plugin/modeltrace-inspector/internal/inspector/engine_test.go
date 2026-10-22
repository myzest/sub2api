package inspector

import (
	"context"
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
	pluginv1 "local.sub2api/modeltrace-inspector/internal/pluginapi"
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
	return Command{ID: newID(), Instance: s.instance, IssuedAt: time.Now().Unix(), Action: action, Selection: Selection{AccountID: 7, Model: "fixture-model", Effort: "high", Rounds: 1}}
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
	waitForTimeout(t, 5*time.Second, condition)
}
func waitForTimeout(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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

// fixturePrompt is used only by transport/parser tests. Production challenges
// are generated independently for every batch and round.
func fixturePrompt() Prompt {
	return Prompt{Text: "Generate 310 integers between 1 and 355 without tools.", ExpectedCount: 310}
}

func fixtureNumbers(seed int) string {
	values := make([]string, 310)
	for i := range values {
		values[i] = fmt.Sprint((i*i*17+i*31+seed*13)%355 + 1)
	}
	return strings.Join(values, ",")
}

func readRecord(t *testing.T, s *Server, id int64) Record {
	t.Helper()
	command := testCommand(s, "read_batch")
	command.AccountID = id
	var record Record
	if err := json.Unmarshal(invoke(t, s, command), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestParallelChallengesFreshSessionsFailureAndLatestBatch(t *testing.T) {
	var calls, inFlight, maximum atomic.Int64
	var mu sync.Mutex
	sessions := map[string]bool{}
	arrived, release := make(chan struct{}, 3), make(chan struct{})
	var released sync.Once
	s, h := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		active := inFlight.Add(1)
		defer inFlight.Add(-1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["previous_response_id"] != nil || body["store"] != false || body["tool_choice"] != "none" || len(body["tools"].([]any)) != 0 || len(body["input"].([]any)) != 1 {
			t.Error("history or tools leaked into request")
		}
		if body["instructions"] != "" || body["temperature"] != nil || body["seed"] != nil {
			t.Error("fingerprint request added an untracked generation condition")
		}
		for key, want := range map[string]string{"Authorization": "Bearer fixture-access-token", "User-Agent": "codex_cli_rs/0.144.0", "Originator": "codex_cli_rs", "Version": "0.144.0", "Chatgpt-Account-Id": "fixture-chatgpt-account", "Openai-Beta": "responses=experimental"} {
			if r.Header.Get(key) != want {
				t.Errorf("resolved Codex identity header %s was not preserved", key)
			}
		}
		session := r.Header.Get("Session_id")
		mu.Lock()
		if active > maximum.Load() {
			maximum.Store(active)
		}
		if sessions[session] || session == "host-fixed-session" || !uuidPattern.MatchString(session) {
			t.Error("session reused")
		}
		sessions[session] = true
		mu.Unlock()
		if body["prompt_cache_key"] != session || r.Header.Get("Conversation_id") != session {
			t.Error("request identity mismatch")
		}
		if n <= 3 {
			arrived <- struct{}{}
			<-release
		}
		if n == 2 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"fixture rate limit"}}`)
			return
		}
		completed(w, fixtureNumbers(int(n)))
	})
	defer released.Do(func() { close(release) })
	c := testCommand(s, "start")
	c.Rounds = 2
	requestCtx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(Config{Command: &c})
	reply, err := s.TestConfig(requestCtx, &pluginv1.TestConfigRequest{ConfigJson: raw})
	if err != nil || !reply.Success {
		t.Fatal(reply, err)
	}
	cancel() // Closing the UI/start request must not cancel accepted work.
	waitFor(t, func() bool { return len(arrived) == 3 })
	active := activeBatch(s, 7)
	if active == nil || len(active.Items) != 6 {
		t.Fatal("missing challenge items", active)
	}
	counts := map[int]bool{}
	for i, item := range active.Items {
		if item.Prompt.ExpectedCount < 292 || item.Prompt.ExpectedCount > 332 || item.Probe != i%3+1 || item.Round != i/3+1 {
			t.Fatal("invalid generated challenge", item)
		}
		if i < 3 {
			if item.State != "running" || counts[item.Prompt.ExpectedCount] {
				t.Fatal("first round not independent and concurrent", item)
			}
			counts[item.Prompt.ExpectedCount] = true
		} else if item.State != "queued" {
			t.Fatal("later round started early")
		}
	}
	released.Do(func() { close(release) })
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	if calls.Load() != 6 || maximum.Load() != 3 {
		t.Fatalf("calls=%d concurrency=%d", calls.Load(), maximum.Load())
	}
	rec := readRecord(t, s, 7)
	if rec.Pending != nil || rec.Latest == nil || rec.Latest.State != "completed" || len(rec.Latest.Analyses) != 2 {
		t.Fatalf("unexpected record %+v", rec)
	}
	if rec.Latest.Analyses[0].State != "partial" || rec.Latest.Analyses[0].Result.UsedOutputs != 2 || rec.Latest.Analyses[1].State != "complete" || rec.Latest.Analyses[1].Result.UsedOutputs != 3 {
		t.Fatal("HTTP failure counted as a valid fingerprint", rec.Latest.Analyses)
	}
	for _, analysis := range rec.Latest.Analyses {
		if analysis.Covered {
			t.Fatal("unlisted request model was marked covered")
		}
	}
	before := getSnapshot(s).Bytes
	var actual int64
	h.mu.Lock()
	for _, value := range h.values {
		actual += int64(len(value))
	}
	h.mu.Unlock()
	if before != actual {
		t.Fatal("storage count drift", before, actual)
	}
	invoke(t, s, c)
	if calls.Load() != 6 {
		t.Fatal("duplicate execution")
	}
	invoke(t, s, testCommand(s, "start"))
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
	invoke(t, s, c)
	waitFor(t, func() bool { return len(started) == 3 })
	invoke(t, s, testCommand(s, "clear_all"))
	released.Do(func() { close(release) })
	waitFor(t, func() bool { x := getSnapshot(s); return len(x.Active) == 0 && x.Busy == "" && x.Bytes == 0 })
	invoke(t, s, c)
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
	c.Rounds = 2
	stop := testCommand(s, "stop")
	stop.BatchID = c.ID
	defer func() { invoke(t, s, stop) }()
	unlimited := 0
	c.TimeoutMinutes = &unlimited
	invoke(t, s, c)
	waitFor(t, func() bool { return len(started) == 3 })
	invoke(t, s, stop)
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 && len(stopped) == 3 })
	rec := readRecord(t, s, 7)
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

func TestNextRoundWaitsForAllThreeChallenges(t *testing.T) {
	release := make(chan struct{})
	var released sync.Once
	var calls atomic.Int64
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-release
		}
		completed(w, fixtureNumbers(1))
	})
	defer released.Do(func() { close(release) })
	c := testCommand(s, "start")
	c.Rounds = 2
	invoke(t, s, c)
	waitFor(t, func() bool { return getSnapshot(s).Results == 2 })
	active := activeBatch(s, 7)
	if active == nil || calls.Load() != 3 {
		t.Fatal("later round did not wait", active)
	}
	for _, item := range active.Items[3:] {
		if item.State != "queued" {
			t.Fatal("later round started while previous sample was pending", item)
		}
	}
	for i, item := range active.Items[:3] {
		if item.Parts > 0 {
			savedAnswer(t, s, 7, c.ID, i)
		}
	}
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

func TestNotReadyErrorIsSynchronizedWithBackgroundFailures(t *testing.T) {
	s, _ := testServer(t, nil)
	s.ready, s.lastError = false, "fixture storage failure"
	stop, finished := make(chan struct{}), make(chan struct{})
	// Another account may finish while a failed clear has left storage unready.
	// Exercise its synchronized error publication against incoming commands.
	go func() {
		defer close(finished)
		for {
			select {
			case <-stop:
				return
			default:
				s.mu.Lock()
				s.lastError = "fixture background batch commit failure"
				s.mu.Unlock()
			}
		}
	}()
	defer func() { close(stop); <-finished }()
	for range 1000 {
		command := testCommand(s, "read_batch")
		raw, _ := json.Marshal(Config{Command: &command})
		reply, err := s.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: raw})
		if err != nil || reply.Success || !strings.HasPrefix(reply.Message, "测试存储未就绪：fixture ") {
			t.Fatal("unready command lost its storage error", reply, err)
		}
	}
}
