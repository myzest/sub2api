package inspector

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
	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
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
func (f *fixtureHostServer) KVDelete(c context.Context, r *pluginv1.KVDeleteRequest) (*pluginv1.KVDeleteResponse, error) {
	return f.host.KVDelete(c, r)
}
func (f *fixtureHostServer) KVList(c context.Context, r *pluginv1.KVListRequest) (*pluginv1.KVListResponse, error) {
	return f.host.KVList(c, r)
}
func (f *fixtureHostServer) ListAccounts(c context.Context, r *pluginv1.ListAccountsRequest) (*pluginv1.ListAccountsResponse, error) {
	return f.host.ListAccounts(c, r)
}
func (f *fixtureHostServer) ResolveOutboundIdentity(c context.Context, r *pluginv1.ResolveOutboundIdentityRequest) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	return f.host.ResolveOutboundIdentity(c, r)
}

func TestRealPluginProcessAndHostBroker(t *testing.T) {
	path := os.Getenv("INSPECTOR_TEST_BINARY")
	if path == "" {
		t.Skip("set INSPECTOR_TEST_BINARY to test the built executable")
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
	host := &fixtureHostServer{host: &fakeHost{values: map[string][]byte{}}}
	id := api.Broker.NextId()
	go api.Broker.AcceptAndServe(id, func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(opts...)
		pluginv1.RegisterHostServiceServer(s, host)
		return s
	})
	init, err := api.InitHostServices(ctx, &pluginv1.InitHostServicesRequest{HostServiceId: id, HostServiceApiVersion: 2})
	if err != nil || !init.Ready {
		t.Fatal(init, err)
	}
	waitFor(t, func() bool {
		r, err := api.Health(ctx, &pluginv1.HealthRequest{})
		if err != nil {
			return false
		}
		var x Snapshot
		_ = json.Unmarshal([]byte(r.StatusJson), &x)
		return x.Ready && len(x.Accounts) == 2
	})
	validation, err := api.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: []byte(`{}`)})
	if err != nil || !validation.Valid {
		t.Fatal(validation, err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "real child process forwarding") }))
	defer up.Close()
	frames := forward(t, api, &pluginv1.ForwardRequestStart{Method: "GET", Url: up.URL, Platform: "openai", AccountType: "oauth"}, "")
	if len(frames) < 3 || frames[len(frames)-1].GetEnd() == nil {
		t.Fatal("child process forwarding failed", frames)
	}
}
