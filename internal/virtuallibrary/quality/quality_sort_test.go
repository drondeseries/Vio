package quality

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// --- evaluators -------------------------------------------------------------

// TestSortValueForAttributes pins each attribute evaluator, including which
// values the criterion treats as unknown.
func TestSortValueForAttributes(t *testing.T) {
	tenBit := stream.StreamCandidate{Is10Bit: true}
	eightBit := stream.StreamCandidate{}

	tests := []struct {
		name      string
		attribute string
		candidate stream.StreamCandidate
		profile   QualityProfile
		wantValue int64
		wantKnown bool
	}{
		{"size known", sortAttributeSize, stream.StreamCandidate{FileSize: 1000}, QualityProfile{}, 1000, true},
		{"size unknown", sortAttributeSize, stream.StreamCandidate{FileSize: 0}, QualityProfile{}, 0, false},
		{"bitrate known", sortAttributeBitrate, stream.StreamCandidate{Bitrate: 8000}, QualityProfile{}, 8000, true},
		{"bitrate unknown", sortAttributeBitrate, stream.StreamCandidate{Bitrate: 0}, QualityProfile{}, 0, false},
		{"resolution 2160p", sortAttributeResolution, stream.StreamCandidate{Resolution: "2160p"}, QualityProfile{}, 4, true},
		{"resolution 480p", sortAttributeResolution, stream.StreamCandidate{Resolution: "480p"}, QualityProfile{}, 1, true},
		{"resolution unknown", sortAttributeResolution, stream.StreamCandidate{Resolution: "weird"}, QualityProfile{}, 0, false},
		{"audio 7.1", sortAttributeAudioChannels, stream.StreamCandidate{AudioChannels: "7.1"}, QualityProfile{}, 3, true},
		{"audio unknown", sortAttributeAudioChannels, stream.StreamCandidate{AudioChannels: "mono"}, QualityProfile{}, 0, false},
		{"bit depth 10-bit", sortAttributeBitDepth, tenBit, QualityProfile{}, 1, true},
		{"bit depth 8-bit is a known value", sortAttributeBitDepth, eightBit, QualityProfile{}, 0, true},
		{"score is known even at zero", sortAttributeScore, stream.StreamCandidate{QualityScore: 0}, QualityProfile{}, 0, true},
		{"score negative", sortAttributeScore, stream.StreamCandidate{QualityScore: -25}, QualityProfile{}, -25, true},
		{"hdr dv", sortAttributeHDR, stream.StreamCandidate{HDR: "dv"}, QualityProfile{}, 0, true},
		{"hdr hdr10+", sortAttributeHDR, stream.StreamCandidate{HDR: "hdr10+"}, QualityProfile{}, 1, true},
		{"hdr hdr10", sortAttributeHDR, stream.StreamCandidate{HDR: "hdr10"}, QualityProfile{}, 2, true},
		{"hdr generic", sortAttributeHDR, stream.StreamCandidate{HDR: "hdr"}, QualityProfile{}, 3, true},
		{"hdr none", sortAttributeHDR, stream.StreamCandidate{HDR: ""}, QualityProfile{}, 4, true},
		{"source remux", sortAttributeSource, stream.StreamCandidate{SourceType: "remux"}, QualityProfile{}, 0, true},
		{"source bluray", sortAttributeSource, stream.StreamCandidate{SourceType: "bluray"}, QualityProfile{}, 1, true},
		{"source web-dl", sortAttributeSource, stream.StreamCandidate{SourceType: "web-dl"}, QualityProfile{}, 2, true},
		{"source hdtv", sortAttributeSource, stream.StreamCandidate{SourceType: "hdtv"}, QualityProfile{}, 3, true},
		{"source unknown", sortAttributeSource, stream.StreamCandidate{SourceType: "cam"}, QualityProfile{}, 4, true},
		{"confirmed true", sortAttributeConfirmed, stream.StreamCandidate{SourceConfirmed: true}, QualityProfile{}, 0, true},
		{"confirmed false", sortAttributeConfirmed, stream.StreamCandidate{SourceConfirmed: false}, QualityProfile{}, 1, true},
		{"language exact", sortAttributeLanguage, stream.StreamCandidate{AudioLanguages: []string{"pt-BR"}}, QualityProfile{Language: "pt-BR"}, 0, true},
		{"language multi", sortAttributeLanguage, stream.StreamCandidate{IsMultiAudio: true}, QualityProfile{Language: "pt-BR"}, 3, true},
		{"language no match", sortAttributeLanguage, stream.StreamCandidate{AudioLanguages: []string{"eng"}}, QualityProfile{Language: "pt-BR"}, 0, false},
		{"language no preference", sortAttributeLanguage, stream.StreamCandidate{AudioLanguages: []string{"eng"}}, QualityProfile{}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value, known := sortValueFor(tc.attribute, tc.candidate, tc.profile)
			if value != tc.wantValue || known != tc.wantKnown {
				t.Fatalf("sortValueFor(%s) = (%d, %v), want (%d, %v)", tc.attribute, value, known, tc.wantValue, tc.wantKnown)
			}
		})
	}
}

