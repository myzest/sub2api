package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"local.sub2api/codex-inspector/internal/kv"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/pluginconfig"
)

type fixtureHost struct {
	pluginv1.UnimplementedHostServiceServer
	k kv.KV
}

func (f *fixtureHost) KVGet(ctx context.Context, r *pluginv1.KVGetRequest) (*pluginv1.KVGetResponse, error) {
	v, ok, e := f.k.Get(ctx, r.Namespace, r.Key)
	return &pluginv1.KVGetResponse{Value: v, Found: ok}, e
}
func (f *fixtureHost) KVSet(ctx context.Context, r *pluginv1.KVSetRequest) (*pluginv1.KVSetResponse, error) {
	e := f.k.Set(ctx, r.Namespace, r.Key, r.Value, time.Duration(r.TtlSeconds)*time.Second)
	return &pluginv1.KVSetResponse{}, e
}
func (f *fixtureHost) KVDelete(ctx context.Context, r *pluginv1.KVDeleteRequest) (*pluginv1.KVDeleteResponse, error) {
	return &pluginv1.KVDeleteResponse{}, f.k.Delete(ctx, r.Namespace, r.Key)
}
func (f *fixtureHost) KVList(ctx context.Context, r *pluginv1.KVListRequest) (*pluginv1.KVListResponse, error) {
	keys, e := f.k.List(ctx, r.Namespace, r.KeyPrefix, int(r.Limit))
	return &pluginv1.KVListResponse{Keys: keys}, e
}
func (f *fixtureHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest) (*pluginv1.ListAccountsResponse, error) {
	return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 1, Name: "fixture", Schedulable: true, MetadataJson: []byte(`{"password":"do-not-publish"}`)}}}, nil
}
func TestRealPluginProcessAndHostBroker(t *testing.T) {
	path := os.Getenv("CODEX_INSPECTOR_TEST_BINARY")
	if path == "" {
		t.Skip("set CODEX_INSPECTOR_TEST_BINARY for subprocess integration")
	}
	client := hcplugin.NewClient(&hcplugin.ClientConfig{HandshakeConfig: pluginv1.HandshakeConfig, Plugins: pluginv1.ClientPluginMap(), Cmd: exec.Command(path), AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC}, SkipHostEnv: true, Logger: hclog.NewNullLogger(), SyncStdout: io.Discard, SyncStderr: io.Discard, StartTimeout: 10 * time.Second})
	defer client.Kill()
	rpc, e := client.Client()
	if e != nil {
		t.Fatal(e)
	}
	handle, e := rpc.Dispense(pluginv1.TransportPluginName)
	if e != nil {
		t.Fatal(e)
	}
	api := handle.(*pluginv1.TransportClient)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, e := api.GetInfo(ctx, &pluginv1.GetInfoRequest{})
	if e != nil || info.PluginId != PluginID || info.PluginVersion != Version {
		t.Fatal(info, e)
	}
	id := api.Broker.NextId()
	host := &fixtureHost{k: kv.NewMemoryKV()}
	go api.Broker.AcceptAndServe(id, func(opts []grpc.ServerOption) *grpc.Server {
		g := grpc.NewServer(opts...)
		pluginv1.RegisterHostServiceServer(g, host)
		return g
	})
	ready, e := api.InitHostServices(ctx, &pluginv1.InitHostServicesRequest{HostServiceId: id, HostServiceApiVersion: 2})
	if e != nil || !ready.Ready {
		t.Fatal(ready, e)
	}
	c := pluginconfig.Default()
	c.Timezone.Mode = "custom"
	c.Timezone.CustomTZ = "Europe/Paris"
	validation, e := api.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: c.Marshal()})
	if e != nil || !validation.Valid {
		t.Fatal(validation, e)
	}
	applied, e := api.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if e != nil || !applied.Applied {
		t.Fatal(applied, e)
	}
	health, e := api.Health(ctx, &pluginv1.HealthRequest{})
	if e != nil || !health.Healthy || strings.Contains(health.StatusJson, "do-not-publish") {
		t.Fatal(health, e)
	}
	var snapshot map[string]any
	_ = json.Unmarshal([]byte(health.StatusJson), &snapshot)
	if snapshot["host_services"] != "ready" || len(snapshot["accounts"].([]any)) != 1 {
		t.Fatal(snapshot)
	}
	received := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- string(b)
		w.Write([]byte("real subprocess response"))
	}))
	defer up.Close()
	stream, e := api.Forward(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, frame := range frames(up.URL, environmentBody(), 127) {
		if e = stream.Send(frame); e != nil {
			t.Fatal(e)
		}
	}
	_ = stream.CloseSend()
	var output strings.Builder
	ended := false
	for {
		frame, e := stream.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if frame.GetError() != nil {
			t.Fatal(frame.GetError())
		}
		output.Write(frame.GetBodyChunk())
		if frame.GetEnd() != nil {
			ended = true
		}
	}
	if !ended || output.String() != "real subprocess response" || !strings.Contains(<-received, "Europe/Paris") {
		t.Fatal("child process round trip failed")
	}
}
