// Package httpheader centralizes the Vio rebrand's HTTP header names.
//
// Wire rule: the server emits X-Vio-* headers. Every direct read accepts the
// legacy X-Silo-* spelling as a fallback so old clients keep working, with
// the X-Vio-* value winning when both are present.
//
// Frozen (never renamed): the OpenAPI extension keys x-silo-* and the
// problem/URN surface are owned by other lanes and stay untouched here.
package httpheader

import "net/http"

// Canonical header names emitted on the wire.
const (
	DeviceID       = "X-Vio-Device-Id"
	DeviceName     = "X-Vio-Device-Name"
	DevicePlatform = "X-Vio-Device-Platform"
	Client         = "X-Vio-Client"
	ClientVersion  = "X-Vio-Client-Version"
	ClientBuild    = "X-Vio-Client-Build"
	ClientChannel  = "X-Vio-Client-Channel"
	ClientFamily   = "X-Vio-Client-Family"
	MutationID     = "X-Vio-Mutation-Id"
	StreamToken    = "X-Vio-Stream-Token"
	// Plugin identity/theme headers stamped on proxied plugin requests.
	PluginUserID       = "X-Vio-User-Id"
	PluginUserRole     = "X-Vio-User-Role"
	PluginUserName     = "X-Vio-User-Name"
	PluginProfileName  = "X-Vio-Profile-Name"
	PluginProfilePrime = "X-Vio-Profile-Primary"
	PluginTheme        = "X-Vio-Theme"
)

// Legacy fallbacks accepted on ingest.
const (
	LegacyDeviceID       = "X-Silo-Device-Id"
	LegacyDeviceName     = "X-Silo-Device-Name"
	LegacyDevicePlatform = "X-Silo-Device-Platform"
	LegacyClient         = "X-Silo-Client"
	LegacyClientVersion  = "X-Silo-Client-Version"
	LegacyClientBuild    = "X-Silo-Client-Build"
	LegacyClientChannel  = "X-Silo-Client-Channel"
	LegacyClientFamily   = "X-Silo-Client-Family"
	LegacyMutationID     = "X-Silo-Mutation-Id"
	LegacyStreamToken    = "X-Silo-Stream-Token"
	LegacyPluginUserID   = "X-Silo-User-Id"
	LegacyPluginUserRole = "X-Silo-User-Role"
	LegacyPluginUserName = "X-Silo-User-Name"
	LegacyPluginProfName = "X-Silo-Profile-Name"
	LegacyPluginProfPrim = "X-Silo-Profile-Primary"
	LegacyPluginTheme    = "X-Silo-Theme"
)

// Get returns the X-Vio-* value when present, else the legacy X-Silo-* value.
func Get(h http.Header, name, legacy string) string {
	if h == nil {
		return ""
	}
	if v := h.Get(name); v != "" {
		return v
	}
	return h.Get(legacy)
}

// GetDeviceID reads the device identity header with legacy fallback.
func GetDeviceID(h http.Header) string { return Get(h, DeviceID, LegacyDeviceID) }

// GetDeviceName reads the device name header with legacy fallback.
func GetDeviceName(h http.Header) string { return Get(h, DeviceName, LegacyDeviceName) }

// GetDevicePlatform reads the device platform header with legacy fallback.
func GetDevicePlatform(h http.Header) string {
	return Get(h, DevicePlatform, LegacyDevicePlatform)
}

// GetClientFamily reads the client family header with legacy fallback.
func GetClientFamily(h http.Header) string { return Get(h, ClientFamily, LegacyClientFamily) }

// GetMutationID reads the mutation id header with legacy fallback.
func GetMutationID(h http.Header) string { return Get(h, MutationID, LegacyMutationID) }

// GetStreamToken reads the stream token header with legacy fallback.
func GetStreamToken(h http.Header) string { return Get(h, StreamToken, LegacyStreamToken) }

// ClientInfo holds the client identity headers with legacy fallback applied.
type ClientInfo struct {
	Name    string
	Version string
	Build   string
	Channel string
}

// GetClientInfo reads the X-Vio-Client-* family with legacy fallback.
func GetClientInfo(h http.Header) ClientInfo {
	return ClientInfo{
		Name:    Get(h, Client, LegacyClient),
		Version: Get(h, ClientVersion, LegacyClientVersion),
		Build:   Get(h, ClientBuild, LegacyClientBuild),
		Channel: Get(h, ClientChannel, LegacyClientChannel),
	}
}
