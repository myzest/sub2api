package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"local.sub2api/codex-inspector/internal/detect"
	"local.sub2api/codex-inspector/internal/diag"
	"local.sub2api/codex-inspector/internal/kv"
	"local.sub2api/codex-inspector/internal/modeltrace"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/pluginconfig"
	"local.sub2api/codex-inspector/internal/transport"
	"local.sub2api/codex-inspector/internal/tzresolve"
)

const Capability = "openai.oauth.outbound_transport.v1"
const kvTimeout = 500 * time.Millisecond
const snapshotCacheTTL = 5 * time.Second

type hostBinding struct{ client pluginv1.HostServiceClient }
type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	cfg              atomic.Pointer[pluginconfig.Config]
	host             atomic.Pointer[hostBinding]
	hostReady        atomic.Bool
	hostAPIVersion   atomic.Uint32
	broker           *hcplugin.GRPCBroker
	initMu           sync.Mutex
	conn             *grpc.ClientConn
	pool             *transport.Pool
	kv               *kvSwitch
	diag             *diag.Recorder
	tz               *tzresolve.Resolver
	holder, instance string
	now              func() time.Time
	ctx              context.Context
	cancel           context.CancelFunc
	bg, loops        sync.WaitGroup
	runOnce          sync.Once
	snapMu           sync.Mutex
	snapAt           time.Time
	snapJSON         string
	detectMu         sync.Mutex
	busyTask         string
	dispatch         map[string]any
	runTask          func(context.Context, detect.Task) // injected only by local tests
}

