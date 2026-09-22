package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/hashicorp/go-hclog"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/events"
)

// EventPublisher is the subset of *events.Hub required by RuntimeHostServer.
// Defined as an interface so tests can supply a fake.
type EventPublisher interface {
	Publish(ctx context.Context, env events.Envelope) error
}

// LibraryRecord is the wire shape for libraries returned to plugins. Mirrors
// the proto Library message.
type LibraryRecord struct {
	ID        string
	Name      string
	MediaType string // movie | tv | mixed
}

// LibraryLister returns libraries visible to a user (or all when userID is "").
type LibraryLister interface {
	ListLibraries(ctx context.Context, userID string) ([]LibraryRecord, error)
}

// LibraryPresenceRecord is a single host catalog match returned by a
// CatalogPresenceLookup. The plugin sees it as proto MediaPresence.
type LibraryPresenceRecord struct {
	ExternalID string
	MediaID    string
	LibraryID  string
	Title      string
}

// CatalogPresenceLookup answers "which of these external IDs do we already
// have?" for a batch of IDs. v1 supports provider "tmdb" only; other values
// should return the empty result without error.
type CatalogPresenceLookup interface {
	LookupByExternalIDs(ctx context.Context, provider, mediaType string, ids []string) ([]LibraryPresenceRecord, error)
}

// InstalledPluginRecord is the host-side shape returned to plugins for peer
// discovery.
type InstalledPluginRecord struct {
	InstallationID int
	PluginID       string
	Version        string
	Enabled        bool
	Capabilities   []*pluginv1.CapabilityDescriptor
}

// InstalledPluginLister returns installed plugins and their capability
// descriptors for RuntimeHost.ListInstalledPlugins.
type InstalledPluginLister interface {
	ListInstalledPlugins(ctx context.Context) ([]InstalledPluginRecord, error)
}

// InstalledPluginListerFunc adapts a plain function to InstalledPluginLister.
type InstalledPluginListerFunc func(ctx context.Context) ([]InstalledPluginRecord, error)

func (f InstalledPluginListerFunc) ListInstalledPlugins(ctx context.Context) ([]InstalledPluginRecord, error) {
	return f(ctx)
}

// GlobalConfigSetter persists a global config entry for a plugin installation.
type GlobalConfigSetter interface {
	SetGlobalConfigEntry(ctx context.Context, installationID int, key string, value map[string]any) error
}

// GlobalConfigSetterFunc adapts a plain function to GlobalConfigSetter.
type GlobalConfigSetterFunc func(ctx context.Context, installationID int, key string, value map[string]any) error

func (f GlobalConfigSetterFunc) SetGlobalConfigEntry(ctx context.Context, installationID int, key string, value map[string]any) error {
	return f(ctx, installationID, key, value)
}

// VirtualCatalogRegistrar performs host-owned transactional registration of
// virtual media submitted by an installed plugin.
type VirtualCatalogRegistrar interface {
	UpsertVirtualMedia(context.Context, int, catalog.VirtualMedia) (*catalog.VirtualMediaResult, error)
}

type VirtualCatalogReconciler interface {
	ReconcileVirtualMedia(context.Context, int, string, []string, []int) (catalog.VirtualReconcileResult, error)
}

type VirtualCatalogRegistrarFunc func(context.Context, int, catalog.VirtualMedia) (*catalog.VirtualMediaResult, error)

func (f VirtualCatalogRegistrarFunc) UpsertVirtualMedia(ctx context.Context, installationID int, req catalog.VirtualMedia) (*catalog.VirtualMediaResult, error) {
	return f(ctx, installationID, req)
}

// DefaultPublishEventRatePerSec is the default maximum number of events a
// plugin may publish per second. It also serves as the burst size so a plugin
// can fire a short burst at a higher rate before being throttled.
const DefaultPublishEventRatePerSec = 100

