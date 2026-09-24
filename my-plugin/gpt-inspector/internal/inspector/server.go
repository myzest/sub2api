package inspector

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

type commandReply struct {
	fingerprint string
	at          int64
	value       any
}

type runningBatch struct {
	batch  *Batch
	cancel context.CancelFunc
}

type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	broker       *hcplugin.GRPCBroker
	conn         *grpc.ClientConn
	host         pluginv1.HostServiceClient
	mu           sync.Mutex
	commands     sync.Mutex
	instance     string
	ready        bool
	busy         string
	lastError    string
	accounts     []Account
	catalogs     map[int64][]Model
	records      map[int64]*Record
	jobs         map[int64]*runningBatch
	store        *store
	seen         map[string]commandReply
	revision     uint64
	snapshot     atomic.Value
	pool         transportPool
	modelsURL    string
	responsesURL string
}

func New() *Server {
	s := &Server{instance: newID(), catalogs: map[int64][]Model{}, records: map[int64]*Record{}, jobs: map[int64]*runningBatch{}, seen: map[string]commandReply{},
		modelsURL: "https://chatgpt.com/backend-api/codex/models", responsesURL: "https://chatgpt.com/backend-api/codex/responses"}
	s.publishLocked()
	return s
}
func (s *Server) SetHostBroker(b *hcplugin.GRPCBroker) { s.broker = b }
func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{PluginId: PluginID, PluginVersion: Version, ProtocolVersion: 1, TransportApiVersion: 1, Capabilities: []string{Capability}}, nil
}
func (s *Server) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	return &pluginv1.HealthResponse{Healthy: true, Message: "GPT Inspector 运行中", StatusJson: s.snapshot.Load().(string)}, nil
}
func (s *Server) ValidateConfig(_ context.Context, req *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	cfg, err := parseConfig(req.ConfigJson)
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Message: err.Error()}, nil
	}
	raw, _ := json.Marshal(cfg)
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: raw}, nil
}
func (s *Server) ApplyConfig(_ context.Context, req *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	_, err := parseConfig(req.ConfigJson)
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	// Apply is deliberately side-effect free: the host replays it on startup,
	// validation, TestConfig, and rollback. Only explicit TestConfig dispatches.
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}
func (s *Server) InitHostServices(_ context.Context, req *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	if req.HostServiceApiVersion < 2 || s.broker == nil {
		s.mu.Lock()
		s.lastError = "当前宿主未提供 HostService v2，测试功能无法使用"
		s.publishLocked()
		s.mu.Unlock()
		return &pluginv1.InitHostServicesResponse{Message: "测试功能要求 HostService v2"}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host != nil {
		return &pluginv1.InitHostServicesResponse{Ready: true}, nil
	}
	conn, err := s.broker.Dial(req.HostServiceId)
	if err != nil {
		s.lastError = "连接宿主账号与存储服务失败"
		s.publishLocked()
		return nil, err
	}
	s.conn, s.host = conn, pluginv1.NewHostServiceClient(conn)
	s.store = &store{host: s.host, sizes: map[string]int64{}}
	s.busy = "正在读取保存的结果"
	s.publishLocked()
	go s.bootstrap()
	return &pluginv1.InitHostServicesResponse{Ready: true}, nil
}
func (s *Server) TestConfig(ctx context.Context, req *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	started := time.Now()
	cfg, err := parseConfig(req.ConfigJson)
	var value any
	var id string
	if err == nil && cfg.Command == nil {
		err = errors.New("请启用插件并在插件弹窗内选择操作")
	}
	if err == nil {
		id = cfg.Command.ID
		s.commands.Lock()
		value, err = s.dispatch(ctx, *cfg.Command)
		s.commands.Unlock()
	}
	message := "操作完成"
	if err != nil {
		message = err.Error()
	}
	raw, marshalErr := json.Marshal(map[string]any{"command_id": id, "data": value})
	if marshalErr != nil {
		return nil, marshalErr
	}
	return &pluginv1.TestConfigResponse{Success: err == nil, Message: message, StatusJson: string(raw), LatencyMs: time.Since(started).Milliseconds()}, nil
}
func (s *Server) dispatch(ctx context.Context, c Command) (any, error) {
	if c.Instance != s.instance {
		return nil, errors.New("插件进程已变化，请重新打开弹窗")
	}
	if age := time.Now().Unix() - c.IssuedAt; age < -30 || age > 300 {
		return nil, errors.New("操作已过期，请重试")
	}
	fingerprint, _ := json.Marshal(c)
	for id, entry := range s.seen {
		if time.Now().Unix()-entry.at > 300 {
			delete(s.seen, id)
		}
	}
	if old, ok := s.seen[c.ID]; ok {
		if old.fingerprint != string(fingerprint) {
			return nil, errors.New("命令 ID 已被其他操作使用")
		}
		return old.value, nil
	}
	s.mu.Lock()
	if s.host == nil || s.store == nil {
		s.mu.Unlock()
		return nil, errors.New("宿主账号与存储服务尚未连接")
	}
	if s.busy != "" {
		busy := s.busy
		s.mu.Unlock()
		return nil, errors.New(busy + "，请稍后重试")
	}
	if !s.ready && c.Action != "clear_all" {
		s.mu.Unlock()
		return nil, errors.New("测试存储未就绪：" + s.lastError)
	}
	s.mu.Unlock()
	var value any
	var err error
	switch c.Action {
	case "accounts":
		var accounts []Account
		accounts, err = s.listAccounts(ctx)
		if err == nil {
			s.mu.Lock()
			s.accounts = accounts
			if s.lastError == "读取宿主账号目录失败" {
				s.lastError = ""
			}
			s.publishLocked()
			s.mu.Unlock()
			value = accounts
		}
	case "models":
		var models []Model
		models, err = s.fetchModels(ctx, c.AccountID)
		if err == nil {
			s.mu.Lock()
			s.catalogs[c.AccountID] = models
			s.mu.Unlock()
			value = models
		}
	case "start":
		value, err = s.start(ctx, c)
	case "stop":
		// Keep old saved stop configs valid during upgrade, but never dispatch
		// an unscoped stop now that multiple accounts can run at once.
		if c.AccountID <= 0 || !uuidPattern.MatchString(c.BatchID) {
			err = errors.New("请选择要停止的账号批次，请刷新任务列表后重试")
			break
		}
		s.mu.Lock()
		if job := s.jobs[c.AccountID]; job != nil {
			if job.batch.ID != c.BatchID {
				err = errors.New("该账号的运行批次已变化，请刷新后选择要停止的任务")
			} else {
				job.batch.State = "stopping"
				job.cancel()
				s.publishLocked()
			}
		}
		s.mu.Unlock()
		value = map[string]bool{"stopping": true}
	case "read_batch":
		value, err = s.readBatch(c.AccountID)
	case "read_result":
		value, err = s.readResult(ctx, c)
	case "clear_account", "clear_all":
		value, err = s.beginClear(ctx, c)
	}
	if err == nil && (c.Action == "start" || c.Action == "stop" || c.Action == "clear_account" || c.Action == "clear_all") {
		s.seen[c.ID] = commandReply{string(fingerprint), c.IssuedAt, value}
	}
	return value, err
}
