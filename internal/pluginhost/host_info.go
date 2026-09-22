package pluginhost

import (
	"context"
	"net"
	"strings"

	"github.com/Silo-Server/silo-server/internal/netaccess"
)

// Host roles reported in GetHostInfo.host_role.
const (
	HostRoleAPI   = "api"
	HostRoleProxy = "proxy"
)

// Listener names reported in GetHostInfo.listeners.
const (
	ListenerAPI      = "api"
	ListenerJellyfin = "jellyfin"
	ListenerABS      = "abs"
)

// Default overlay ports a network access provider exposes each listener on.
const (
	DefaultPortAPI      = 443
	DefaultPortJellyfin = 8096
	DefaultPortABS      = 13378
)

// HostListener is one local listener a network access provider should expose
// on the overlay: its loopback dial address and the port to expose it on.
type HostListener struct {
	Name        string
	Address     string
	DefaultPort int
}

// HostInfo is what GetHostInfo reports about the process hosting the plugin.
// Public URLs and listeners can change with a config reload, so the host
// supplies a HostInfoFunc that builds it per call.
type HostInfo struct {
	// PublicBaseURL is server.public_url without a trailing slash; empty when
	// unset.
	PublicBaseURL string
	// PluginContentPrefix is the API path under which plugin HTTP routes are
	// proxied; GetHostInfo appends /plugins/<installation_id> to it.
	PluginContentPrefix string
	Role                string
	Name                string
	NodeID              int64
	Listeners           []HostListener
}

// HostInfoFunc answers GetHostInfo for the current process.
type HostInfoFunc func(ctx context.Context) (HostInfo, error)

// NetworkAccessBroker is the netaccess side of a resident network access
// provider's lifetime: Host.Start issues the ingress token before the plugin
// can ask for it, stopping the process revokes it, and status pushes land in
// the host's status cache. *netaccess.Broker implements it.
type NetworkAccessBroker interface {
	Issue(installationID int, provider string) (string, error)
	// Revoke drops the token only while it is still the installation's
	// current one, so a late stop cannot revoke a replacement's token.
	Revoke(installationID int, token string)
	IngressToken(installationID int) (string, bool)
	// ReportFor records a status push from the process holding token. A push
	// from a process whose token was already revoked (it crashed, was
	// stopped, or was replaced while the RPC was in flight) is dropped, so a
	// dead instance can never write a stale origin back over a fresh one.
	ReportFor(installationID int, token string, status netaccess.Status) (previous netaccess.Status, changed bool, accepted bool)
}

// NOTE (fork, stripped for SDK): provider detection from manifests needs
// network_access_provider.v1 descriptor support in the plugin SDK, which the
// pinned SDK predates. No installation is treated as a provider until the
// SDK is updated.

// LoopbackDialAddress turns a listen address into the host:port a plugin in
// the same process namespace dials: a wildcard or empty host becomes
// 127.0.0.1, a concrete host is kept.
func LoopbackDialAddress(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