// --- composition ------------------------------------------------------------

// TestSortCriteriaCompositionScoreVsSize proves the criteria order decides the
// ranking, not a fixed tail: the same candidate set ranks differently when the
// custom-format score key is first vs the size key.
func TestSortCriteriaCompositionScoreVsSize(t *testing.T) {
	formats := []CustomFormat{{Name: "High", Pattern: "High", PatternType: "token", Score: 100, Enabled: true}}
	makeCandidates := func() []stream.StreamCandidate {
		return []stream.StreamCandidate{
			{Name: "High.1080p.WEB-DL", FileSize: 1000, OriginalIndex: 0},
			{Name: "Low.1080p.WEB-DL", FileSize: 5000, OriginalIndex: 1},
		}
	}
	scoreFirst := QualityProfile{Sort: []SortCriterion{
		{Attribute: sortAttributeScore, Direction: sortDirectionDesc},
		{Attribute: sortAttributeSize, Direction: sortDirectionDesc},
	}}
	sizeFirst := QualityProfile{Sort: []SortCriterion{
		{Attribute: sortAttributeSize, Direction: sortDirectionDesc},
		{Attribute: sortAttributeScore, Direction: sortDirectionDesc},
	}}

	scored := makeCandidates()
	SortCandidatesForProfile(scored, scoreFirst, formats)
	if scored[0].Name != "High.1080p.WEB-DL" {
		t.Fatalf("score-first order = %q first, want the higher-scored candidate", scored[0].Name)
	}

	sized := makeCandidates()
	SortCandidatesForProfile(sized, sizeFirst, formats)
	if sized[0].Name != "Low.1080p.WEB-DL" {
		t.Fatalf("size-first order = %q first, want the larger candidate", sized[0].Name)
	}
}

// TestSortUnknownValueSortsLast proves an unknown numeric value sorts after
// every known value and still falls through to the next criterion, under both
// default (descending) and explicit ascending direction.
func TestSortUnknownValueSortsLast(t *testing.T) {
	for _, direction := range []string{sortDirectionDesc, sortDirectionAsc} {
		t.Run(direction, func(t *testing.T) {
			known := stream.StreamCandidate{Name: "known", FileSize: 100, OriginalIndex: 1}
			unknown := stream.StreamCandidate{Name: "unknown", FileSize: 0, OriginalIndex: 0}
			candidates := []stream.StreamCandidate{unknown, known}
			SortCandidatesForProfile(candidates, QualityProfile{Sort: []SortCriterion{
				{Attribute: sortAttributeSize, Direction: direction},
			}}, nil)
			if candidates[0].Name != "known" {
				t.Fatalf("unknown size sorted first under %s: %+v", direction, candidates)
			}
		})
	}

	// A known-value difference still respects ascending direction.
	small := stream.StreamCandidate{Name: "small", FileSize: 100, OriginalIndex: 0}
	large := stream.StreamCandidate{Name: "large", FileSize: 900, OriginalIndex: 1}
	candidates := []stream.StreamCandidate{large, small}
	SortCandidatesForProfile(candidates, QualityProfile{Sort: []SortCriterion{
		{Attribute: sortAttributeSize, Direction: sortDirectionAsc},
	}}, nil)
	if candidates[0].Name != "small" {
		t.Fatalf("ascending size order = %q first, want the smaller", candidates[0].Name)
	}

	// An unknown primary key falls through to a known secondary key.
	a := stream.StreamCandidate{Name: "a", FileSize: 0, Bitrate: 100, OriginalIndex: 0}
	b := stream.StreamCandidate{Name: "b", FileSize: 0, Bitrate: 900, OriginalIndex: 1}
	candidates = []stream.StreamCandidate{a, b}
	SortCandidatesForProfile(candidates, QualityProfile{Sort: []SortCriterion{
		{Attribute: sortAttributeSize, Direction: sortDirectionDesc},
		{Attribute: sortAttributeBitrate, Direction: sortDirectionDesc},
	}}, nil)
	if candidates[0].Name != "b" {
		t.Fatalf("unknown primary key did not fall through to bitrate: %+v", candidates)
	}
}

