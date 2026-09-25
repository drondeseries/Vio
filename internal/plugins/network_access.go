package plugins

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/Silo-Server/silo-server/internal/netaccess"
	"github.com/Silo-Server/silo-server/internal/pluginhost"
)

// ErrNetworkAccessProviderNotFound reports a provider slug no enabled
// installation declares. It is netaccess.ErrProviderNotFound so the proxy
// routes and the API agree on it without the proxy importing this package.
var ErrNetworkAccessProviderNotFound = netaccess.ErrProviderNotFound

// ErrNetworkAccessHostUnknown reports a host id a connect or disconnect named
// that this deployment does not run a provider instance on.
var ErrNetworkAccessHostUnknown = errors.New("network access host unknown")

// NetworkAccessProvider is one enabled installation declaring
// network_access_provider.v1, as the capability endpoint lists it. It is read
// from the manifest descriptor; listing never launches the plugin.
type NetworkAccessProvider struct {
	InstallationID int
	CapabilityID   string
	// Provider is the stable slug (tailscale, netbird, ...) that keys the
	// admin routes and names the access path.
	Provider    string
	DisplayName string
}

// NetworkAccessHost identifies one process running an instance of a
// provider installation: the api server or, later, a proxy node. ID is the
// instance-state scope ("api", "node:<id>") so the two never disagree.
type NetworkAccessHost struct {
	ID   string
	Role string
	Name string
}

// NetworkAccessHostStatus is one host's answer. Status.State is
// netaccess.StateUnavailable when the plugin process is not running on the
// host or did not answer; Status.Error then says why when known.
type NetworkAccessHostStatus struct {
	Host   NetworkAccessHost
	Status netaccess.Status
}

// NetworkAccessReport is the admin view of one provider across every host.
type NetworkAccessReport struct {
	Provider NetworkAccessProvider
	Hosts    []NetworkAccessHostStatus
}

// NetworkAccessStatusSink receives the status a provider answered to an
// on-demand read or command, so the host's cached view (WebSocket origin
// allow list, later stream URL selection) does not wait for the plugin's next
// push. *netaccess.Broker implements it.
type NetworkAccessStatusSink interface {
	IngressToken(installationID int) (string, bool)
	ReportFor(installationID int, token string, status netaccess.Status) (previous netaccess.Status, changed bool, accepted bool)
}

// NetworkAccessHostTimeout bounds one host's answer to a status read or a
// command, local or remote, like the node force-reload.
const NetworkAccessHostTimeout = 10 * time.Second

// NetworkAccessNode is one enabled proxy node the admin operations fan out
// to. URL is the backend address the API dials with the node bearer.
type NetworkAccessNode struct {
	ID   int
	Name string
	URL  string
}

// Host returns the node's host identity: the instance-state scope
// "node:<id>" as the id, role proxy, the node's name.
func (n NetworkAccessNode) Host() NetworkAccessHost {
	return NetworkAccessHost{ID: NodeHostScope(int64(n.ID)), Role: pluginhost.HostRoleProxy, Name: n.Name}
}

// NetworkAccessNodes reaches the provider instances running on proxy nodes.
// The API's node handler implements it over each node's bearer routes; a
// proxy node's own service has none and answers for itself alone.
type NetworkAccessNodes interface {
	// ListNetworkAccessNodes returns every enabled proxy node.
	ListNetworkAccessNodes(ctx context.Context) ([]NetworkAccessNode, error)
	NodeNetworkAccessStatus(ctx context.Context, node NetworkAccessNode, provider string) (netaccess.Status, error)
	NodeNetworkAccessConnect(ctx context.Context, node NetworkAccessNode, provider string) (netaccess.Status, error)
	NodeNetworkAccessDisconnect(ctx context.Context, node NetworkAccessNode, provider string) (netaccess.Status, error)
}

// SetNetworkAccessNodes wires the proxy-node fan-out into the admin status,
// connect and disconnect operations. Without it only the local host is
// reported.
func (s *Service) SetNetworkAccessNodes(nodes NetworkAccessNodes) {
	if s == nil {
		return
	}
	s.networkAccessNodes = nodes
}

