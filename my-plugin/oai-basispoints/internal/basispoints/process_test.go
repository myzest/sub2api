package basispoints

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

type fixtureHostServer struct {
	pluginv1.UnimplementedHostServiceServer
	host *fakeHost
}

func (f *fixtureHostServer) KVGet(c context.Context, r *pluginv1.KVGetRequest) (*pluginv1.KVGetResponse, error) {
	return f.host.KVGet(c, r)
}
func (f *fixtureHostServer) KVSet(c context.Context, r *pluginv1.KVSetRequest) (*pluginv1.KVSetResponse, error) {
	return f.host.KVSet(c, r)
}
func (f *fixtureHostServer) ListAccounts(c context.Context, r *pluginv1.ListAccountsRequest) (*pluginv1.ListAccountsResponse, error) {
	return f.host.ListAccounts(c, r)
}
func (f *fixtureHostServer) ResolveOutboundIdentity(c context.Context, r *pluginv1.ResolveOutboundIdentityRequest) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	return f.host.ResolveOutboundIdentity(c, r)
}

func TestRealPluginProcessAndHostBroker(t *testing.T) {
	path := os.Getenv("BASISPOINTS_TEST_BINARY")
	if path == "" {
		t.Skip("set BASISPOINTS_TEST_BINARY to the packaged runtime")
	}
	client := hcplugin.NewClient(&hcplugin.ClientConfig{HandshakeConfig: pluginv1.HandshakeConfig, Plugins: pluginv1.ClientPluginMap(), Cmd: exec.Command(path), AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC}, SkipHostEnv: true, Logger: hclog.NewNullLogger(), SyncStdout: io.Discard, SyncStderr: io.Discard, StartTimeout: 10 * time.Second})
	defer client.Kill()
	rpc, err := client.Client()
	if err != nil {
		t.Fatal(err)
	}
	handle, err := rpc.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		t.Fatal(err)
	}
	api := handle.(*pluginv1.TransportClient)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := api.GetInfo(ctx, &pluginv1.GetInfoRequest{})
	if err != nil || info.PluginId != PluginID || info.PluginVersion != Version {
		t.Fatal(info, err)
	}
	host := &fixtureHostServer{host: newFakeHost()}
	id := api.Broker.NextId()
	go api.Broker.AcceptAndServe(id, func(opts []grpc.ServerOption) *grpc.Server {
		server := grpc.NewServer(opts...)
		pluginv1.RegisterHostServiceServer(server, host)
		return server
	})
	init, err := api.InitHostServices(ctx, &pluginv1.InitHostServicesRequest{HostServiceId: id, HostServiceApiVersion: 2})
	if err != nil || !init.Ready {
		t.Fatal(init, err)
	}
	health, err := api.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var state Snapshot
	if err := json.Unmarshal([]byte(health.StatusJson), &state); err != nil || !state.HostReady || state.Config.RouteEnabled {
		t.Fatal(health, err)
	}
	cfg := state.Config
	cfg.Command = &Command{ID: newID(), Instance: state.Instance, IssuedAt: time.Now().Unix(), Action: "accounts"}
	validation, err := api.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: encoded(cfg)})
	if err != nil || !validation.Valid {
		t.Fatal(validation, err)
	}
	apply, err := api.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if err != nil || !apply.Applied {
		t.Fatal(apply, err)
	}
	test, err := api.TestConfig(ctx, &pluginv1.TestConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if err != nil || !test.Success {
		t.Fatal(test, err)
	}
	health, _ = api.Health(ctx, &pluginv1.HealthRequest{})
	json.Unmarshal([]byte(health.StatusJson), &state)
	if len(state.Accounts) != 2 {
		t.Fatal("Host broker account directory not available", state)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "real plugin process forwarding") }))
	defer up.Close()
	frames := forward(t, api, &pluginv1.ForwardRequestStart{Method: "GET", Url: up.URL, Platform: "openai", AccountType: "oauth", AccountId: 7}, nil)
	if string(bodyOf(frames)) != "real plugin process forwarding" || frames[len(frames)-1].GetEnd() == nil {
		t.Fatal("child transport failed", frames)
	}
}