// --- stability --------------------------------------------------------------

// TestSortEqualKeysFallsToOriginalIndexAndVariantID proves an all-equal key set
// still yields a deterministic order: OriginalIndex first, then the candidate
// variant id as the final stable tie-break.
func TestSortEqualKeysFallsToOriginalIndexAndVariantID(t *testing.T) {
	profile := QualityProfile{Sort: []SortCriterion{{Attribute: sortAttributeSize, Direction: sortDirectionDesc}}}

	// Distinct OriginalIndex: the provider order wins.
	first := stream.StreamCandidate{Name: "First", URL: "http://host/a.mkv", FileSize: 0, OriginalIndex: 0}
	second := stream.StreamCandidate{Name: "Second", URL: "http://host/b.mkv", FileSize: 0, OriginalIndex: 1}
	candidates := []stream.StreamCandidate{second, first}
	SortCandidatesForProfile(candidates, profile, nil)
	if candidates[0].OriginalIndex != 0 || candidates[1].OriginalIndex != 1 {
		t.Fatalf("equal keys did not fall back to OriginalIndex: %+v", candidates)
	}

	// Equal OriginalIndex: the variant id breaks the tie deterministically.
	left := stream.StreamCandidate{Name: "Left", URL: "http://host/left.mkv", OriginalIndex: 7}
	right := stream.StreamCandidate{Name: "Right", URL: "http://host/right.mkv", OriginalIndex: 7}
	leftID, rightID := stream.CandidateVariantID(left), stream.CandidateVariantID(right)
	if leftID == "" || rightID == "" || leftID == rightID {
		t.Fatalf("test needs distinct variant ids: %q %q", leftID, rightID)
	}
	candidates = []stream.StreamCandidate{right, left}
	SortCandidatesForProfile(candidates, profile, nil)
	if stream.CandidateVariantID(candidates[0]) >= stream.CandidateVariantID(candidates[1]) {
		t.Fatalf("equal OriginalIndex did not fall back to the variant id: %v", []string{
			stream.CandidateVariantID(candidates[0]), stream.CandidateVariantID(candidates[1]),
		})
	}
}

// --- validation -------------------------------------------------------------