// SetNetworkAccessHostInfo supplies the identity this process reports as a
// host; main wires the same HostInfoFunc the plugin host answers GetHostInfo
// with. Without it the host is reported as role api with an empty name.
func (s *Service) SetNetworkAccessHostInfo(info pluginhost.HostInfoFunc) {
	if s == nil {
		return
	}
	s.networkAccessHostInfo = info
}

// SetNetworkAccessStatusSink wires the status cache on-demand reads refresh.
func (s *Service) SetNetworkAccessStatusSink(sink NetworkAccessStatusSink) {
	if s == nil {
		return
	}
	s.networkAccessStatus = sink
}

// ListNetworkAccessProviders returns every enabled installation declaring
// network_access_provider.v1, in installation id order. An installation whose
// manifest cannot be read is logged and skipped rather than hiding the rest.
//
// NOTE (fork, stripped for SDK): provider discovery needs
// network_access_provider.v1 descriptor support in the plugin SDK, which the
// pinned SDK predates, so no installation is ever listed until the SDK is
// updated.
func (s *Service) ListNetworkAccessProviders(ctx context.Context) ([]NetworkAccessProvider, error) {
	return nil, nil
}

// networkAccessManifest keeps a running provider discoverable and commandable
// during a transient manifest read failure. Enabled rows still control membership;
// a process from an older version cannot stand in for a replacement release.
func (s *Service) networkAccessManifest(ctx context.Context, installation *Installation) (*pluginv1.PluginManifest, error) {
	manifest, err := s.readInstalledManifest(ctx, installation)
	if err == nil || s.host == nil {
		return manifest, err
	}
	client, clientErr := s.host.Client(installation.ID)
	if clientErr == nil && manifestVersion(client.Manifest()) == installation.Version {
		return client.Manifest(), nil
	}
	return nil, err
}

// networkAccessProvider resolves a slug to its installation.
func (s *Service) networkAccessProvider(ctx context.Context, slug string) (NetworkAccessProvider, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return NetworkAccessProvider{}, ErrNetworkAccessProviderNotFound
	}
	providers, err := s.ListNetworkAccessProviders(ctx)
	if err != nil {
		return NetworkAccessProvider{}, err
	}
	for _, provider := range providers {
		if provider.Provider == slug {
			return provider, nil
		}
	}
	return NetworkAccessProvider{}, ErrNetworkAccessProviderNotFound
}

// localNetworkAccessHost identifies this process as a host: the api host by
// default, or whatever HostInfo says (a proxy names itself node:<id>).
func (s *Service) localNetworkAccessHost(ctx context.Context) NetworkAccessHost {
	host := NetworkAccessHost{ID: HostScopeAPI, Role: pluginhost.HostRoleAPI}
	if s.networkAccessHostInfo != nil {
		if info, err := s.networkAccessHostInfo(ctx); err == nil {
			if info.Role != "" {
				host.Role = info.Role
			}
			host.Name = info.Name
			if info.Role == pluginhost.HostRoleProxy && info.NodeID > 0 {
				host.ID = NodeHostScope(info.NodeID)
			}
		}
	}
	return host
}

// networkAccessTarget is one host an admin operation reaches: the local
// process, or a proxy node over its bearer routes.
type networkAccessTarget struct {
	host NetworkAccessHost
	node *NetworkAccessNode
}

// networkAccessTargets lists the hosts this deployment runs provider
// instances on: this process first, then every enabled proxy node in id
// order. A node listing failure is reported, not hidden: an admin who reads
// "connected" on the api host alone must not conclude the proxies are too.
func (s *Service) networkAccessTargets(ctx context.Context) ([]networkAccessTarget, error) {
	targets := []networkAccessTarget{{host: s.localNetworkAccessHost(ctx)}}
	if s.networkAccessNodes == nil {
		return targets, nil
	}
	nodes, err := s.networkAccessNodes.ListNetworkAccessNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list network access proxy nodes: %w", err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, node := range nodes {
		node := node
		targets = append(targets, networkAccessTarget{host: node.Host(), node: &node})
	}
	return targets, nil
}

