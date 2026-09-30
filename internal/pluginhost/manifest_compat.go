package pluginhost

import (
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// legacyVirtualStreamField is the CapabilityDescriptor field number the fork
// assigned virtual_stream_provider before upstream claimed 11 for
// network_access. The fork moved its descriptor to 13; binaries built
// against the old SDK still emit it at 11.
const legacyVirtualStreamField = 11

// manifestsMatch reports whether the installed manifest and the live manifest
// served by the plugin process describe the same plugin. It is proto.Equal
// plus one fork-compatibility fold: an old binary's virtual_stream_provider
// descriptor arrives at field 11 and decodes as unknown, while the installed
// manifest (parsed from JSON, where the field name is unchanged) carries it
// at field 13. Without the fold no pre-renumber binary can start —
// host.Start would refuse the virtual-library plugin right after a server
// upgrade. The fold only fills a missing typed descriptor from legacy
// unknown bytes; every other field, including the binary checksum, still
// compares strictly.
func manifestsMatch(installed, live *pluginv1.PluginManifest) bool {
	if proto.Equal(installed, live) {
		return true
	}
	normalized, ok := proto.Clone(live).(*pluginv1.PluginManifest)
	if !ok {
		return false
	}
	changed := false
	for _, c := range normalized.GetCapabilities() {
		if c.GetVirtualStreamProvider() == nil {
			if d := legacyVirtualStreamDescriptor(c); d != nil {
				c.VirtualStreamProvider = d
				changed = true
			}
		}
	}
	if !changed {
		return false
	}
	return proto.Equal(installed, stripLegacyVirtualUnknown(normalized))
}

// legacyVirtualStreamDescriptor parses unknown field 11 of a capability
// descriptor as a VirtualStreamProviderDescriptor. It returns nil when the
// field is absent or malformed.
func legacyVirtualStreamDescriptor(c *pluginv1.CapabilityDescriptor) *pluginv1.VirtualStreamProviderDescriptor {
	unknown := c.ProtoReflect().GetUnknown()
	for len(unknown) > 0 {
		num, typ, n := protowire.ConsumeTag(unknown)
		if n < 0 {
			return nil
		}
		unknown = unknown[n:]
		m := protowire.ConsumeFieldValue(num, typ, unknown)
		if m < 0 {
			return nil
		}
		if num == legacyVirtualStreamField && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(unknown[:m])
			var d pluginv1.VirtualStreamProviderDescriptor
			if err := proto.Unmarshal(v, &d); err == nil {
				return &d
			}
		}
		unknown = unknown[m:]
	}
	return nil
}

// stripLegacyVirtualUnknown drops unknown field 11 from every capability so
// a folded live manifest compares byte-clean against the installed one.
func stripLegacyVirtualUnknown(m *pluginv1.PluginManifest) *pluginv1.PluginManifest {
	for _, c := range m.GetCapabilities() {
		unknown := c.ProtoReflect().GetUnknown()
		if len(unknown) == 0 {
			continue
		}
		var kept []byte
		rest := unknown
		for len(rest) > 0 {
			num, typ, n := protowire.ConsumeTag(rest)
			if n < 0 {
				break
			}
			rest = rest[n:]
			m := protowire.ConsumeFieldValue(num, typ, rest)
			if m < 0 {
				break
			}
			if num != legacyVirtualStreamField {
				kept = protowire.AppendTag(kept, num, typ)
				kept = append(kept, rest[:m]...)
			}
			rest = rest[m:]
		}
		c.ProtoReflect().SetUnknown(kept)
	}
	return m
}