// RuntimeHostServer is the gRPC server that plugins call back into via the
// go-plugin broker. Each plugin instance gets its own RuntimeHostServer so
// the pluginID is fixed at construction.
type RuntimeHostServer struct {
	pluginv1.UnimplementedRuntimeHostServer
	publisher EventPublisher
	libs      LibraryLister
	catalog   CatalogPresenceLookup
	pluginID  string
	limiter   *rate.Limiter

	installedPlugins InstalledPluginLister
	configSetter     GlobalConfigSetter
	virtualCatalog   VirtualCatalogRegistrar
	installationID   int

	hostInfo      HostInfoFunc
	instanceState InstanceStateStore
	networkAccess NetworkAccessBroker
	// ingressToken is the token issued to this process instance; pushes are
	// accepted only while it is current.
	ingressToken string
	// provider is the network_access_provider.v1 slug from the plugin's
	// manifest; empty for plugins that are not providers.
	provider string
	logger   hclog.Logger
}

// RuntimeHostOptions configures a RuntimeHostServer for one plugin instance.
type RuntimeHostOptions struct {
	Publisher          EventPublisher
	Libraries          LibraryLister
	Catalog            CatalogPresenceLookup
	InstalledPlugins   InstalledPluginLister
	GlobalConfigSetter GlobalConfigSetter
	HostInfo           HostInfoFunc
	InstanceState      InstanceStateStore
	NetworkAccess      NetworkAccessBroker
	// VirtualCatalog owns virtual media registration requested by plugins
	// (fork: virtual-library traffic resolves through core; plugins register
	// into the catalog via this host-owned registrar, never via dispatch).
	VirtualCatalog VirtualCatalogRegistrar
	Logger         hclog.Logger
	// EventRatePerSec caps PublishEvent; <= 0 takes DefaultPublishEventRatePerSec.
	EventRatePerSec int

	PluginID       string
	InstallationID int
	// NetworkAccessProvider is the provider slug the manifest declares, or
	// empty. Only providers may push network access status.
	NetworkAccessProvider string
	// IngressToken is the token issued to this process instance. Status
	// pushes are accepted only while it is still the installation's current
	// token.
	IngressToken string
}

// NewRuntimeHostServerWithOptions builds the server the host binds for one
// plugin instance.
func NewRuntimeHostServerWithOptions(opts RuntimeHostOptions) *RuntimeHostServer {
	s := NewRuntimeHostServerWithRate(opts.Publisher, opts.Libraries, opts.PluginID, opts.EventRatePerSec)
	s.catalog = opts.Catalog
	s.installedPlugins = opts.InstalledPlugins
	s.configSetter = opts.GlobalConfigSetter
	s.installationID = opts.InstallationID
	s.hostInfo = opts.HostInfo
	s.instanceState = opts.InstanceState
	s.networkAccess = opts.NetworkAccess
	s.virtualCatalog = opts.VirtualCatalog
	s.provider = opts.NetworkAccessProvider
	s.ingressToken = opts.IngressToken
	s.logger = opts.Logger
	if s.logger == nil {
		s.logger = hclog.NewNullLogger()
	}
	return s
}

// NewRuntimeHostServer constructs a RuntimeHostServer bound to the given
// pluginID. The pluginID is server-stamped on all published events so plugins
// cannot forge core event names.
func NewRuntimeHostServer(publisher EventPublisher, libs LibraryLister, pluginID string) *RuntimeHostServer {
	return &RuntimeHostServer{
		publisher: publisher,
		libs:      libs,
		pluginID:  pluginID,
		limiter:   rate.NewLimiter(rate.Limit(DefaultPublishEventRatePerSec), DefaultPublishEventRatePerSec),
	}
}

// NewRuntimeHostServerWithRate is like NewRuntimeHostServer but installs a
// caller-specified rate limit (events/sec, also used as burst). Pass perSec<=0
// to fall back to DefaultPublishEventRatePerSec.
func NewRuntimeHostServerWithRate(publisher EventPublisher, libs LibraryLister, pluginID string, perSec int) *RuntimeHostServer {
	if perSec <= 0 {
		perSec = DefaultPublishEventRatePerSec
	}
	return &RuntimeHostServer{
		publisher: publisher,
		libs:      libs,
		pluginID:  pluginID,
		limiter:   rate.NewLimiter(rate.Limit(perSec), perSec),
	}
}

// NewRuntimeHostServerWithCatalog is like NewRuntimeHostServer but also
// accepts a CatalogPresenceLookup. Use this when the host can answer
// CheckMediaPresence; passing nil makes presence queries return empty.
func NewRuntimeHostServerWithCatalog(publisher EventPublisher, libs LibraryLister, catalog CatalogPresenceLookup, pluginID string) *RuntimeHostServer {
	s := NewRuntimeHostServer(publisher, libs, pluginID)
	s.catalog = catalog
	return s
}