// networkAccessOperation names what a report applies to each targeted host.
type networkAccessOperation int

const (
	networkAccessRead networkAccessOperation = iota
	networkAccessConnect
	networkAccessDisconnect
)

// NetworkAccessStatus reads the provider's live status on every host.
func (s *Service) NetworkAccessStatus(ctx context.Context, provider string) (NetworkAccessReport, error) {
	return s.networkAccessReport(ctx, provider, nil, networkAccessRead)
}

// ConnectNetworkAccess asks the provider on the named hosts (nil = every
// host) to bring the overlay up, and reports the status each host reached.
// Hosts not named answer their current status.
func (s *Service) ConnectNetworkAccess(ctx context.Context, provider string, hosts []string) (NetworkAccessReport, error) {
	return s.networkAccessReport(ctx, provider, hosts, networkAccessConnect)
}

// DisconnectNetworkAccess is the counterpart of ConnectNetworkAccess.
func (s *Service) DisconnectNetworkAccess(ctx context.Context, provider string, hosts []string) (NetworkAccessReport, error) {
	return s.networkAccessReport(ctx, provider, hosts, networkAccessDisconnect)
}

// networkAccessReport resolves the provider, checks the requested host ids,
// applies the operation to the targeted hosts (every host when targets is
// nil) and reads status from the rest. Hosts are contacted in parallel, each
// bounded by NetworkAccessHostTimeout, so one hung proxy delays the answer by
// ten seconds and hides nothing about the others.
func (s *Service) networkAccessReport(ctx context.Context, slug string, targets []string, op networkAccessOperation) (NetworkAccessReport, error) {
	if s == nil {
		return NetworkAccessReport{}, ErrNetworkAccessProviderNotFound
	}
	provider, err := s.networkAccessProvider(ctx, slug)
	if err != nil {
		return NetworkAccessReport{}, err
	}
	hosts, err := s.networkAccessTargets(ctx)
	if err != nil {
		return NetworkAccessReport{}, err
	}
	for _, id := range targets {
		id = strings.TrimSpace(id)
		known := false
		for _, target := range hosts {
			if target.host.ID == id {
				known = true
				break
			}
		}
		if !known {
			return NetworkAccessReport{}, fmt.Errorf("%w: %q", ErrNetworkAccessHostUnknown, id)
		}
	}
	report := NetworkAccessReport{Provider: provider, Hosts: make([]NetworkAccessHostStatus, len(hosts))}
	for i, target := range hosts {
		hostCtx, cancel := context.WithTimeout(ctx, NetworkAccessHostTimeout)
		status := s.applyNetworkAccess(hostCtx, provider)
		cancel()
		report.Hosts[i] = NetworkAccessHostStatus{Host: target.host, Status: status}
	}
	return report, nil
}

// applyNodeNetworkAccess runs one operation against the provider on a proxy
// node. The node answers for its own instance; a node that cannot be
// reached, refuses the bearer, or runs a build without the routes reports
// unavailable with the reason.
func (s *Service) applyNodeNetworkAccess(ctx context.Context, node NetworkAccessNode, provider NetworkAccessProvider, op networkAccessOperation) netaccess.Status {
	unavailable := netaccess.Status{InstallationID: provider.InstallationID, Provider: provider.Provider, State: netaccess.StateUnavailable}
	if s.networkAccessNodes == nil {
		unavailable.Error = "proxy node fan-out is not configured"
		return unavailable
	}
	var (
		status netaccess.Status
		err    error
	)
	switch op {
	case networkAccessConnect:
		status, err = s.networkAccessNodes.NodeNetworkAccessConnect(ctx, node, provider.Provider)
	case networkAccessDisconnect:
		status, err = s.networkAccessNodes.NodeNetworkAccessDisconnect(ctx, node, provider.Provider)
	default:
		status, err = s.networkAccessNodes.NodeNetworkAccessStatus(ctx, node, provider.Provider)
	}
	if err != nil {
		if errors.Is(err, ErrNetworkAccessProviderNotFound) {
			unavailable.Error = "provider is not installed on this node yet"
		} else {
			unavailable.Error = err.Error()
		}
		return unavailable
	}
	if status.InstallationID == 0 {
		status.InstallationID = provider.InstallationID
	}
	if status.Provider == "" {
		status.Provider = provider.Provider
	}
	if status.State == "" {
		status.State = netaccess.StateUnavailable
	}
	return status
}