func New() *Server {
	host, _ := os.Hostname()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		panic("cannot create plugin instance ID")
	}
	instance := hex.EncodeToString(random)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{pool: transport.NewPool(), kv: &kvSwitch{current: disabledKV{}}, instance: instance, holder: fmt.Sprintf("%s-%d-%s", host, os.Getpid(), instance), now: time.Now, ctx: ctx, cancel: cancel}
	c := pluginconfig.Default()
	s.cfg.Store(&c)
	s.diag = diag.New(s.holder, func() time.Time { return s.now() })
	s.tz = &tzresolve.Resolver{KV: s.kv, Pool: s.pool, Diag: s.diag}
	return s
}
func (s *Server) Close() {
	s.cancel()
	s.bg.Wait()
	s.loops.Wait()
	s.pool.CloseIdle()
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
}
func (s *Server) SetHostBroker(b *hcplugin.GRPCBroker) { s.broker = b }
func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{PluginId: PluginID, PluginVersion: Version, ProtocolVersion: pluginv1.ProtocolVersion, TransportApiVersion: pluginv1.TransportAPIVersion, Capabilities: []string{Capability}}, nil
}
func (s *Server) ValidateConfig(_ context.Context, req *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	c, e := pluginconfig.Parse(req.GetConfigJson())
	if e != nil {
		return &pluginv1.ValidateConfigResponse{Message: diag.SanitizeError(e.Error())}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: c.Marshal()}, nil
}
func (s *Server) ApplyConfig(_ context.Context, req *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	c, e := pluginconfig.Parse(req.GetConfigJson())
	if e != nil {
		return &pluginv1.ApplyConfigResponse{Message: diag.SanitizeError(e.Error())}, nil
	}
	s.cfg.Store(&c)
	s.pool.CloseIdle()
	s.invalidateSnapshot()
	// The host calls Apply before DB persistence, during rollback, startup and
	// temporary validation. Only explicit TestConfig may consume detection tasks.
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}
func (s *Server) InitHostServices(_ context.Context, req *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.hostReady.Load() {
		return &pluginv1.InitHostServicesResponse{Ready: true}, nil
	}
	if req.GetHostServiceApiVersion() < 2 || s.broker == nil {
		return &pluginv1.InitHostServicesResponse{Message: "检测需要 HostService v2；普通转发仍可用"}, nil
	}
	conn, e := s.broker.Dial(req.GetHostServiceId())
	if e != nil {
		s.diag.Tally("host_services", "unavailable")
		return &pluginv1.InitHostServicesResponse{Message: "连接宿主服务失败；普通转发仍可用"}, nil
	}
	s.conn = conn
	client := pluginv1.NewHostServiceClient(conn)
	s.kv.swap(kv.NewHostKV(client))
	s.host.Store(&hostBinding{client})
	s.hostAPIVersion.Store(req.HostServiceApiVersion)
	s.hostReady.Store(true)
	s.invalidateSnapshot()
	s.runOnce.Do(func() {
		s.loops.Add(1)
		go func() {
			defer s.loops.Done()
			ticker := time.NewTicker(diag.PublishInterval)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(s.ctx, kvTimeout)
					_, _ = s.diag.Publish(ctx, s.kv)
					cancel()
				}
			}
		}()
	})
	return &pluginv1.InitHostServicesResponse{Ready: true}, nil
}
func (s *Server) TestConfig(ctx context.Context, req *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	c, e := pluginconfig.Parse(req.GetConfigJson())
	if e != nil {
		return &pluginv1.TestConfigResponse{Message: diag.SanitizeError(e.Error())}, nil
	}
	if c.Detect.TaskID == "" {
		return &pluginv1.TestConfigResponse{Success: true, Message: "codex-inspector ready（未发起网络探测）"}, nil
	}
	if !strings.HasPrefix(c.Detect.TaskID, "d"+s.instance+"-") {
		return &pluginv1.TestConfigResponse{Message: "插件进程已变化或未启用，请刷新状态后重新开始检测"}, nil
	}
	created, e := time.Parse(time.RFC3339, c.Detect.CreatedAt)
	age := s.now().Sub(created)
	if e != nil || age > 30*time.Minute || age < -30*time.Minute {
		return &pluginv1.TestConfigResponse{Message: "检测任务已过期，请重新开始"}, nil
	}
	if !s.hostReady.Load() || s.host.Load() == nil {
		return &pluginv1.TestConfigResponse{Message: "宿主账号与存储服务尚未就绪"}, nil
	}
	s.detectMu.Lock()
	defer s.detectMu.Unlock()
	if s.busyTask != "" && s.busyTask != c.Detect.TaskID {
		return &pluginv1.TestConfigResponse{Message: "已有检测任务执行中，请等待完成"}, nil
	}
	store := &detect.Store{KV: s.kv}
	readCtx, cancel := context.WithTimeout(ctx, kvTimeout)
	executed, e := store.AlreadyExecuted(readCtx, c.Detect.TaskID)
	cancel()
	if e != nil {
		return &pluginv1.TestConfigResponse{Message: "无法确认任务执行状态；未发起检测"}, nil
	}
	id := c.Detect.TaskID
	if !executed && s.busyTask == "" {
		s.busyTask = id
		s.dispatch = map[string]any{"task_id": id, "state": "queued"}
		task := detect.Task{ID: id, CreatedAt: c.Detect.CreatedAt, AccountIDs: append([]int64(nil), c.Detect.AccountIDs...), Models: append([]string(nil), c.Detect.Models...), Repeats: c.Detect.Repeats, Concurrency: c.Detect.Concurrency}
		s.goDetect(func(runCtx context.Context) {
			if s.runTask != nil {
				s.runTask(runCtx, task)
			} else {
				r := &detect.Runner{Store: store, Lease: kv.Lease{KV: s.kv, NS: "detect", Key: "lease"}, Pool: s.pool, Bank: modeltrace.LoadBank(), Host: s.host.Load().client, Diag: s.diag, Holder: s.holder, Now: s.now}
				r.Run(runCtx, task)
			}
			checkCtx, checkCancel := context.WithTimeout(s.ctx, kvTimeout)
			saved, ok, err := store.LoadTask(checkCtx, id)
			checkCancel()
			state := "not_started"
			if err == nil && ok {
				state = "interrupted"
				if saved.Done {
					state = "done"
				}
			}
			s.detectMu.Lock()
			s.busyTask = ""
			s.dispatch = map[string]any{"task_id": id, "state": state}
			s.detectMu.Unlock()
			s.invalidateSnapshot()
		})
	}
	raw, _ := json.Marshal(map[string]any{"task_id": id, "accepted": true, "already_executed": executed})
	return &pluginv1.TestConfigResponse{Success: true, Message: "检测任务已受理；进度以状态页为准", StatusJson: string(raw)}, nil
}
func (s *Server) goDetect(fn func(context.Context)) {
	s.bg.Add(1)
	go func() { defer s.bg.Done(); fn(s.ctx) }()
}
func (s *Server) waitBackground()     { s.bg.Wait() }
func (s *Server) invalidateSnapshot() { s.snapMu.Lock(); s.snapJSON = ""; s.snapMu.Unlock() }
func (s *Server) Health(ctx context.Context, _ *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	return &pluginv1.HealthResponse{Healthy: true, Message: "Codex Inspector 运行中", StatusJson: s.cachedSnapshot(ctx)}, nil
}
func (s *Server) cachedSnapshot(ctx context.Context) string {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	now := s.now()
	if s.snapJSON != "" && now.Sub(s.snapAt) >= 0 && now.Sub(s.snapAt) < snapshotCacheTTL {
		return s.snapJSON
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c := s.cfg.Load()
	status := map[string]any{"version": Version, "instance": s.instance, "enabled": c.Enabled, "timezone": map[string]any{"mode": c.Timezone.Mode}, "host_services": "degraded", "timezones": pluginconfig.Whitelist, "models": pluginconfig.SelectableModels, "accounts": []any{}, "events": []diag.Event{}, "detect": nil}
	type account struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Schedulable bool   `json:"schedulable"`
	}
	if binding := s.host.Load(); binding != nil && s.hostReady.Load() {
		status["host_services"] = "ready"
		accounts, e := binding.client.ListAccounts(snapshotCtx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
		if e != nil || accounts == nil {
			status["accounts_error"] = "读取账号目录失败"
		} else {
			safe := []account{}
			for _, a := range accounts.Accounts {
				if a != nil {
					safe = append(safe, account{a.Id, a.Name, a.Schedulable})
				}
			}
			status["accounts"] = safe
		}
	}
	events, replicas, e := diag.Merge(snapshotCtx, s.kv)
	if e != nil {
		status["storage_error"] = "状态存储不可用"
	}
	merged := []diag.Event{}
	for _, ev := range events {
		if ev.Host != s.holder {
			merged = append(merged, ev)
		}
	}
	merged = append(merged, s.diag.Events()...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].At.After(merged[j].At) })
	if len(merged) > diag.MergedLimit {
		merged = merged[:diag.MergedLimit]
	}
	status["events"] = merged
	status["replicas"] = replicas
	task, found, err := (&detect.Store{KV: s.kv}).LoadLatest(snapshotCtx)
	if err == nil && found {
		status["detect"] = map[string]any{"task_id": task.ID, "state": detectState(task, now), "completed": len(task.Results), "total": len(task.AccountIDs) * len(task.Models), "results": task.Results, "started_at": task.StartedAt, "heartbeat_at": task.HeartbeatAt, "done": task.Done}
	}
	s.detectMu.Lock()
	if s.dispatch != nil {
		d := map[string]any{}
		for k, v := range s.dispatch {
			d[k] = v
		}
		status["dispatch"] = d
		if found && task.ID == d["task_id"] && d["state"] == "interrupted" {
			status["detect"].(map[string]any)["state"] = "interrupted"
		}
	}
	s.detectMu.Unlock()
	raw, _ := json.Marshal(status)
	s.snapJSON = string(raw)
	s.snapAt = now
	return s.snapJSON
}
func detectState(t detect.Task, now time.Time) string {
	if t.Done {
		return "done"
	}
	if t.HeartbeatAt.IsZero() || now.Sub(t.HeartbeatAt) > 90*time.Second {
		return "interrupted"
	}
	return "running"
}

