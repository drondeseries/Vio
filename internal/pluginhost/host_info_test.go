package pluginhost_test

import (
	"context"
	"testing"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/Silo-Server/silo-server/internal/pluginhost"
)

func apiHostInfo(listeners ...pluginhost.HostListener) pluginhost.HostInfoFunc {
	return func(context.Context) (pluginhost.HostInfo, error) {
		return pluginhost.HostInfo{
			PublicBaseURL:       "https://silo.example/",
			PluginContentPrefix: "/api/v2/plugin-content",
			Role:                pluginhost.HostRoleAPI,
			Name:                "Living Room",
			Listeners:           listeners,
		}, nil
	}
}

func TestRuntimeHostServer_GetHostInfo_ReportsBaseURLs(t *testing.T) {
	// NOTE (fork, stripped for SDK): role/name/node/listeners/ingress-token
	// response fields need a newer plugin SDK, so only base URLs are covered
	// until the SDK is updated.
	srv := pluginhost.NewRuntimeHostServerWithOptions(pluginhost.RuntimeHostOptions{
		HostInfo: apiHostInfo(
			pluginhost.HostListener{Name: pluginhost.ListenerAPI, Address: "127.0.0.1:8080", DefaultPort: pluginhost.DefaultPortAPI},
		),
		Logger:         hclog.NewNullLogger(),
		PluginID:       "silo.tailscale",
		InstallationID: 9,
	})

	resp, err := srv.GetHostInfo(context.Background(), &pluginv1.GetHostInfoRequest{})
	if err != nil {
		t.Fatalf("GetHostInfo: %v", err)
	}
	if resp.GetPublicBaseUrl() != "https://silo.example" {
		t.Fatalf("public_base_url = %q", resp.GetPublicBaseUrl())
	}
	if resp.GetInternalBaseUrl() != "http://127.0.0.1:8080" {
		t.Fatalf("internal_base_url = %q", resp.GetInternalBaseUrl())
	}
	if resp.GetPluginProxyBaseUrl() != "https://silo.example/api/v2/plugin-content/plugins/9" {
		t.Fatalf("plugin_proxy_base_url = %q", resp.GetPluginProxyBaseUrl())
	}
}

func TestRuntimeHostServer_GetHostInfo_Unconfigured(t *testing.T) {
	_, err := pluginhost.NewRuntimeHostServerWithOptions(pluginhost.RuntimeHostOptions{}).GetHostInfo(context.Background(), &pluginv1.GetHostInfoRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("no host info configured: %v", err)
	}
}

func TestLoopbackDialAddress(t *testing.T) {
	for in, want := range map[string]string{":8080": "127.0.0.1:8080", "0.0.0.0:8080": "127.0.0.1:8080", "[::]:8080": "127.0.0.1:8080", "10.0.0.5:9000": "10.0.0.5:9000", "": ""} {
		if got := pluginhost.LoopbackDialAddress(in); got != want {
			t.Fatalf("LoopbackDialAddress(%q) = %q, want %q", in, got, want)
		}
	}
}
