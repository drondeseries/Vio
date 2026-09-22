package plugins

// NOTE (fork, stripped for SDK): this file tested the provider-RPC path
// (list/connect/disconnect/status through a live fixture plugin), which needs
// network_access_provider.v1 support in the plugin SDK. The pinned SDK
// predates it, so these tests are parked until the SDK is updated.

import (
	"context"
	"net"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-server/internal/pluginhost"
)

type callbackListener struct {
	net.Listener
	accepted chan struct{}
	once     sync.Once
}

func (l *callbackListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.once.Do(func() { close(l.accepted) })
	}
	return conn, err
}

// The plugin must dial the callback stream during BindHostBroker, before any
// capability RPC needs Host(). Observing that connection directly catches the
// lazy-dial regression without waiting out go-plugin's pending-stream timeout.
func skippedTestNetworkAccessHostCallbackConnectsAtBind(t *testing.T) {
	process := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  pluginhost.HandshakeConfig(),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Cmd:              exec.Command(buildResidentFixture(t)),
		Plugins:          sdkruntime.DefaultPluginSet(sdkruntime.CapabilityServers{}),
		Logger:           hclog.NewNullLogger(),
	})
	t.Cleanup(process.Kill)
	protocol, err := process.Client()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.Dispense(sdkruntime.PluginSetName)
	if err != nil {
		t.Fatal(err)
	}
	client := raw.(*sdkruntime.Client)
	broker := client.Broker()
	id := broker.NextId()
	listener, err := broker.Accept(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	observed := &callbackListener{Listener: listener, accepted: make(chan struct{})}
	server := grpc.NewServer()
	pluginv1.RegisterRuntimeHostServer(server, pluginhost.NewRuntimeHostServerWithOptions(pluginhost.RuntimeHostOptions{
		HostInfo: func(context.Context) (pluginhost.HostInfo, error) {
			return pluginhost.HostInfo{Role: pluginhost.HostRoleAPI, Name: "api"}, nil
		},
	}))
	t.Cleanup(server.Stop)
	go func() { _ = server.Serve(observed) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := client.Runtime().BindHostBroker(ctx, &pluginv1.BindHostBrokerRequest{BrokerId: id}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed.accepted:
	case <-ctx.Done():
		t.Fatal("plugin did not open its callback connection during binding")
	}
}
