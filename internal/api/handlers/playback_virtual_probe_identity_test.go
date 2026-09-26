package handlers

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestApplyResolvedIdentity(t *testing.T) {
	full := virtualProbeIdentity{VideoHash: "hash-a", GUID: "guid-a", ReleaseName: "Release.A", ReleaseSize: 1234}

	t.Run("fills empty tiers", func(t *testing.T) {
		file := &models.MediaFile{}
		applyResolvedIdentity(file, full)
		if file.ProviderVideoHash != "hash-a" || file.ProviderGUID != "guid-a" ||
			file.ProviderReleaseName != "Release.A" || file.ProviderReleaseSize != 1234 {
			t.Fatalf("identity not stamped: %+v", file)
		}
	})

	t.Run("never overwrites stored tiers", func(t *testing.T) {
		file := &models.MediaFile{ProviderVideoHash: "hash-old", ProviderGUID: "guid-old", ProviderReleaseName: "Old", ProviderReleaseSize: 99}
		applyResolvedIdentity(file, full)
		if file.ProviderVideoHash != "hash-old" || file.ProviderGUID != "guid-old" ||
			file.ProviderReleaseName != "Old" || file.ProviderReleaseSize != 99 {
			t.Fatalf("stored identity overwritten: %+v", file)
		}
	})

	t.Run("nil file and empty identity are safe", func(t *testing.T) {
		applyResolvedIdentity(nil, full)
		file := &models.MediaFile{ProviderVideoHash: "hash-old"}
		applyResolvedIdentity(file, virtualProbeIdentity{})
		if file.ProviderVideoHash != "hash-old" || file.ProviderGUID != "" || file.ProviderReleaseSize != 0 {
			t.Fatalf("empty identity mutated the file: %+v", file)
		}
	})
}
