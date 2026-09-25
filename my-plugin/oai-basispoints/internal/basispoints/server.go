package basispoints

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

type Account struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Schedulable bool   `json:"schedulable"`
}
type Probe struct {
	ID                string             `json:"id"`
	AccountID         int64              `json:"account_id"`
	Model             string             `json:"model"`
	Effort            string             `json:"effort"`
	State             string             `json:"state"`
	Message           string             `json:"message"`
	HTTPStatus        int                `json:"http_status,omitempty"`
	ReturnedModel     string             `json:"returned_model,omitempty"`
	ResponseID        string             `json:"response_id,omitempty"`
	Usage             any                `json:"usage,omitempty"`
	LatencyMS         int64              `json:"latency_ms,omitempty"`
	FinishedAt        int64              `json:"finished_at,omitempty"`
	Kind              string             `json:"kind,omitempty"`
	Stage             string             `json:"stage,omitempty"`
	ContentType       string             `json:"content_type,omitempty"`
	RequestID         string             `json:"request_id,omitempty"`
	RequestBytes      int                `json:"request_bytes,omitempty"`
	UpstreamError     string             `json:"upstream_error,omitempty"`
	ImageDetail       string             `json:"image_detail,omitempty"`
	ImageFormat       string             `json:"image_format,omitempty"`
	ImagePreview      string             `json:"image_preview,omitempty"`
	ImageExpected     string             `json:"image_expected,omitempty"`
	ImageReply        string             `json:"image_reply,omitempty"`
	ImageTransport    string             `json:"image_transport,omitempty"`
	Attachment        *attachmentReport  `json:"attachment,omitempty"`
	Round             int                `json:"round,omitempty"`
	Rounds            []probeRoundResult `json:"rounds,omitempty"`
	ToolName          string             `json:"tool_name,omitempty"`
	ToolType          string             `json:"tool_type,omitempty"`
	ToolSource        string             `json:"tool_source,omitempty"`
	ToolCalls         int                `json:"tool_calls,omitempty"`
	ToolExpected      string             `json:"tool_expected,omitempty"`
	ToolReply         string             `json:"tool_reply,omitempty"`
	Relay             []relayDiagnostic  `json:"relay,omitempty"`
	Replay            *replayDiagnostic  `json:"replay,omitempty"`
	RouteDiagnosticID string             `json:"route_diagnostic_id,omitempty"`
}
type Snapshot struct {
	Instance             string               `json:"instance"`
	Version              string               `json:"version"`
	HostReady            bool                 `json:"host_ready"`
	Config               Config               `json:"config"`
	Accounts             []Account            `json:"accounts"`
	LastError            string               `json:"last_error,omitempty"`
	Probe                *Probe               `json:"probe,omitempty"`
	LastRequest          *requestDiagnostic   `json:"last_request,omitempty"`
	RecentRequests       []*requestDiagnostic `json:"recent_requests,omitempty"`
	ImageRequests        []*requestDiagnostic `json:"image_requests,omitempty"`
	DiagnosticGeneration uint64               `json:"diagnostic_generation"`
	DiagnosticsClearedAt int64                `json:"diagnostics_cleared_at,omitempty"`
}
type commandResult struct {
	fingerprint string
	at          int64
	value       any
	err         error
}

type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	broker               *hcplugin.GRPCBroker
	conn                 *grpc.ClientConn
	mu                   sync.RWMutex
	host                 pluginv1.HostServiceClient
	cfg                  Config
	instance             string
	accounts             []Account
	lastError            string
	probe                *Probe
	lastRequest          *requestDiagnostic
	recentRequests       []*requestDiagnostic
	imageRequests        []*requestDiagnostic
	diagnosticGeneration uint64
	diagnosticsClearedAt int64
	commands             sync.Mutex
	seen                 map[string]commandResult
	pool                 transportPool
	attachments          attachmentCache
	responsesURL         string // Fixed in production; only package tests may substitute a fixture.
}