// NewRuntimeHostServerWithServices is like NewRuntimeHostServerWithCatalog but
// also enables peer discovery and plugin-owned config persistence.
func NewRuntimeHostServerWithServices(
	publisher EventPublisher,
	libs LibraryLister,
	catalog CatalogPresenceLookup,
	installedPlugins InstalledPluginLister,
	configSetter GlobalConfigSetter,
	virtualCatalog VirtualCatalogRegistrar,
	pluginID string,
	installationID int,
) *RuntimeHostServer {
	s := NewRuntimeHostServerWithCatalog(publisher, libs, catalog, pluginID)
	s.installedPlugins = installedPlugins
	s.configSetter = configSetter
	s.virtualCatalog = virtualCatalog
	s.installationID = installationID
	return s
}

func (s *RuntimeHostServer) UpsertVirtualMedia(ctx context.Context, req *pluginv1.UpsertVirtualMediaRequest) (*pluginv1.UpsertVirtualMediaResponse, error) {
	if s.virtualCatalog == nil {
		return nil, fmt.Errorf("server: virtual catalog is not configured")
	}
	if s.installationID <= 0 {
		return nil, fmt.Errorf("server: plugin installation is not bound")
	}

	variants := make([]catalog.VirtualMediaVariant, 0, len(req.GetVariants()))
	for _, v := range req.GetVariants() {
		variants = append(variants, catalog.VirtualMediaVariant{
			VirtualURI:     v.GetVirtualUri(),
			Label:          v.GetLabel(),
			Resolution:     v.GetResolution(),
			CodecVideo:     v.GetCodecVideo(),
			CodecAudio:     v.GetCodecAudio(),
			HDR:            v.GetHdr(),
			Bitrate:        int(v.GetBitrate()),
			RuntimeMinutes: int(v.GetRuntimeMinutes()),
			FileSize:       v.GetFileSize(), Container: v.GetContainer(), SourceType: v.GetSourceType(),
			AudioLanguages: v.GetAudioLanguages(), SubtitleLanguages: v.GetSubtitleLanguages(), Availability: v.GetAvailability(),
		})
	}

	episodes := make([]catalog.VirtualEpisode, 0, len(req.GetEpisodes()))
	for _, episode := range req.GetEpisodes() {
		var airDate time.Time
		if episode.GetAirDateUnix() > 0 {
			airDate = time.Unix(episode.GetAirDateUnix(), 0).UTC()
		}
		epVariants := make([]catalog.VirtualMediaVariant, 0, len(episode.GetVariants()))
		for _, v := range episode.GetVariants() {
			epVariants = append(epVariants, catalog.VirtualMediaVariant{
				VirtualURI:     v.GetVirtualUri(),
				Label:          v.GetLabel(),
				Resolution:     v.GetResolution(),
				CodecVideo:     v.GetCodecVideo(),
				CodecAudio:     v.GetCodecAudio(),
				HDR:            v.GetHdr(),
				Bitrate:        int(v.GetBitrate()),
				RuntimeMinutes: int(v.GetRuntimeMinutes()),
				FileSize:       v.GetFileSize(), Container: v.GetContainer(), SourceType: v.GetSourceType(),
				AudioLanguages: v.GetAudioLanguages(), SubtitleLanguages: v.GetSubtitleLanguages(), Availability: v.GetAvailability(),
			})
		}
		// Carry first-variant metadata to the episode so non-variant
		// upsertVirtualFileWithMeta stores audio/subtitle languages.
		var epResolution, epCodecVideo, epCodecAudio, epHDR, epContainer, epSourceType string
		var epBitrate int
		var epFileSize int64
		var epAudioLangs, epSubLangs []string
		if len(epVariants) > 0 {
			epResolution = epVariants[0].Resolution
			epCodecVideo = epVariants[0].CodecVideo
			epCodecAudio = epVariants[0].CodecAudio
			epHDR = epVariants[0].HDR
			epBitrate = epVariants[0].Bitrate
			epFileSize = epVariants[0].FileSize
			epContainer = epVariants[0].Container
			epSourceType = epVariants[0].SourceType
			epAudioLangs = epVariants[0].AudioLanguages
			epSubLangs = epVariants[0].SubtitleLanguages
		}
		episodes = append(episodes, catalog.VirtualEpisode{
			SeasonNumber: int(episode.GetSeasonNumber()), EpisodeNumber: int(episode.GetEpisodeNumber()),
			Title: episode.GetTitle(), Overview: episode.GetOverview(), AirDate: airDate,
			RuntimeMinutes: int(episode.GetRuntimeMinutes()), StillPath: episode.GetStillPath(), VirtualURI: episode.GetVirtualUri(),
			Variants:   epVariants,
			Resolution: epResolution, CodecVideo: epCodecVideo, CodecAudio: epCodecAudio,
			HDR: epHDR, Bitrate: epBitrate, FileSize: epFileSize,
			Container: epContainer, SourceType: epSourceType,
			AudioLanguages: epAudioLangs, SubtitleLanguages: epSubLangs,
		})
	}

	// Carry top-level stream metadata from the first variant when the request
	// carries no dedicated top-level fields — this lets the catalog store
	// resolution, codecs, audio/subtitle languages so the watch detail and
	// player UI show track options without waiting for a playback probe.
	var topResolution, topCodecVideo, topCodecAudio, topHDR, topContainer, topSourceType string
	var topBitrate int
	var topFileSize int64
	var topAudioLangs, topSubLangs []string
	if len(variants) > 0 {
		topResolution = variants[0].Resolution
		topCodecVideo = variants[0].CodecVideo
		topCodecAudio = variants[0].CodecAudio
		topHDR = variants[0].HDR
		topBitrate = variants[0].Bitrate
		topFileSize = variants[0].FileSize
		topContainer = variants[0].Container
		topSourceType = variants[0].SourceType
		topAudioLangs = variants[0].AudioLanguages
		topSubLangs = variants[0].SubtitleLanguages
	}
	vm := catalog.VirtualMedia{
		LibraryID: req.GetLibraryId(), MediaType: req.GetMediaType(), Title: req.GetTitle(), Year: int(req.GetYear()),
		IMDbID: req.GetImdbId(), TMDBID: req.GetTmdbId(), TVDBID: req.GetTvdbId(), Overview: req.GetOverview(),
		Genres: req.GetGenres(), PosterPath: req.GetPosterPath(), BackdropPath: req.GetBackdropPath(),
		VirtualURI: req.GetVirtualUri(), RuntimeMinutes: int(req.GetRuntimeMinutes()), Episodes: episodes,
		Variants: variants, Source: req.GetSourceKey(),
		Resolution: topResolution, CodecVideo: topCodecVideo, CodecAudio: topCodecAudio,
		HDR: topHDR, Bitrate: topBitrate, FileSize: topFileSize,
		Container: topContainer, SourceType: topSourceType,
		AudioLanguages: topAudioLangs, SubtitleLanguages: topSubLangs,
	}

	result, err := s.virtualCatalog.UpsertVirtualMedia(ctx, s.installationID, vm)
	if err != nil {
		return nil, err
	}
	return &pluginv1.UpsertVirtualMediaResponse{MediaId: result.MediaID, LibraryId: result.LibraryID, EpisodesUpserted: int32(result.EpisodesUpserted)}, nil
}