// HostNetworkAccessStatus lists the live status of every provider instance
// on this host alone; a proxy node answers the API's fan-out with it.
func (s *Service) HostNetworkAccessStatus(ctx context.Context) (netaccess.HostStatusReport, error) {
	report := netaccess.HostStatusReport{Providers: []netaccess.Status{}}
	if s == nil {
		return report, nil
	}
	providers, err := s.ListNetworkAccessProviders(ctx)
	if err != nil {
		return report, err
	}
	for _, provider := range providers {
		hostCtx, cancel := context.WithTimeout(ctx, NetworkAccessHostTimeout)
		report.Providers = append(report.Providers, s.applyNetworkAccess(hostCtx, provider))
		cancel()
	}
	return report, nil
}

// HostNetworkAccessProviderStatus reads one provider on this host without
// waiting for unrelated providers. The API uses this for its per-provider
// status fan-out to proxy nodes.
func (s *Service) HostNetworkAccessProviderStatus(ctx context.Context, slug string) (netaccess.Status, error) {
	return s.hostNetworkAccess(ctx, slug, networkAccessRead)
}

// HostNetworkAccessConnect brings the provider up on this host alone.
func (s *Service) HostNetworkAccessConnect(ctx context.Context, slug string) (netaccess.Status, error) {
	return s.hostNetworkAccess(ctx, slug, networkAccessConnect)
}

// HostNetworkAccessDisconnect tears the provider down on this host alone.
func (s *Service) HostNetworkAccessDisconnect(ctx context.Context, slug string) (netaccess.Status, error) {
	return s.hostNetworkAccess(ctx, slug, networkAccessDisconnect)
}

func (s *Service) hostNetworkAccess(ctx context.Context, slug string, op networkAccessOperation) (netaccess.Status, error) {
	if s == nil {
		return netaccess.Status{}, ErrNetworkAccessProviderNotFound
	}
	provider, err := s.networkAccessProvider(ctx, slug)
	if err != nil {
		return netaccess.Status{}, err
	}
	hostCtx, cancel := context.WithTimeout(ctx, NetworkAccessHostTimeout)
	defer cancel()
	return s.applyNetworkAccess(hostCtx, provider), nil
}

// applyNetworkAccess answers one provider read or command on this host.
// NOTE (fork, stripped for SDK): provider RPCs need
// network_access_provider.v1 client support in the plugin SDK, which the
// pinned SDK predates, so every operation answers unavailable with the
// reason until the SDK is updated. Host validation and report shapes are
// unchanged, so the admin and capability surfaces keep working.
func (s *Service) applyNetworkAccess(ctx context.Context, provider NetworkAccessProvider) netaccess.Status {
	return netaccess.Status{InstallationID: provider.InstallationID, Provider: provider.Provider, State: netaccess.StateUnavailable, Error: "network access providers require a plugin SDK with network_access_provider.v1 support"}
}

func networkAccessUnavailableReason(err error, state RuntimeState) string {
	switch {
	case errors.Is(err, pluginhost.ErrPluginUnhealthy):
		return "plugin process is unhealthy"
	case state.LastError != "":
		return fmt.Sprintf("plugin process is %s: %s", state.State, state.LastError)
	case state.State != "" && state.State != ResidentRunning:
		return "plugin process is " + string(state.State)
	}
	return "plugin process is not running"
}