type kvSwitch struct {
	mu      sync.RWMutex
	current kv.KV
}

func (k *kvSwitch) swap(next kv.KV) { k.mu.Lock(); k.current = next; k.mu.Unlock() }
func (k *kvSwitch) get() kv.KV      { k.mu.RLock(); defer k.mu.RUnlock(); return k.current }
func (k *kvSwitch) Get(ctx context.Context, ns, key string) ([]byte, bool, error) {
	return k.get().Get(ctx, ns, key)
}
func (k *kvSwitch) Set(ctx context.Context, ns, key string, value []byte, ttl time.Duration) error {
	return k.get().Set(ctx, ns, key, value, ttl)
}
func (k *kvSwitch) Delete(ctx context.Context, ns, key string) error {
	return k.get().Delete(ctx, ns, key)
}
func (k *kvSwitch) List(ctx context.Context, ns, prefix string, limit int) ([]string, error) {
	return k.get().List(ctx, ns, prefix, limit)
}

type disabledKV struct{}

var errHostNotReady = errors.New("host storage not ready")

func (disabledKV) Get(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, errHostNotReady
}
func (disabledKV) Set(context.Context, string, string, []byte, time.Duration) error {
	return errHostNotReady
}
func (disabledKV) Delete(context.Context, string, string) error { return errHostNotReady }
func (disabledKV) List(context.Context, string, string, int) ([]string, error) {
	return nil, errHostNotReady
}
