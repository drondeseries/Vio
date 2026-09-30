package pluginhost

import (
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// legacyVirtualStreamField is the CapabilityDescriptor field number the fork
// assigned virtual_stream_provider before moving it to 13: upstream's
// request-router seasons change assigned 11 to network_access_provider in
// its own tree, colliding with the fork's earlier assignment. Binaries
// built against the old fork SDK still emit the descriptor at 11.
const (
	legacyVirtualStreamField          = 11
	legacyVirtualStreamCapabilityType = "virtual_stream_provider.v1"
)

// manifestsMatch reports whether the installed manifest and the live manifest
// served by the plugin process describe the same plugin. It is proto.Equal
// plus one fork-compatibility fold: an old binary's virtual_stream_provider
// descriptor arrives at field 11 and decodes as unknown, while the installed
// manifest (parsed from JSON, where the field name is unchanged) carries it
// at field 13. Without the fold no pre-renumber binary can start —
// host.Start would refuse the virtual-library plugin right after a server
// upgrade. The fold applies per capability and only when the legacy bytes
// are unambiguous (exactly one well-formed bytes-typed field 11 and no
// typed descriptor); duplicates, malformed data, and unrelated unknown
// fields keep the strict verdict. Checksum and every other field always
// compare strictly.
func manifestsMatch(installed, live *pluginv1.PluginManifest) bool {
	if proto.Equal(installed, live) {
		return true
	}
	normalized, ok := proto.Clone(live).(*pluginv1.PluginManifest)
	if !ok {
		return false
	}
	folded := false
	for _, c := range normalized.GetCapabilities() {
		foldedLegacy, ok := foldLegacyVirtualDescriptor(c)
		if !ok {
			return false
		}
		folded = folded || foldedLegacy
	}
	if !folded {
		return false
	}
	return proto.Equal(installed, normalized)
}

// usesLegacyVirtualEncoding reports whether a live manifest carries the
// pre-renumber field-11 virtual descriptor bytes, for a deprecation log on
// compatibility-assisted starts.
func usesLegacyVirtualEncoding(live *pluginv1.PluginManifest) bool {
	for _, c := range live.GetCapabilities() {
		if c.GetType() == legacyVirtualStreamCapabilityType &&
			c.GetVirtualStreamProvider() == nil && hasLegacyVirtualBytes(c) {
			return true
		}
	}
	return false
}

func hasLegacyVirtualBytes(c *pluginv1.CapabilityDescriptor) bool {
	unknown := c.ProtoReflect().GetUnknown()
	for len(unknown) > 0 {
		num, typ, n := protowire.ConsumeTag(unknown)
		if n < 0 {
			return false
		}
		unknown = unknown[n:]
		m := protowire.ConsumeFieldValue(num, typ, unknown)
		if m < 0 {
			return false
		}
		if num == legacyVirtualStreamField && typ == protowire.BytesType {
			return true
		}
		unknown = unknown[m:]
	}
	return false
}

// foldLegacyVirtualDescriptor fills a missing typed virtual_stream_provider
// from exactly one well-formed legacy field-11 value, removing only those
// bytes and leaving every other unknown field untouched. It reports whether
// it folded, and false (refuse) on duplicates or malformed legacy data.
func foldLegacyVirtualDescriptor(c *pluginv1.CapabilityDescriptor) (bool, bool) {
	if c.GetVirtualStreamProvider() != nil {
		return false, true
	}
	// Only virtual-stream capabilities fold: legacy bytes anywhere else
	// stay significant for the strict comparison.
	if c.GetType() != legacyVirtualStreamCapabilityType {
		return false, true
	}
	unknown := c.ProtoReflect().GetUnknown()
	var kept []byte
	var folded *pluginv1.VirtualStreamProviderDescriptor
	rest := unknown
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return false, false
		}
		field := rest[:n]
		rest = rest[n:]
		if num != legacyVirtualStreamField || typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, rest)
			if m < 0 {
				return false, false
			}
			kept = append(kept, field...)
			kept = append(kept, rest[:m]...)
			rest = rest[m:]
			continue
		}
		v, m := protowire.ConsumeBytes(rest)
		if m < 0 {
			return false, false
		}
		rest = rest[m:]
		var d pluginv1.VirtualStreamProviderDescriptor
		if err := proto.Unmarshal(v, &d); err != nil {
			return false, false
		}
		if folded != nil {
			return false, false
		}
		folded = &d
	}
	if folded == nil {
		return false, true
	}
	c.VirtualStreamProvider = folded
	c.ProtoReflect().SetUnknown(kept)
	return true, true
}