func TestValidateSortCriteria(t *testing.T) {
	valid := func(sortCriteria []SortCriterion) error {
		cfg := QualityConfig{EnableProfiles: true, Profiles: []QualityProfile{{Label: "p", Sort: sortCriteria}}}
		return cfg.Validate()
	}

	if err := valid(nil); err != nil {
		t.Fatalf("empty sort rejected: %v", err)
	}
	if err := valid([]SortCriterion{{Attribute: sortAttributeSize}, {Attribute: sortAttributeHDR, Direction: sortDirectionAsc}}); err != nil {
		t.Fatalf("valid sort rejected: %v", err)
	}
	if err := valid([]SortCriterion{{Attribute: "framerate"}}); err == nil {
		t.Fatal("unknown sort attribute accepted")
	}
	if err := valid([]SortCriterion{{Attribute: sortAttributeSize, Direction: "sideways"}}); err == nil {
		t.Fatal("bad sort direction accepted")
	}
	tooMany := make([]SortCriterion, maxSortCriteria+1)
	for i := range tooMany {
		tooMany[i] = SortCriterion{Attribute: sortAttributeSize}
	}
	if err := valid(tooMany); err == nil {
		t.Fatal("more than the maximum sort criteria accepted")
	}

	// Validation normalizes the stored spelling for the ranking path.
	cfg := QualityConfig{EnableProfiles: true, Profiles: []QualityProfile{{Label: "p", Sort: []SortCriterion{{Attribute: " SIZE ", Direction: " DESC "}}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid mixed-case sort rejected: %v", err)
	}
	if got := cfg.Profiles[0].Sort[0]; got.Attribute != sortAttributeSize || got.Direction != sortDirectionDesc {
		t.Fatalf("sort not normalized: %+v", got)
	}
}

// --- JSON round-trip --------------------------------------------------------

func TestQualityProfileSortJSONRoundTrip(t *testing.T) {
	original := QualityProfile{
		Label: "custom",
		Sort: []SortCriterion{
			{Attribute: sortAttributeSize, Direction: sortDirectionDesc},
			{Attribute: sortAttributeHDR},
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"sort"`) {
		t.Fatalf("sort not serialized: %s", data)
	}
	var decoded QualityProfile
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sort) != 2 || decoded.Sort[0] != original.Sort[0] || decoded.Sort[1] != original.Sort[1] {
		t.Fatalf("sort did not round-trip: %+v", decoded.Sort)
	}

	// A profile stored before sort existed decodes with no criteria.
	var legacy QualityProfile
	if err := json.Unmarshal([]byte(`{"label":"old","resolution":"1080p"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Sort != nil {
		t.Fatalf("absent sort decoded as %+v, want nil", legacy.Sort)
	}
}

// --- back-compat ------------------------------------------------------------

// referencePreChangeTail is the ranking tail exactly as it existed before
// profiles could declare sort criteria. It is the oracle the empty-sort default
// must reproduce.
func referenceSortCandidatesForProfile(candidates []stream.StreamCandidate, p QualityProfile, formats []CustomFormat) {
	if len(candidates) == 0 {
		return
	}
	if len(candidates) == 1 {
		candidates[0].QualityScore, candidates[0].CustomFormatRejected = customFormatScore(candidates[0], formats)
		return
	}
	type scoredCandidate struct {
		candidate      stream.StreamCandidate
		rejected       bool
		profileMatched bool
	}
	profileActive := strings.TrimSpace(p.Label) != ""
	scored := make([]scoredCandidate, len(candidates))
	for idx := range candidates {
		score, reject := customFormatScore(candidates[idx], formats)
		candidates[idx].QualityScore = score
		candidates[idx].CustomFormatRejected = reject
		scored[idx] = scoredCandidate{
			candidate:      candidates[idx],
			rejected:       reject,
			profileMatched: !profileActive || MatchProfile(candidates[idx], p),
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		c1, c2 := scored[i].candidate, scored[j].candidate
		if scored[i].rejected != scored[j].rejected {
			return !scored[i].rejected
		}
		if profileActive && scored[i].profileMatched != scored[j].profileMatched {
			return scored[i].profileMatched
		}
		if c1.SourceConfirmed != c2.SourceConfirmed {
			return c1.SourceConfirmed
		}
		if c1.QualityScore != c2.QualityScore {
			return c1.QualityScore > c2.QualityScore
		}
		if p.Language != "" {
			r1 := stream.CandidateLanguageMatchRank(c1, p.Language)
			r2 := stream.CandidateLanguageMatchRank(c2, p.Language)
			if r1 >= 0 && r2 < 0 {
				return true
			}
			if r2 >= 0 && r1 < 0 {
				return false
			}
			if r1 >= 0 && r2 >= 0 && r1 != r2 {
				return r1 < r2
			}
		}
		if r1, r2 := stream.ResolutionScore(c1.Resolution), stream.ResolutionScore(c2.Resolution); r1 != r2 {
			return r1 > r2
		}
		if s1, s2 := stream.SourceScore(c1.SourceType), stream.SourceScore(c2.SourceType); s1 != s2 {
			return s1 > s2
		}
		if c1.AudioChannels != c2.AudioChannels {
			if ch1, ch2 := stream.AudioChannelsScore(c1.AudioChannels), stream.AudioChannelsScore(c2.AudioChannels); ch1 != ch2 {
				return ch1 > ch2
			}
		}
		if c1.FileSize != c2.FileSize {
			return c1.FileSize > c2.FileSize
		}
		return c1.OriginalIndex < c2.OriginalIndex
	})
	for idx := range scored {
		candidates[idx] = scored[idx].candidate
	}
}

// backCompatFixture exercises every default key with a distinct OriginalIndex
// so the reference comparator and the new default tail are comparable.
func backCompatFixture() ([]stream.StreamCandidate, QualityProfile, []CustomFormat) {
	profile := QualityProfile{Label: "pt", Language: "pt-BR"}
	high := CustomFormat{Name: "High", Pattern: "High", PatternType: "token", Score: 100, Enabled: true}
	same := func(name string, index int) stream.StreamCandidate {
		return stream.StreamCandidate{
			Name: name, OriginalIndex: index, SourceConfirmed: true,
			AudioLanguages: []string{"pt-BR"}, Resolution: "1080p",
			SourceType: "web-dl", AudioChannels: "5.1", FileSize: 1000,
		}
	}
	candidates := []stream.StreamCandidate{
		same("1080p.web-dl.5.1", 1),
		same("1080p.web-dl.5.1.small", 2),
		same("1080p.web-dl.2.0", 3),
		same("1080p.hdtv.5.1", 4),
		same("720p.web-dl.5.1", 5),
		same("8760p.web-dl.5.1", 6),
	}
	candidates[1].FileSize = 500
	candidates[2].AudioChannels = "2.0"
	candidates[3].SourceType = "hdtv"
	candidates[4].Resolution = "720p"
	candidates[5].Resolution = "" // unknown resolution
	candidates[5].Name = "unknown-res"
	// Confirmed first is exercised by an unconfirmed candidate last, and score by
	// a high-scoring candidate whose name contains the format token.
	unconfirmed := same("unconfirmed.1080p.web-dl.5.1", 7)
	unconfirmed.SourceConfirmed = false
	candidates = append(candidates, unconfirmed)
	highScore := same("High.1080p.web-dl.5.1", 0)
	candidates = append(candidates, highScore)
	// A non-matching language distinguishes the language key.
	wrongLang := same("wrong-lang.1080p.web-dl.5.1", 8)
	wrongLang.AudioLanguages = []string{"eng"}
	candidates = append(candidates, wrongLang)
	return candidates, profile, []CustomFormat{high}
}

// TestDefaultSortMatchesPreChangeImplementation is the back-compat oracle: an
// empty profile.Sort must order a candidate set identically to the ranking tail
// that predated sort criteria.
func TestDefaultSortMatchesPreChangeImplementation(t *testing.T) {
	candidates, profile, formats := backCompatFixture()

	got := make([]stream.StreamCandidate, len(candidates))
	copy(got, candidates)
	SortCandidatesForProfile(got, profile, formats)

	want := make([]stream.StreamCandidate, len(candidates))
	copy(want, candidates)
	referenceSortCandidatesForProfile(want, profile, formats)

	if len(got) != len(want) {
		t.Fatalf("length changed: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Name != want[i].Name {
			t.Fatalf("default sort order differs at %d: got %q want %q\n got: %v\nwant: %v",
				i, got[i].Name, want[i].Name, names(got), names(want))
		}
	}
}

// TestDefaultSortHasExpectedOrder pins the concrete order the fixture must
// produce, so a future edit to the default criteria cannot silently change it.
func TestDefaultSortHasExpectedOrder(t *testing.T) {
	candidates, profile, formats := backCompatFixture()
	SortCandidatesForProfile(candidates, profile, formats)

	want := []string{
		"High.1080p.web-dl.5.1",        // confirmed, highest custom-format score
		"1080p.web-dl.5.1",             // confirmed, 1080p, web-dl, 5.1, largest
		"1080p.web-dl.5.1.small",       // confirmed, smaller size
		"1080p.web-dl.2.0",             // confirmed, fewer channels
		"1080p.hdtv.5.1",               // confirmed, weaker source
		"720p.web-dl.5.1",              // confirmed, lower resolution
		"unknown-res",                  // confirmed, unknown resolution
		"unconfirmed.1080p.web-dl.5.1", // profile-matching, so before the non-matching tail
		"wrong-lang.1080p.web-dl.5.1",  // confirmed but does not satisfy the profile language
	}
	if got := names(candidates); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("default order = %v, want %v", got, want)
	}
}

func names(candidates []stream.StreamCandidate) []string {
	out := make([]string, len(candidates))
	for i := range candidates {
		out[i] = candidates[i].Name
	}
	return out
}

// TestSortOrdersLanguagePrecedenceExactBareVariantMulti proves the comparator
// honors the full rank chain end to end: under a fr-CA preference the exact
// candidate beats bare French, which beats the fr-BE variant, which beats a
// MULTi-flagged release with no French evidence, which beats an unrelated
// language. All other keys are tied so language alone decides.
func TestSortOrdersLanguagePrecedenceExactBareVariantMulti(t *testing.T) {
	profile := QualityProfile{Label: "fr", Language: "fr-CA"}
	make := func(name string, langs []string, multi bool, index int) stream.StreamCandidate {
		return stream.StreamCandidate{
			Name: name, OriginalIndex: index, SourceConfirmed: true,
			AudioLanguages: langs, IsMultiAudio: multi,
			Resolution: "1080p", SourceType: "web-dl", AudioChannels: "5.1", FileSize: 1000,
		}
	}
	candidates := []stream.StreamCandidate{
		make("wrong-lang", []string{"eng"}, false, 0),
		make("multi-flag", nil, true, 1),
		make("variant", []string{"FR-BE"}, false, 2),
		make("bare", []string{"FRA"}, false, 3),
		make("exact", []string{"FR-CA"}, false, 4),
	}
	SortCandidatesForProfile(candidates, profile, nil)
	want := []string{"exact", "bare", "variant", "multi-flag", "wrong-lang"}
	if got := names(candidates); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("language order = %v, want %v", got, want)
	}
}

// TestSortOrdersMultilingualMemberByLanguageNotFlag proves MULTi membership
// ranks by the carried language, not the flag fallback: a MULTi-flagged
// candidate whose list contains bare French sorts with the French cohort
// (ahead of a variant), while a flag-only MULTi release sorts behind it.
func TestSortOrdersMultilingualMemberByLanguageNotFlag(t *testing.T) {
	profile := QualityProfile{Label: "fr", Language: "fr"}
	make := func(name string, langs []string, multi bool, index int) stream.StreamCandidate {
		return stream.StreamCandidate{
			Name: name, OriginalIndex: index, SourceConfirmed: true,
			AudioLanguages: langs, IsMultiAudio: multi,
			Resolution: "1080p", SourceType: "web-dl", AudioChannels: "5.1", FileSize: 1000,
		}
	}
	candidates := []stream.StreamCandidate{
		make("multi-flag-only", nil, true, 0),
		make("variant", []string{"FR-CA"}, false, 1),
		make("multi-member", []string{"ENG", "FRE"}, true, 2),
	}
	SortCandidatesForProfile(candidates, profile, nil)
	want := []string{"multi-member", "variant", "multi-flag-only"}
	if got := names(candidates); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("member order = %v, want %v", got, want)
	}
}

// TestSortEndToEndFromReleaseNames proves the connected parser-to-ranking
// path: raw Stremio-style release names go through ParseStreamDetails, and
// the sort orders the parsed candidates by the profile language. An exact
// PT-BR release beats the bare-Portuguese one, which beats the PT-PT
// variant, with all other keys tied so language alone decides.
func TestSortEndToEndFromReleaseNames(t *testing.T) {
	profile := QualityProfile{Label: "pt", Language: "pt-BR"}
	releaseNames := []string{
		"Show.S01E01.1080p.WEB-DL.PT-PT.H.264-GROUP",
		"Show.S01E01.1080p.WEB-DL.POR.H.264-GROUP",
		"Show.S01E01.1080p.WEB-DL.PT-BR.H.264-GROUP",
	}
	candidates := make([]stream.StreamCandidate, len(releaseNames))
	for i, name := range releaseNames {
		candidates[i] = stream.StreamCandidate{Name: name, OriginalIndex: i}
		stream.ParseStreamDetails(&candidates[i])
	}
	for i, c := range candidates {
		if len(c.AudioLanguages) != 1 {
			t.Fatalf("candidate %d (%q) languages = %v, want exactly one regional/bare span", i, releaseNames[i], c.AudioLanguages)
		}
	}
	SortCandidatesForProfile(candidates, profile, nil)
	want := []string{
		"Show.S01E01.1080p.WEB-DL.PT-BR.H.264-GROUP",
		"Show.S01E01.1080p.WEB-DL.POR.H.264-GROUP",
		"Show.S01E01.1080p.WEB-DL.PT-PT.H.264-GROUP",
	}
	if got := names(candidates); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("parsed order = %v, want %v", got, want)
	}
}
