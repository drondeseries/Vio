package pluginhost

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func oldStyleCapability() *pluginv1.CapabilityDescriptor {
	// What a pre-renumber binary serves: the virtual descriptor at field 11
	// arrives as unknown bytes under the new SDK.
	raw, err := proto.Marshal(&pluginv1.VirtualStreamProviderDescriptor{})
	if err != nil {
		panic(err)
	}
	capability := &pluginv1.CapabilityDescriptor{
		Type: "virtual_stream_provider.v1",
		Id:   "streams",
	}
	var unknown []byte
	unknown = protowire.AppendTag(unknown, legacyVirtualStreamField, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, raw)
	capability.ProtoReflect().SetUnknown(unknown)
	return capability
}

func TestManifestsMatchFoldsLegacyVirtualDescriptor(t *testing.T) {
	installed := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Version:  "1.0",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{
				Type:                  "virtual_stream_provider.v1",
				Id:                    "streams",
				VirtualStreamProvider: &pluginv1.VirtualStreamProviderDescriptor{},
			},
		},
	}
	live := &pluginv1.PluginManifest{
		PluginId:     "old-plugin",
		Version:      "1.0",
		Checksum:     "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{oldStyleCapability()},
	}
	if proto.Equal(installed, live) {
		t.Fatal("want the renumber to break strict equality, else the shim is untested")
	}
	if !manifestsMatch(installed, live) {
		t.Fatal("old field-11 virtual descriptor must fold onto field 13")
	}
}

func TestManifestsMatchStillRejectsTampering(t *testing.T) {
	installed := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{
				Type:                  "virtual_stream_provider.v1",
				Id:                    "streams",
				VirtualStreamProvider: &pluginv1.VirtualStreamProviderDescriptor{},
			},
		},
	}
	tampered := &pluginv1.PluginManifest{
		PluginId:     "old-plugin",
		Checksum:     "different-checksum",
		Capabilities: []*pluginv1.CapabilityDescriptor{oldStyleCapability()},
	}
	if manifestsMatch(installed, tampered) {
		t.Fatal("checksum mismatch must still refuse the plugin")
	}
	// Malformed legacy bytes cannot fold: strict equality still refuses.
	broken := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{Type: "virtual_stream_provider.v1", Id: "streams"},
		},
	}
	broken.GetCapabilities()[0].ProtoReflect().SetUnknown(
		append(protowire.AppendTag(nil, legacyVirtualStreamField, protowire.BytesType), 0xff),
	)
	if manifestsMatch(installed, broken) {
		t.Fatal("malformed legacy bytes must still refuse the plugin")
	}
}

func legacyBytes(t *testing.T, d *pluginv1.VirtualStreamProviderDescriptor) []byte {
	t.Helper()
	raw, err := proto.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	out := protowire.AppendTag(nil, legacyVirtualStreamField, protowire.BytesType)
	return protowire.AppendBytes(out, raw)
}

func TestManifestsMatchRejectsDuplicateLegacyDescriptors(t *testing.T) {
	installed := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{
				Type:                  "virtual_stream_provider.v1",
				Id:                    "streams",
				VirtualStreamProvider: &pluginv1.VirtualStreamProviderDescriptor{},
			},
		},
	}
	dup := append(legacyBytes(t, &pluginv1.VirtualStreamProviderDescriptor{}),
		legacyBytes(t, &pluginv1.VirtualStreamProviderDescriptor{})...)
	live := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{Type: "virtual_stream_provider.v1", Id: "streams"},
		},
	}
	live.GetCapabilities()[0].ProtoReflect().SetUnknown(dup)
	if manifestsMatch(installed, live) {
		t.Fatal("duplicate legacy descriptors must refuse, not fold the first")
	}
}

func TestManifestsMatchKeepsUnrelatedLegacyBytesSignificant(t *testing.T) {
	installed := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{Type: "metadata_provider.v1", Id: "meta"},
			{
				Type:                  "virtual_stream_provider.v1",
				Id:                    "streams",
				VirtualStreamProvider: &pluginv1.VirtualStreamProviderDescriptor{},
			},
		},
	}
	live := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{Type: "metadata_provider.v1", Id: "meta"},
			{Type: "virtual_stream_provider.v1", Id: "streams"},
		},
	}
	// Legacy bytes on an unrelated capability must not be swept away by a
	// successful fold elsewhere.
	stray := legacyBytes(t, &pluginv1.VirtualStreamProviderDescriptor{})
	live.GetCapabilities()[0].ProtoReflect().SetUnknown(stray)
	live.GetCapabilities()[1].ProtoReflect().SetUnknown(
		legacyBytes(t, &pluginv1.VirtualStreamProviderDescriptor{}))
	if manifestsMatch(installed, live) {
		t.Fatal("legacy bytes on an unrelated capability must still mismatch")
	}
}

func TestManifestsMatchDoesNotMutateInputs(t *testing.T) {
	installed := &pluginv1.PluginManifest{
		PluginId: "old-plugin",
		Checksum: "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{
				Type:                  "virtual_stream_provider.v1",
				Id:                    "streams",
				VirtualStreamProvider: &pluginv1.VirtualStreamProviderDescriptor{},
			},
		},
	}
	live := &pluginv1.PluginManifest{
		PluginId:     "old-plugin",
		Version:      "1.0",
		Checksum:     "abc",
		Capabilities: []*pluginv1.CapabilityDescriptor{oldStyleCapability()},
	}
	beforeLive := proto.Clone(live).(*pluginv1.PluginManifest)
	beforeInstalled := proto.Clone(installed).(*pluginv1.PluginManifest)
	_ = manifestsMatch(installed, live)
	if !proto.Equal(live, beforeLive) || !proto.Equal(installed, beforeInstalled) {
		t.Fatal("manifestsMatch must not mutate its inputs")
	}
}