func newID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func New() *Server {
	return &Server{cfg: defaultConfig(), instance: newID(), accounts: []Account{}, seen: map[string]commandResult{}, responsesURL: bpsURL}
}
func (s *Server) SetHostBroker(b *hcplugin.GRPCBroker) { s.broker = b }
func (s *Server) hostClient() pluginv1.HostServiceClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.host
}
func (s *Server) config() Config { s.mu.RLock(); defer s.mu.RUnlock(); return s.cfg }
func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{PluginId: PluginID, PluginVersion: Version, ProtocolVersion: 1, TransportApiVersion: 1, Capabilities: []string{Capability}}, nil
}
func (s *Server) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	c.Command = nil
	raw := encoded(Snapshot{Instance: s.instance, Version: Version, HostReady: s.host != nil, Config: c, Accounts: s.accounts, LastError: s.lastError, Probe: s.probe, LastRequest: s.lastRequest, RecentRequests: s.recentRequests, ImageRequests: s.imageRequests, DiagnosticGeneration: s.diagnosticGeneration, DiagnosticsClearedAt: s.diagnosticsClearedAt})
	return &pluginv1.HealthResponse{Healthy: true, Message: "Basis Points 插件运行中", StatusJson: string(raw)}, nil
}
func (s *Server) ValidateConfig(_ context.Context, r *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	c, err := parseConfig(r.ConfigJson)
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Message: err.Error()}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: encoded(c)}, nil
}
func (s *Server) ApplyConfig(_ context.Context, r *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	c, err := parseConfig(r.ConfigJson)
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	c.Command = nil // Applying/replaying saved config must never start an upstream probe.
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}
func (s *Server) InitHostServices(_ context.Context, r *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	if r.HostServiceApiVersion < 2 || s.broker == nil {
		return &pluginv1.InitHostServicesResponse{Message: "需要 HostService v2 才能列出账号、探测和回放工具"}, nil
	}
	s.mu.Lock()
	if s.host != nil {
		s.mu.Unlock()
		return &pluginv1.InitHostServicesResponse{Ready: true}, nil
	}
	conn, err := s.broker.Dial(r.HostServiceId)
	if err != nil {
		s.mu.Unlock()
		return &pluginv1.InitHostServicesResponse{Message: "宿主服务连接失败"}, nil
	}
	s.conn, s.host = conn, pluginv1.NewHostServiceClient(conn)
	s.mu.Unlock()
	go func() { _, _ = s.listAccounts(context.Background()) }()
	return &pluginv1.InitHostServicesResponse{Ready: true}, nil
}
func (s *Server) listAccounts(parent context.Context) ([]Account, error) {
	host := s.hostClient()
	if host == nil {
		return nil, errors.New("HostService v2 尚未连接")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	r, err := host.ListAccounts(ctx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	accounts := []Account{}
	if err == nil {
		for _, a := range r.GetAccounts() {
			if a.Id > 0 && a.Platform == "openai" && a.AccountType == "oauth" && a.Status == "active" && !a.IsShadow {
				accounts = append(accounts, Account{a.Id, a.Name, a.Schedulable})
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastError = "读取宿主账号目录失败"
		return nil, errors.New(s.lastError)
	}
	s.accounts, s.lastError = accounts, ""
	return accounts, nil
}
func (s *Server) identity(ctx context.Context, id int64) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	accounts, err := s.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, a := range accounts {
		if a.ID == id {
			ok = a.Schedulable
			break
		}
	}
	if !ok {
		return nil, errors.New("账号不可用或已暂停调度，请先检查账号状态")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := s.hostClient().ResolveOutboundIdentity(ctx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: id})
	if err != nil || r == nil || !r.Found || r.AccountId != id || r.Platform != "openai" || r.AccountType != "oauth" || r.Token == "" {
		return nil, errors.New("宿主未提供有效的 OpenAI OAuth 身份")
	}
	return r, nil
}

func (s *Server) TestConfig(ctx context.Context, r *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	started := time.Now()
	cfg, err := parseConfig(r.ConfigJson)
	var value any
	if err == nil && cfg.Command == nil {
		err = errors.New("请在插件页面选择刷新账号、清空诊断历史或单账号探测")
	}
	if err == nil {
		value, err = s.command(ctx, cfg)
	}
	message := "操作完成"
	if err != nil {
		message = err.Error()
	}
	id := ""
	if cfg.Command != nil {
		id = cfg.Command.ID
	}
	return &pluginv1.TestConfigResponse{Success: err == nil, Message: message, StatusJson: string(encoded(object{"command_id": id, "data": value})), LatencyMs: time.Since(started).Milliseconds()}, nil
}
func (s *Server) command(ctx context.Context, cfg Config) (any, error) {
	c := cfg.Command
	s.commands.Lock()
	defer s.commands.Unlock()
	if c.Instance != s.instance {
		return nil, errors.New("插件进程已变化，请重新打开页面")
	}
	if age := time.Now().Unix() - c.IssuedAt; age < -30 || age > 300 {
		return nil, errors.New("操作已过期，请重新发起")
	}
	fingerprint := string(encoded(c))
	for id, v := range s.seen {
		if time.Now().Unix()-v.at > 300 {
			delete(s.seen, id)
		}
	}
	if old, ok := s.seen[c.ID]; ok {
		if old.fingerprint != fingerprint {
			return nil, errors.New("操作 ID 已被占用")
		}
		return old.value, old.err
	}
	var value any
	var err error
	if c.Action == "accounts" {
		value, err = s.listAccounts(ctx)
	} else if c.Action == "clear_diagnostics" {
		value = s.clearDiagnostics()
	} else {
		s.mu.Lock()
		if s.host == nil {
			err = errors.New("HostService v2 尚未连接")
		} else if s.probe != nil && s.probe.State == "running" {
			err = errors.New("已有探测正在运行")
		} else {
			e, _ := effort(c.Effort, cfg.AllowUltra)
			p := Probe{ID: c.ID, AccountID: c.AccountID, Model: c.Model, Effort: e, Kind: "text", Stage: "identity", State: "running", Message: "正在使用宿主 OAuth 身份探测 BPS"}
			if c.Action == "probe_image" || c.Action == "probe_image_route" {
				p.Kind, p.ImageDetail = "image", c.ImageDetail
				p.ImageFormat = c.ImageFormat
				p.ImageTransport = cfg.ImageTransport
				if c.Action == "probe_image_route" {
					p.Kind = "image_route"
				}
			}
			if c.Action == "probe_tools" {
				p.Kind = "tools"
			}
			s.probe = &p
			value = object{"probe_id": p.ID, "state": p.State}
			go s.runProbe(p, cfg)
		}
		s.mu.Unlock()
	}
	s.seen[c.ID] = commandResult{fingerprint, time.Now().Unix(), value, err}
	return value, err
}

// Probes are explicit and may consume quota. Status may include the generated
// test image and its answer, but never credentials or a raw upstream error body.
func (s *Server) runProbe(p Probe, cfg Config) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(min(cfg.TimeoutSeconds, 120))*time.Second)
	defer cancel()
	err := s.probeOnce(ctx, &p, cfg)
	p.State, p.Message = "succeeded", "BPS 接受此 OAuth 凭据并完成文本响应；不代表模型质量验收"
	if p.Kind == "image" {
		p.Message = "图片探测通过：BPS 返回了图中正确的六位数字；仅代表这一张测试图和当前 detail 的结果"
	}
	if p.Kind == "image_route" {
		p.Message = "图片路由探测通过：模拟桌面请求经 Forward 路由返回了图中六位数字；真实拖图请求请查看独立图片路由诊断"
	}
	if p.Kind == "tools" {
		p.Message = "工具探测通过：custom 多行输入、function 嵌套参数及三轮模拟结果回放完成；不代表桌面端已实际执行本地工具"
	}
	if err != nil {
		p.State, p.Message = "failed", err.Error()
	}
	p.LatencyMS, p.FinishedAt = time.Since(started).Milliseconds(), time.Now().Unix()
	s.mu.Lock()
	s.probe = &p
	s.mu.Unlock()
}
