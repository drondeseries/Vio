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

// TestMatchProfileGenericHDRMatchesWholeHDRFamily proves the "4K HDR" profile
// (HDR: "hdr") matches every HDR format the classifier emits, including Dolby
// Vision, instead of rejecting the content it exists to match.
//
// This reverses the earlier pin that deliberately excluded DV. The exclusion
// was user-hostile: a DV-only listing made the "4K HDR" preset match zero
// candidates, which the resolver turns into a hard "no stream matches profile"
// failure for an auto-picked profile — playback lost entirely rather than a
// slightly-off selection. "HDR" names the family, so DV belongs in it; a
// profile that wants only Dolby Vision still asks for "dv" explicitly and
// stays exact.
func TestMatchProfileGenericHDRMatchesWholeHDRFamily(t *testing.T) {
	profile := QualityProfile{Label: "4K HDR", Resolution: "2160p", HDR: "hdr"}
	for _, candidateHDR := range []string{"hdr", hdrValueHDR10, hdrValueHDR10Plus, hdrValueDV} {
		candidate := stream.StreamCandidate{Resolution: "2160p", HDR: candidateHDR}
		if !MatchProfile(candidate, profile) {
			t.Fatalf("HDR %q did not satisfy a generic hdr profile", candidateHDR)
		}
	}
	// SDR (no HDR marker) still fails the requirement.
	if MatchProfile(stream.StreamCandidate{Resolution: "2160p"}, profile) {
		t.Fatal("generic hdr profile matched an SDR candidate")
	}

	dvProfile := QualityProfile{Label: "4K DV", Resolution: "2160p", HDR: "dv"}
	if !MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "dv"}, dvProfile) {
		t.Fatal("dv profile did not match dv content")
	}
	if MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: hdrValueHDR10}, dvProfile) {
		t.Fatal("dv profile matched HDR10 content")
	}
}

// TestMatchProfileUnknownResolutionDoesNotFailRequirement proves an unparsed
// resolution is treated as unknown rather than as a proven mismatch. A listing
// whose releases carry no explicit resolution marker must not be filtered to
// zero (a hard failure) by an auto-picked profile; the ranked order still
// prefers a candidate with a known, higher resolution.
func TestMatchProfileUnknownResolutionDoesNotFailRequirement(t *testing.T) {
	profile := QualityProfile{Label: "4K HDR", Resolution: "2160p", HDR: "hdr"}

	if !MatchProfile(stream.StreamCandidate{HDR: "hdr", Name: "Show.S01E05.REPACK"}, profile) {
		t.Fatal("unknown resolution failed the resolution requirement")
	}
	// A known resolution still gates exactly: 1080p does not satisfy 2160p.
	if MatchProfile(stream.StreamCandidate{Resolution: "1080p", HDR: "hdr"}, profile) {
		t.Fatal("a known 1080p candidate satisfied a 2160p requirement")
	}
	// A known 2160p candidate still matches.
	if !MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "hdr"}, profile) {
		t.Fatal("a known 2160p candidate failed a 2160p requirement")
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
	// A zero profile contributes no profile ordering. It does not disable the
	// shared scoring or the OriginalIndex tie-break, so equally-scored
	// candidates keep the provider's OriginalIndex order...
	zeroA := stream.StreamCandidate{Name: "1080p A", Resolution: "1080p", OriginalIndex: 0}
	zeroB := stream.StreamCandidate{Name: "1080p B", Resolution: "1080p", OriginalIndex: 1}
	zero := []stream.StreamCandidate{zeroA, zeroB}
	SortCandidatesForProfile(zero, QualityProfile{}, nil)
	if zero[0].OriginalIndex != 0 || zero[1].OriginalIndex != 1 {
		t.Fatalf("zero profile changed the provider order: %+v", zero)
	}
	// ...and a resolution difference still ranks, because the scoring keys
	// apply with no profile too (custom-format scoring is the same path).
	scored := []stream.StreamCandidate{removed, matched}
	SortCandidatesForProfile(scored, QualityProfile{}, nil)
	if scored[0].Resolution != "1080p" {
		t.Fatalf("zero profile disabled the shared resolution scoring: %+v", scored)
	}
}

// TestMatchProfileExcludeHDRSymmetricWithRequirement proves the exclude side
// uses the same sibling rule as the requirement side: ExcludeHDR "hdr" excludes
// the whole HDR family including Dolby Vision, while ExcludeHDR "dv" excludes
// only DV.
//
// This reverses the earlier pin that left DV behind an ExcludeHDR "hdr". With
// HDR "hdr" now accepting DV, the exclusion must cover it too or "exclude HDR"
// would mean "exclude everything except DV" — the opposite of its name.
func TestMatchProfileExcludeHDRSymmetricWithRequirement(t *testing.T) {
	hdrExclude := QualityProfile{Label: "no hdr", Resolution: "2160p", ExcludeHDR: "hdr"}
	for _, candidateHDR := range []string{"hdr", hdrValueHDR10, hdrValueHDR10Plus, hdrValueDV} {
		if MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: candidateHDR}, hdrExclude) {
			t.Fatalf("ExcludeHDR \"hdr\" did not exclude %q", candidateHDR)
		}
	}
	if !MatchProfile(stream.StreamCandidate{Resolution: "2160p"}, hdrExclude) {
		t.Fatal("ExcludeHDR \"hdr\" excluded an SDR candidate")
	}

	dvExclude := QualityProfile{Label: "no dv", Resolution: "2160p", ExcludeHDR: "dv"}
	if MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: "dv"}, dvExclude) {
		t.Fatal("ExcludeHDR \"dv\" did not exclude dv")
	}
	if !MatchProfile(stream.StreamCandidate{Resolution: "2160p", HDR: hdrValueHDR10}, dvExclude) {
		t.Fatal("ExcludeHDR \"dv\" excluded HDR10")
	}
}