func (s *RuntimeHostServer) ReconcileVirtualMedia(ctx context.Context, req *pluginv1.ReconcileVirtualMediaRequest) (*pluginv1.ReconcileVirtualMediaResponse, error) {
	reconciler, ok := s.virtualCatalog.(VirtualCatalogReconciler)
	if !ok {
		return nil, errors.New("server: virtual catalog is not configured")
	}
	sourceKey := strings.TrimSpace(req.GetSourceKey())
	if sourceKey == "" {
		return nil, errors.New("source_key is required")
	}
	libraryIDs := make([]int, 0, len(req.GetLibraryIds()))
	for _, value := range req.GetLibraryIds() {
		id, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid library_id %q", value)
		}
		libraryIDs = append(libraryIDs, id)
	}
	result, err := reconciler.ReconcileVirtualMedia(ctx, s.installationID, sourceKey, req.GetKeepMediaIds(), libraryIDs)
	if err != nil {
		return nil, err
	}
	return &pluginv1.ReconcileVirtualMediaResponse{ItemsRemoved: int32(result.ItemsRemoved), FilesRemoved: int32(result.FilesRemoved)}, nil
}

// PublishEvent auto-prefixes the plugin's event name with "plugin.<plugin_id>."
// and forwards to the EventPublisher on events.ChannelPlugins. The plugin ID
// is server-stamped (not taken from the request) so plugins cannot forge
// core event names by crafting a malicious event_name.
func (s *RuntimeHostServer) PublishEvent(ctx context.Context, req *pluginv1.PublishEventRequest) (*pluginv1.PublishEventResponse, error) {
	if s.limiter != nil && !s.limiter.Allow() {
		return nil, fmt.Errorf("rate limit exceeded for plugin %q", s.pluginID)
	}
	name := strings.TrimSpace(req.GetEventName())
	if name == "" {
		return nil, fmt.Errorf("event_name is required")
	}
	if s.pluginID == "" {
		return nil, fmt.Errorf("server: plugin id not bound")
	}
	if s.publisher == nil {
		return nil, fmt.Errorf("server: event publisher not configured")
	}

	var payload json.RawMessage
	if p := req.GetPayload(); p != nil {
		raw, err := p.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("encode payload: %w", err)
		}
		payload = raw
	}

	prefixed := "plugin." + s.pluginID + "." + name
	env := events.Envelope{
		Channel: events.ChannelPlugins,
		Event:   prefixed,
		Data:    payload,
	}
	if err := s.publisher.Publish(ctx, env); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	return &pluginv1.PublishEventResponse{}, nil
}

