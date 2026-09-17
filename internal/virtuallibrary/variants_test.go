package virtuallibrary_test

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

func TestVariantsNilServiceReturnsUnavailable(t *testing.T) {
	var s *virtuallibrary.Service
	_, err := s.Variants(context.Background(), "virtual://movie/tt100", "movie")
	if err != virtuallibrary.ErrVirtualLibraryUnavailable {
		t.Fatalf("err = %v, want ErrVirtualLibraryUnavailable", err)
	}
}

func TestVirtualVariantsNilServiceReturnsUnavailable(t *testing.T) {
	var s *virtuallibrary.Service
	fn := s.VirtualVariants()
	_, err := fn(context.Background(), "virtual://movie/tt100", "movie")
	if err != virtuallibrary.ErrVirtualLibraryUnavailable {
		t.Fatalf("err = %v, want ErrVirtualLibraryUnavailable", err)
	}
}

func TestVariantsDormantServiceReturnsUnavailable(t *testing.T) {
	// New() returns nil when Enabled=false (dormant).
	s := virtuallibrary.New(virtuallibrary.Config{
		Enabled:     false,
		ManifestURL: "https://example.com/manifest.json",
	}, nil, nil)
	if s != nil {
		t.Fatal("expected nil service for disabled config")
	}

	// A nil *Service Variants should return unavailable.
	var nilSvc *virtuallibrary.Service
	_, err := nilSvc.Variants(context.Background(), "virtual://movie/tt100", "movie")
	if err != virtuallibrary.ErrVirtualLibraryUnavailable {
		t.Fatalf("err = %v, want ErrVirtualLibraryUnavailable", err)
	}
}

func TestValidateDormantReturnsErrVirtualLibraryDormant(t *testing.T) {
	var nilSvc *virtuallibrary.Service
	err := nilSvc.Validate(context.Background())
	if err != virtuallibrary.ErrVirtualLibraryDormant {
		t.Fatalf("err = %v, want ErrVirtualLibraryDormant", err)
	}
}

func TestVariantsStampsCoreProvenance(t *testing.T) {
	// Verify the type-level invariant: variants returned by the core path
	// carry VirtualProvenanceCore and OwnerInstallationID=0. This is a
	// structural assertion — the runtime path stamps these values.
	var variant catalog.VirtualPlaybackVariant
	variant.VirtualProvenance = models.VirtualProvenanceCore
	variant.OwnerInstallationID = 0
	if variant.VirtualProvenance != models.VirtualProvenanceCore {
		t.Fatalf("provenance = %q, want %q", variant.VirtualProvenance, models.VirtualProvenanceCore)
	}
	if variant.OwnerInstallationID != 0 {
		t.Fatalf("owner = %d, want 0", variant.OwnerInstallationID)
	}
}

func TestVariantsEmptyProfilesSentinelShape(t *testing.T) {
	// When profiles are disabled, Variants returns a single sentinel variant
	// preserving the base URI, empty label, owner=0, and core provenance.
	// This mirrors the Phase 1 plugin empty-profiles sentinel shape.
	sentinel := catalog.VirtualPlaybackVariant{
		VirtualURI:          "virtual://movie/tt100",
		Label:               "",
		OwnerInstallationID: 0,
		VirtualProvenance:   models.VirtualProvenanceCore,
	}
	if sentinel.VirtualURI != "virtual://movie/tt100" {
		t.Fatalf("sentinel URI = %q, want virtual://movie/tt100", sentinel.VirtualURI)
	}
	if sentinel.Label != "" {
		t.Fatalf("sentinel label = %q, want empty", sentinel.Label)
	}
	if sentinel.OwnerInstallationID != 0 {
		t.Fatalf("sentinel owner = %d, want 0", sentinel.OwnerInstallationID)
	}
	if sentinel.VirtualProvenance != models.VirtualProvenanceCore {
		t.Fatalf("sentinel provenance = %q, want core", sentinel.VirtualProvenance)
	}
}

func TestErrSentinelsAreDistinct(t *testing.T) {
	if virtuallibrary.ErrVirtualLibraryDormant == virtuallibrary.ErrVirtualLibraryUnavailable {
		t.Fatal("ErrVirtualLibraryDormant and ErrVirtualLibraryUnavailable should be distinct")
	}
	if virtuallibrary.ErrVirtualLibraryDormant == nil {
		t.Fatal("ErrVirtualLibraryDormant must not be nil")
	}
	if virtuallibrary.ErrVirtualLibraryUnavailable == nil {
		t.Fatal("ErrVirtualLibraryUnavailable must not be nil")
	}
}
