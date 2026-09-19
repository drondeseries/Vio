package quality

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// TestMatchProfileVisualTagUsesTokenBoundaries pins the visual_tag fallback to
// the package's token/boundary matcher: a plain substring made "dv" match
// "DVD-Rip", silently admitting SDR DVD rips into a Dolby Vision profile.
func TestMatchProfileVisualTagUsesTokenBoundaries(t *testing.T) {
	profile := QualityProfile{Label: "DV", VisualTag: "dv"}

	if MatchProfile(stream.StreamCandidate{Name: "Movie.2001.DVD-Rip.XviD"}, profile) {
		t.Fatal("visual_tag \"dv\" matched DVD-Rip through a substring fallback")
	}
	if !MatchProfile(stream.StreamCandidate{Name: "Movie.2021.2160p.DV.HDR.WEB-DL"}, profile) {
		t.Fatal("visual_tag \"dv\" did not match a Dolby Vision release")
	}
	// Exact parsed VisualTags matching stays authoritative even when the text
	// does not carry the token.
	if !MatchProfile(stream.StreamCandidate{Name: "Movie.2021.2160p.WEB-DL", VisualTags: []string{"dv"}}, profile) {
		t.Fatal("exact parsed VisualTags match was lost")
	}
}

// TestMatchProfileSizeBoundsAsymmetricForUnknownSize documents and pins the
// deliberate asymmetry: an unknown size fails a minimum (the candidate must
// prove it meets the requirement) but passes a maximum (only a known size can
// exceed the ceiling).
func TestMatchProfileSizeBoundsAsymmetricForUnknownSize(t *testing.T) {
	unknown := stream.StreamCandidate{Name: "Movie.2024.1080p.WEB-DL"}
	if MatchProfile(unknown, QualityProfile{MinSizeGB: 1}) {
		t.Fatal("unknown size was admitted by a minimum")
	}
	if !MatchProfile(unknown, QualityProfile{MaxSizeGB: 1}) {
		t.Fatal("unknown size was rejected by a maximum")
	}

	undersized := stream.StreamCandidate{Name: "x", FileSize: 500_000_000}
	if MatchProfile(undersized, QualityProfile{MinSizeGB: 1}) {
		t.Fatal("undersized candidate passed a minimum")
	}
	oversized := stream.StreamCandidate{Name: "x", FileSize: 2_000_000_000}
	if MatchProfile(oversized, QualityProfile{MaxSizeGB: 1}) {
		t.Fatal("oversized candidate passed a maximum")
	}
}

// TestSortCandidatesForProfileRecordsRejected proves the rejected verdict is
// written onto the candidate (not just used for an internal sort) so it can be
// carried through device ranking and applied as the final ordering step.
func TestSortCandidatesForProfileRecordsRejected(t *testing.T) {
	formats := []CustomFormat{{
		Name: "No CAM", Pattern: "cam", PatternType: "token", Reject: true, Enabled: true,
	}}
	accepted := stream.StreamCandidate{Name: "Movie.2024.1080p.WEB-DL", OriginalIndex: 1}
	rejected := stream.StreamCandidate{Name: "Movie.2024.CAM.1080p", OriginalIndex: 0}
	candidates := []stream.StreamCandidate{rejected, accepted}

	SortCandidatesForProfile(candidates, QualityProfile{}, formats)

	if candidates[0].CustomFormatRejected {
		t.Fatalf("rejected candidate sorted first: %+v", candidates)
	}
	if !candidates[1].CustomFormatRejected {
		t.Fatalf("rejected verdict not recorded on the candidate: %+v", candidates)
	}
	if names := RejectingFormatNames(candidates[1], formats); len(names) != 1 || names[0] != "No CAM" {
		t.Fatalf("RejectingFormatNames = %v, want [No CAM]", names)
	}
}
