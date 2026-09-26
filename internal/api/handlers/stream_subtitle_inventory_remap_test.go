package handlers

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// remapSubtitleInventoryIdentityV3 must translate an ordinal from the old
// inventory onto the bound candidate even when the two share a catalog-row id:
// a same-row candidate rotation re-probes the row in place, so id equality
// proves nothing. The returned file is always the bound candidate.
func TestRemapSubtitleInventoryIdentitySameIDDifferentCandidate(t *testing.T) {
	handler := &StreamHandler{}
	// Same catalog row id, different underlying release.
	old := &models.MediaFile{ID: 7, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "fra"},
	}}
	bound := &models.MediaFile{ID: 7, SubtitleTracks: []models.SubtitleTrack{
		{Index: 4, Codec: "subrip", Language: "eng"},
		{Index: 7, Codec: "subrip", Language: "fra"},
	}}
	file, index, err := handler.remapSubtitleInventoryIdentityV3(context.Background(), old, bound, 0)
	if err != nil {
		t.Fatalf("same-id remap: %v", err)
	}
	if file != bound {
		t.Fatalf("remap returned file %p (id %d), want the bound candidate", file, file.ID)
	}
	if index != 1 {
		t.Fatalf("remap index = %d, want 1 (the bound candidate's French track)", index)
	}
}

// Reordered embedded tracks must map by language/codec identity, not ordinal.
func TestRemapSubtitleInventoryIdentityReorderedEmbeddedTracks(t *testing.T) {
	handler := &StreamHandler{}
	old := &models.MediaFile{ID: 1, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "fra"},
		{Index: 2, Codec: "subrip", Language: "eng"},
	}}
	bound := &models.MediaFile{ID: 2, SubtitleTracks: []models.SubtitleTrack{
		{Index: 9, Codec: "subrip", Language: "eng"},
		{Index: 7, Codec: "subrip", Language: "fra"},
	}}
	tests := []struct {
		name   string
		source int
		want   int
	}{
		{"french at old ordinal 0", 0, 1},
		{"english at old ordinal 1", 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, index, err := handler.remapSubtitleInventoryIdentityV3(context.Background(), old, bound, tt.source)
			if err != nil {
				t.Fatalf("remap: %v", err)
			}
			if file != bound {
				t.Fatalf("remap returned file %p, want the bound candidate", file)
			}
			if index != tt.want {
				t.Fatalf("remap index = %d, want %d", index, tt.want)
			}
		})
	}
}

// Externally reordered sidecars must map by language/format/flags, and the
// bound candidate's published ordinal is what comes back.
func TestRemapSubtitleInventoryIdentityExternalReorder(t *testing.T) {
	handler := &StreamHandler{}
	old := &models.MediaFile{ID: 1, ExternalSubtitles: []models.ExternalSubtitle{
		{Path: "/old/eng.srt", Language: "eng", Format: "srt"},
		{Path: "/old/fra.srt", Language: "fra", Format: "srt"},
	}}
	bound := &models.MediaFile{ID: 2, ExternalSubtitles: []models.ExternalSubtitle{
		{Path: "/new/fra.srt", Language: "fra", Format: "srt"},
		{Path: "/new/eng.srt", Language: "eng", Format: "srt"},
	}}
	file, index, err := handler.remapSubtitleInventoryIdentityV3(context.Background(), old, bound, 0)
	if err != nil {
		t.Fatalf("external reorder remap: %v", err)
	}
	if file != bound {
		t.Fatalf("remap returned file %p, want the bound candidate", file)
	}
	if index != 1 {
		t.Fatalf("remap index = %d, want 1 (the bound candidate's English sidecar)", index)
	}
}