// PublishEventTo is like PublishEvent but restricts delivery to subscribers
// belonging to the target plugin_id.
func (s *RuntimeHostServer) PublishEventTo(ctx context.Context, req *pluginv1.PublishEventToRequest) (*pluginv1.PublishEventToResponse, error) {
	if s.limiter != nil && !s.limiter.Allow() {
		return nil, fmt.Errorf("rate limit exceeded for plugin %q", s.pluginID)
	}
	name := strings.TrimSpace(req.GetEventName())
	if name == "" {
		return nil, fmt.Errorf("event_name is required")
	}
	targetPluginID := strings.TrimSpace(req.GetTargetPluginId())
	if targetPluginID == "" {
		return nil, fmt.Errorf("target_plugin_id is required")
	}
	if s.pluginID == "" {
		return nil, fmt.Errorf("server: plugin id not bound")
	}
	if s.publisher == nil {
		return nil, fmt.Errorf("server: event publisher not configured")
	}

	var payload json.RawMessage
	if p := req.GetPayload(); p != nil {
		raw, err := p.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("encode payload: %w", err)
		}
		payload = raw
	}

	env := events.Envelope{
		Channel:        events.ChannelPlugins,
		Event:          "plugin." + s.pluginID + "." + name,
		Data:           payload,
		TargetPluginID: targetPluginID,
	}
	if err := s.publisher.Publish(ctx, env); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	return &pluginv1.PublishEventToResponse{}, nil
}

// ListLibraries delegates to the LibraryLister, mapping the userID from the
// request through to the underlying data source.
func (s *RuntimeHostServer) ListLibraries(ctx context.Context, req *pluginv1.ListLibrariesRequest) (*pluginv1.ListLibrariesResponse, error) {
	if s.libs == nil {
		return &pluginv1.ListLibrariesResponse{}, nil
	}
	rows, err := s.libs.ListLibraries(ctx, req.GetUserId())
	if err != nil {
		return nil, fmt.Errorf("list libraries: %w", err)
	}
	resp := &pluginv1.ListLibrariesResponse{Libraries: make([]*pluginv1.Library, 0, len(rows))}
	for _, r := range rows {
		resp.Libraries = append(resp.Libraries, &pluginv1.Library{
			Id:        r.ID,
			Name:      r.Name,
			MediaType: r.MediaType,
		})
	}
	return resp, nil
}

