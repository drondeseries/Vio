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

// TestMatchProfileGenericHDRMatchesHDR10Family proves the "4K HDR" profile
// (HDR: "hdr") matches the HDR10 family the classifier emits, instead of
// rejecting the content it exists to match. Dolby Vision is a distinct format
// and is only matched by a profile that asks for "dv" explicitly.
func TestMatchProfileGenericHDRMatchesHDR10Family(t *testing.T) {
	profile := QualityProfile{Label: "4K HDR", Resolution: "2160p", HDR: "hdr"}
	for _, candidateHDR := range []string{"hdr", "hdr10", "hdr10+"} {
		candidate := stream.StreamCandidate{Resolution: "2160p", HDR: candidateHDR}
		if !MatchProfile(candidate, profile) {
			t.Fatalf("HDR %q did not satisfy a generic hdr profile", candidateHDR)
		}
	}
	if MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "dv"}, profile) {
		t.Fatal("generic hdr profile matched Dolby Vision, which is not HDR10")
	}

	dvProfile := QualityProfile{Label: "4K DV", Resolution: "2160p", HDR: "dv"}
	if !MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "dv"}, dvProfile) {
		t.Fatal("dv profile did not match dv content")
	}
	if MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "hdr10"}, dvProfile) {
		t.Fatal("dv profile matched HDR10 content")
	}
}

// TestSortCandidatesForProfileRanksProfileMatchedFirst proves the shared
// ranking keeps the auto picker's candidate list in agreement with the
// resolver's profile filter: a profile-satisfying candidate ranks ahead of a
// profile-removed one even when the provider listed the removed one first.
func TestSortCandidatesForProfileRanksProfileMatchedFirst(t *testing.T) {
	profile := QualityProfile{Label: "fhd", Resolution: "1080p"}
	removed := stream.StreamCandidate{Name: "720p", Resolution: "720p", OriginalIndex: 0}
	matched := stream.StreamCandidate{Name: "1080p", Resolution: "1080p", OriginalIndex: 1}
	candidates := []stream.StreamCandidate{removed, matched}

	SortCandidatesForProfile(candidates, profile, nil)

	if candidates[0].Resolution != "1080p" {
		t.Fatalf("profile-matching candidate not ranked first: %+v", candidates)
	}
	// A zero profile imposes no profile ordering.
	zero := []stream.StreamCandidate{removed, matched}
	SortCandidatesForProfile(zero, QualityProfile{}, nil)
	if zero[0].Resolution != "720p" {
		t.Fatalf("zero profile changed the order: %+v", zero)
	}
}

// TestMatchProfileExcludeHDRSymmetricWithRequirement proves the exclude side
// uses the same sibling rule as the requirement side: ExcludeHDR "hdr" excludes
// the HDR10 family but not Dolby Vision, while ExcludeHDR "dv" excludes only
// DV.
func TestMatchProfileExcludeHDRSymmetricWithRequirement(t *testing.T) {
	hdrExclude := QualityProfile{Label: "no hdr", Resolution: "2160p", ExcludeHDR: "hdr"}
	for _, candidateHDR := range []string{"hdr", "hdr10", "hdr10+"} {
		if MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: candidateHDR}, hdrExclude) {
			t.Fatalf("ExcludeHDR \"hdr\" did not exclude %q", candidateHDR)
		}
	}
	if !MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "dv"}, hdrExclude) {
		t.Fatal("ExcludeHDR \"hdr\" excluded Dolby Vision")
	}

	dvExclude := QualityProfile{Label: "no dv", Resolution: "2160p", ExcludeHDR: "dv"}
	if MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "dv"}, dvExclude) {
		t.Fatal("ExcludeHDR \"dv\" did not exclude dv")
	}
	if !MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "hdr10"}, dvExclude) {
		t.Fatal("ExcludeHDR \"dv\" excluded HDR10")
	}
}