// A stale container-index pin must be resolved against the old evidence first,
// then the named track mapped onto the bound candidate even though the bound
// container indices changed. resolveSubtitleEditionSwitch alone cannot do this:
// the two rows share an id, so its unchanged-edition shortcut would return the
// evidence verbatim.
func TestRemapSubtitleFromEvidenceChangedContainerIndicesWithStalePin(t *testing.T) {
	handler := &StreamHandler{}
	old := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=candidate-a",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 1, Codec: "subrip", Language: "fra"},
		},
	}
	bound := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=candidate-b",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 5, Codec: "subrip", Language: "eng"},
			{Index: 7, Codec: "subrip", Language: "fra"},
		},
	}
	// The client's pin names the old container index, which no longer resolves
	// against the bound candidate.
	query := url.Values{playback.EmbeddedSubtitleStreamIndexParamV3: {"1"}}
	file, index, err := handler.remapSubtitleFromEvidenceV3(context.Background(), old, bound, 0, query)
	if err != nil {
		t.Fatalf("stale-pin remap: %v", err)
	}
	if file != bound {
		t.Fatalf("remap returned file %p (path %q), want the bound candidate", file, file.FilePath)
	}
	if index != 1 {
		t.Fatalf("remap index = %d, want 1 (the bound candidate's French track)", index)
	}
}

// A pin that names no track in the old evidence must not fall through to an
// unrelated ordinal on the bound candidate.
func TestRemapSubtitleFromEvidenceUnresolvablePinUnavailable(t *testing.T) {
	handler := &StreamHandler{}
	old := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=candidate-a",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 1, Codec: "subrip", Language: "fra"},
		},
	}
	bound := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=candidate-b",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 5, Codec: "subrip", Language: "eng"},
			{Index: 7, Codec: "subrip", Language: "fra"},
		},
	}
	// A well-formed sidecar path key that no evidence track matches: the
	// evidence has no equivalent to map, so the request degrades rather than
	// serving the bound candidate at an invented ordinal.
	query := url.Values{playback.ExternalSubtitleKeyParamV3: {strings.Repeat("ab", 32)}}
	file, index, err := handler.remapSubtitleFromEvidenceV3(context.Background(), old, bound, 0, query)
	if err == nil {
		t.Fatalf("unresolvable pin served file=%p index=%d instead of degrading", file, index)
	}
	if !errors.Is(err, errSubtitleIdentityUnavailable) {
		t.Fatalf("err = %v, want errSubtitleIdentityUnavailable", err)
	}
}

// When the bound candidate has no equivalent track, the remap must report the
// selection unavailable rather than serve an unrelated ordinal.
func TestRemapSubtitleInventoryIdentityMissingEquivalentUnavailable(t *testing.T) {
	handler := &StreamHandler{}
	old := &models.MediaFile{ID: 1, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "hdmv_pgs_subtitle", Language: "fra"},
	}}
	bound := &models.MediaFile{ID: 2, SubtitleTracks: []models.SubtitleTrack{
		{Index: 4, Codec: "subrip", Language: "eng"},
	}}
	file, index, err := handler.remapSubtitleInventoryIdentityV3(context.Background(), old, bound, 0)
	if err == nil {
		t.Fatalf("missing equivalent served file=%p index=%d instead of degrading", file, index)
	}
	if !errors.Is(err, errSubtitleIdentityUnavailable) {
		t.Fatalf("err = %v, want errSubtitleIdentityUnavailable", err)
	}
}

// The unchanged-edition shortcut stays outside the inventory remap: identical
// ids still return the named file unchanged, so ordinary same-edition requests
// are untouched by the extraction.
func TestResolveSubtitleEditionSwitchUnchangedEditionShortcut(t *testing.T) {
	handler := &StreamHandler{}
	named := &models.MediaFile{ID: 42, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "fra"},
	}}
	query := url.Values{playback.EmbeddedSubtitleStreamIndexParamV3: {"1"}}
	file, index, err := handler.resolveSubtitleEditionSwitch(context.Background(), named, named, 0, query)
	if err != nil {
		t.Fatalf("unchanged edition: %v", err)
	}
	if file != named || index != 0 {
		t.Fatalf("unchanged edition resolved file=%p index=%d, want the named file at index 0", file, index)
	}
}