// ListInstalledPlugins returns installed plugins and their advertised
// capabilities for peer discovery.
func (s *RuntimeHostServer) ListInstalledPlugins(ctx context.Context, _ *pluginv1.ListInstalledPluginsRequest) (*pluginv1.ListInstalledPluginsResponse, error) {
	if s.installedPlugins == nil {
		return &pluginv1.ListInstalledPluginsResponse{}, nil
	}
	rows, err := s.installedPlugins.ListInstalledPlugins(ctx)
	if err != nil {
		return nil, fmt.Errorf("list installed plugins: %w", err)
	}
	resp := &pluginv1.ListInstalledPluginsResponse{Plugins: make([]*pluginv1.InstalledPlugin, 0, len(rows))}
	for _, row := range rows {
		resp.Plugins = append(resp.Plugins, &pluginv1.InstalledPlugin{
			InstallationId: int64(row.InstallationID),
			PluginId:       row.PluginID,
			Version:        row.Version,
			Enabled:        row.Enabled,
			Capabilities:   row.Capabilities,
		})
	}
	return resp, nil
}

// SetGlobalConfigEntry persists a plugin-owned global config entry for this
// plugin installation.
func (s *RuntimeHostServer) SetGlobalConfigEntry(ctx context.Context, req *pluginv1.SetGlobalConfigEntryRequest) (*pluginv1.SetGlobalConfigEntryResponse, error) {
	key := strings.TrimSpace(req.GetKey())
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	if s.installationID == 0 {
		return nil, fmt.Errorf("server: installation id not bound")
	}
	if s.configSetter == nil {
		return nil, fmt.Errorf("server: config setter not configured")
	}
	value := map[string]any{}
	if req.GetValue() != nil {
		value = req.GetValue().AsMap()
	}
	if err := s.configSetter.SetGlobalConfigEntry(ctx, s.installationID, key, value); err != nil {
		return nil, fmt.Errorf("set global config entry: %w", err)
	}
	return &pluginv1.SetGlobalConfigEntryResponse{}, nil
}

// CheckMediaPresence delegates to the configured CatalogPresenceLookup.
// Returns the empty list when no catalog is configured.
func (s *RuntimeHostServer) CheckMediaPresence(ctx context.Context, req *pluginv1.CheckMediaPresenceRequest) (*pluginv1.CheckMediaPresenceResponse, error) {
	if len(req.GetIds()) > 100 {
		return nil, fmt.Errorf("ids: too many (%d), max 100", len(req.GetIds()))
	}
	if s.catalog == nil {
		return &pluginv1.CheckMediaPresenceResponse{}, nil
	}
	rows, err := s.catalog.LookupByExternalIDs(ctx, req.GetProvider(), req.GetMediaType(), req.GetIds())
	if err != nil {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	resp := &pluginv1.CheckMediaPresenceResponse{
		Present: make([]*pluginv1.MediaPresence, 0, len(rows)),
	}
	for _, r := range rows {
		resp.Present = append(resp.Present, &pluginv1.MediaPresence{
			ExternalId: r.ExternalID,
			MediaId:    r.MediaID,
			LibraryId:  r.LibraryID,
			Title:      r.Title,
		})
	}
	return resp, nil
}

// GetHostInfo reports the hosting process: public and loopback base URLs.
// NOTE (fork, stripped for SDK): the pinned plugin SDK predates the
// HostRole/HostName/NodeId/Listeners/IngressToken response fields, so only
// the base URLs are reported until the SDK is updated.
func (s *RuntimeHostServer) GetHostInfo(ctx context.Context, _ *pluginv1.GetHostInfoRequest) (*pluginv1.GetHostInfoResponse, error) {
	if s.hostInfo == nil {
		return nil, status.Error(codes.Unimplemented, "host info is not configured")
	}
	info, err := s.hostInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("host info: %w", err)
	}
	resp := &pluginv1.GetHostInfoResponse{
		PublicBaseUrl: strings.TrimRight(info.PublicBaseURL, "/"),
	}
	if resp.PublicBaseUrl != "" && info.PluginContentPrefix != "" && s.installationID > 0 {
		resp.PluginProxyBaseUrl = resp.PublicBaseUrl + info.PluginContentPrefix + "/plugins/" + strconv.Itoa(s.installationID)
	}
	for _, listener := range info.Listeners {
		if listener.Name == ListenerAPI && listener.Address != "" {
			resp.InternalBaseUrl = "http://" + listener.Address
		}
	}
	return resp, nil
}

func instanceStateError(err error) error {
	switch {
	case errors.Is(err, ErrInstanceStateKeyTooLong), errors.Is(err, ErrInstanceStateValueTooLarge):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrInstanceStateTooManyKeys):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, ErrInstanceStateUnavailable):
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return fmt.Errorf("instance state: %w", err)
}
