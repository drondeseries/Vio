package playback_test

import (
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

func TestSelectAudioTrack(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en", Codec: "aac", Default: true, Channels: 2},
		{Language: "ja", Codec: "aac", Channels: 2},
		{Language: "ja", Codec: "flac", Channels: 6, Layout: "5.1"},
		{Language: "de", Codec: "aac", Channels: 2},
	}

	tests := []struct {
		name           string
		preferredLang  string
		seriesPrefIdx  int
		seriesPrefLang string
		hasSeriesPref  bool
		want           int
	}{
		{
			name:          "series pref index+lang match",
			preferredLang: "en",
			seriesPrefIdx: 2, seriesPrefLang: "ja", hasSeriesPref: true,
			want: 2,
		},
		{
			name:          "series pref index out of bounds falls back to lang",
			preferredLang: "en",
			seriesPrefIdx: 99, seriesPrefLang: "ja", hasSeriesPref: true,
			want: 1,
		},
		{
			name:          "series pref index wrong lang falls back to lang match",
			preferredLang: "en",
			seriesPrefIdx: 0, seriesPrefLang: "ja", hasSeriesPref: true,
			want: 1,
		},
		{
			name:          "no series pref uses profile language",
			preferredLang: "de",
			hasSeriesPref: false,
			want:          3,
		},
		{
			name:          "no matching language uses default track",
			preferredLang: "fr",
			hasSeriesPref: false,
			want:          0,
		},
		{
			name:          "empty preferred lang uses default track",
			preferredLang: "",
			hasSeriesPref: false,
			want:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seriesPref *playback.AudioTrackPreference
			if tt.hasSeriesPref {
				seriesPref = &playback.AudioTrackPreference{
					AudioTrackIndex: tt.seriesPrefIdx,
					AudioLanguage:   tt.seriesPrefLang,
				}
			}
			got := playback.SelectAudioTrack(tracks, tt.preferredLang, seriesPref)
			if got != tt.want {
				t.Errorf("SelectAudioTrack() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSelectAudioTrack_NoTracks(t *testing.T) {
	got := playback.SelectAudioTrack(nil, "en", nil)
	if got != 0 {
		t.Errorf("SelectAudioTrack(nil) = %d, want 0", got)
	}
}

func TestSelectAudioTrack_PrefersExactRegionalTag(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en-GB"},
		{Language: "en"},
		{Language: "en-US"},
	}
	if got := playback.SelectAudioTrack(tracks, "en-US", nil); got != 2 {
		t.Fatalf("SelectAudioTrack(en-US) = %d, want exact en-US track 2", got)
	}
	if got := playback.SelectAudioTrack(tracks, "en-AU", nil); got != 1 {
		t.Fatalf("SelectAudioTrack(en-AU) = %d, want generic en track 1", got)
	}
}

func TestSelectAudioTrack_MULTiLanguageList(t *testing.T) {
	multi := []models.AudioTrack{
		{Language: "en", Languages: []string{"en", "fr", "de"}, Codec: "eac3", Channels: 6, Default: true},
		{Language: "ja", Codec: "aac", Channels: 2},
	}

	// Profile preference resolves inside the MULTi list even though the primary
	// code is a different language.
	if got := playback.SelectAudioTrack(multi, "fr", nil); got != 0 {
		t.Fatalf("profile fr = %d, want MULTi track 0", got)
	}

	// A single-language track still matches by its primary code.
	single := []models.AudioTrack{
		{Language: "ja", Codec: "aac", Channels: 2, Default: true},
		{Language: "fr", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(single, "fr", nil); got != 1 {
		t.Fatalf("single-language fr = %d, want 1", got)
	}

	// A preference no track (single or MULTi) carries falls back to the default.
	noDe := []models.AudioTrack{
		{Language: "en", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6, Default: true},
		{Language: "ja", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(noDe, "de", nil); got != 0 {
		t.Fatalf("unmatched de = %d, want default 0", got)
	}

	// Series preference index+language and language fallback honor the list.
	seriesIndex := &playback.AudioTrackPreference{AudioTrackIndex: 1, AudioLanguage: "fr"}
	if got := playback.SelectAudioTrack(multi, "", seriesIndex); got != 0 {
		t.Fatalf("series index 1 fr = %d, want MULTi track 0", got)
	}
	seriesFallback := &playback.AudioTrackPreference{AudioTrackIndex: 99, AudioLanguage: "fr"}
	if got := playback.SelectAudioTrack(multi, "", seriesFallback); got != 0 {
		t.Fatalf("series language fallback fr = %d, want MULTi track 0", got)
	}
}

func TestSelectAudioTrack_NoDefaultFallsToFirst(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "ja", Codec: "aac"},
		{Language: "en", Codec: "aac"},
	}
	got := playback.SelectAudioTrack(tracks, "fr", nil)
	if got != 0 {
		t.Errorf("SelectAudioTrack() = %d, want 0", got)
	}
}

// TestSelectAudioTrack_ISO639CrossFormat tests that 2-letter profile
// preferences match 3-letter FFmpeg track codes and vice versa.
func TestSelectAudioTrack_ISO639CrossFormat(t *testing.T) {
	// Real-world FFmpeg track languages use 3-letter ISO 639-2 codes.
	tracks := []models.AudioTrack{
		{Language: "spa", Codec: "aac", Default: true, Channels: 2},
		{Language: "eng", Codec: "aac", Channels: 6, Layout: "5.1"},
		{Language: "jpn", Codec: "flac", Channels: 2},
	}

	tests := []struct {
		name          string
		preferredLang string
		want          int
	}{
		{"2-letter en matches 3-letter eng", "en", 1},
		{"2-letter es matches 3-letter spa", "es", 0},
		{"2-letter ja matches 3-letter jpn", "ja", 2},
		{"3-letter eng matches 3-letter eng", "eng", 1},
		{"unmatched falls to default", "fr", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := playback.SelectAudioTrack(tracks, tt.preferredLang, nil)
			if got != tt.want {
				t.Errorf("SelectAudioTrack() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSelectAudioTrack_SeriesPrefCrossFormat(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "spa", Codec: "aac", Default: true, Channels: 2},
		{Language: "eng", Codec: "aac", Channels: 6},
		{Language: "jpn", Codec: "flac", Channels: 2},
	}

	// Series pref stored with 3-letter code (from track), profile uses 2-letter.
	pref := &playback.AudioTrackPreference{
		AudioTrackIndex: 2,
		AudioLanguage:   "jpn",
	}
	got := playback.SelectAudioTrack(tracks, "en", pref)
	if got != 2 {
		t.Errorf("series pref jpn at index 2: got %d, want 2", got)
	}

	// Series pref language fallback with 2-letter stored code.
	pref2 := &playback.AudioTrackPreference{
		AudioTrackIndex: 99,   // out of bounds
		AudioLanguage:   "ja", // 2-letter
	}
	got = playback.SelectAudioTrack(tracks, "en", pref2)
	if got != 2 {
		t.Errorf("series pref ja fallback: got %d, want 2", got)
	}
}

func TestSelectAudioTrack_SeriesPrefIndexKeepsRegionalVariant(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en", Codec: "aac", Channels: 2, Title: "Commentary"},
		{Language: "en-US", Codec: "eac3", Channels: 6, Title: "English 5.1", Default: true},
	}

	// Preference saved before regional subtags were preserved: bare "en" for
	// the track at index 1. The saved index must still win over the bare
	// "en" commentary track at index 0.
	pref := &playback.AudioTrackPreference{
		AudioTrackIndex: 1,
		AudioLanguage:   "en",
	}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 1 {
		t.Fatalf("SelectAudioTrack() = %d, want saved index 1", got)
	}
}

func TestSelectAudioTrack_SignaturePrefersExactRegionalTag(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en-GB", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "English 5.1"},
		{Language: "en-US", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "English 5.1"},
	}
	sig := func(language string) *userstore.AudioTrackSignature {
		return &userstore.AudioTrackSignature{Language: language, Title: "English 5.1", Codec: "eac3", Layout: "5.1", Channels: 6}
	}

	// A regional signature must not settle for the earlier variant.
	pref := &playback.AudioTrackPreference{AudioTrackIndex: 0, AudioLanguage: "en-US", TrackSignature: sig("en-US")}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 1 {
		t.Fatalf("en-US signature: SelectAudioTrack() = %d, want 1", got)
	}

	// A legacy bare-language signature still matches a regional track.
	pref = &playback.AudioTrackPreference{AudioTrackIndex: 0, AudioLanguage: "en", TrackSignature: sig("en")}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 0 {
		t.Fatalf("bare en signature: SelectAudioTrack() = %d, want 0", got)
	}
}

func TestSelectAudioTrack_PrefersExactTrackSignatureOverIndexFallback(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo", Title: "English Stereo", Default: true},
		{Language: "eng", Codec: "flac", Channels: 6, Layout: "5.1", Title: "English 5.1"},
	}

	pref := &playback.AudioTrackPreference{
		AudioTrackIndex: 0,
		AudioLanguage:   "en",
		TrackSignature: &userstore.AudioTrackSignature{
			Language: "eng",
			Title:    "English 5.1",
			Codec:    "flac",
			Layout:   "5.1",
			Channels: 6,
			Default:  false,
		},
	}

	got := playback.SelectAudioTrack(tracks, "en", pref)
	if got != 1 {
		t.Fatalf("SelectAudioTrack() = %d, want 1", got)
	}
}

func TestMatchAudioTrackAcrossVersionsRemapsReorderedLanguage(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "ja", Codec: "aac", Channels: 2, Title: "Japanese"},
		{Language: "en", Codec: "eac3", Channels: 6, Title: "English 5.1"},
	}
	effective := []models.AudioTrack{
		{Language: "en", Codec: "eac3", Channels: 6, Title: "English 5.1"},
		{Language: "ja", Codec: "aac", Channels: 2, Title: "Japanese"},
	}

	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 1); got != 0 {
		t.Fatalf("MatchAudioTrackAcrossVersions() = %d, want English track 0", got)
	}
}

func TestMatchAudioTrackAcrossVersionsFallsBackToLanguageAcrossCodecs(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "ja", Codec: "aac", Channels: 2},
		{Language: "en", Codec: "truehd", Channels: 8},
	}
	effective := []models.AudioTrack{
		{Language: "en", Codec: "eac3", Channels: 6},
		{Language: "es", Codec: "aac", Channels: 2, Default: true},
	}

	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 1); got != 0 {
		t.Fatalf("MatchAudioTrackAcrossVersions() = %d, want English track 0", got)
	}
}

func TestMatchAudioTrackAcrossVersions_MULTiCrossFile(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2},
		{Language: "ja", Codec: "aac", Channels: 2},
		{Language: "en", Languages: []string{"en", "fr", "de"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
	}
	target := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2},
		{Language: "en", Languages: []string{"fr", "de", "en"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
		{Language: "ja", Codec: "aac", Channels: 2, Default: true},
	}

	// Requested MULTi [en,fr,de] at index 2 resolves to the equivalent MULTi
	// track by signature despite the target listing the languages in a
	// different order and at a different ordinal.
	if got := playback.MatchAudioTrackAcrossVersions(requested, target, 2); got != 1 {
		t.Fatalf("MULTi remap = %d, want target MULTi track 1", got)
	}
	// A single-language requested track resolves by language at its own ordinal.
	if got := playback.MatchAudioTrackAcrossVersions(requested, target, 0); got != 0 {
		t.Fatalf("es remap = %d, want 0", got)
	}
	// A single-language requested track resolves at a different target ordinal.
	if got := playback.MatchAudioTrackAcrossVersions(requested, target, 1); got != 2 {
		t.Fatalf("ja remap = %d, want target track 2", got)
	}

	// A requested track absent from the target falls back to the target default.
	absent := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2},
		{Language: "it", Codec: "ac3", Channels: 2},
		{Language: "en", Languages: []string{"en", "fr", "de"}, Codec: "eac3", Channels: 6},
	}
	if got := playback.MatchAudioTrackAcrossVersions(absent, target, 1); got != 2 {
		t.Fatalf("absent-track remap = %d, want target default 2", got)
	}
}

func TestMatchAudioTrackAcrossVersionsTriesEveryLanguage(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "mul", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
	}
	effective := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "fr", Codec: "eac3", Channels: 6, Title: "French"},
	}

	// The requested MULTi track carries [en, fr]. "en" is absent from the
	// target but "fr" is present, so the remap must follow the later language
	// instead of degrading to the target default.
	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 1); got != 1 {
		t.Fatalf("second-language MULTi remap = %d, want target French track 1", got)
	}
}

func TestMatchAudioTrackAcrossVersionsTriesLanguageListAfterPrimary(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "en", Languages: []string{"en", "de"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
	}
	effective := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "de", Codec: "eac3", Channels: 6},
	}

	// A concrete primary language that is absent from the target must not stop
	// the match from considering the track's other declared languages.
	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 0); got != 1 {
		t.Fatalf("language-list fallback = %d, want target German track 1", got)
	}
}

func TestMatchAudioTrackAcrossVersionsFallsBackToDefaultWhenNoLanguageMatches(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "mul", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6},
	}
	effective := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "ja", Codec: "aac", Channels: 2},
	}

	// When no carried language exists on the target, the default track still
	// wins over the first track.
	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 0); got != 0 {
		t.Fatalf("no-language-match remap = %d, want target default 0", got)
	}
}

// playableByCodec builds the playability predicate a track selector receives,
// matching the codec only. The real predicate is AudioTrackPlayableFuncV3; this
// keeps the selector mechanics tests focused.
func playableByCodec(codecs ...string) func(models.AudioTrack) bool {
	return func(track models.AudioTrack) bool {
		for _, codec := range codecs {
			if strings.EqualFold(strings.TrimSpace(codec), strings.TrimSpace(track.Codec)) {
				return true
			}
		}
		return false
	}
}

// TestSelectAudioTrackPrefersPlayableSameLanguageTrack proves a default track
// the client cannot render yields to a renderable track of the same language, so
// a file with an AAC compatibility track does not force an AAC transcode.
func TestSelectAudioTrackPrefersPlayableSameLanguageTrack(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "truehd", Channels: 8, Default: true},
		{Language: "eng", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrackPreferringPlayable(tracks, "", nil, playableByCodec("aac", "mp3", "opus")); got != 1 {
		t.Fatalf("SelectAudioTrackPreferringPlayable() = %d, want the playable English AAC track 1", got)
	}
}

// TestSelectAudioTrackPrefersPlayableWithinPreferredLanguage proves the
// language preference still decides which track is chosen: the playable track
// of the preferred language wins over a non-playable one.
func TestSelectAudioTrackPrefersPlayableWithinPreferredLanguage(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "truehd", Channels: 8, Default: true},
		{Language: "eng", Codec: "aac", Channels: 2},
		{Language: "fra", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrackPreferringPlayable(tracks, "eng", nil, playableByCodec("aac")); got != 1 {
		t.Fatalf("SelectAudioTrackPreferringPlayable() = %d, want the English AAC track 1", got)
	}
}

// TestSelectAudioTrackDoesNotTradeLanguageForCodec proves playability never
// overrides the language intent: with only a foreign-language playable track,
// the non-playable preferred-language track is kept and the planner transforms
// its audio as before.
func TestSelectAudioTrackDoesNotTradeLanguageForCodec(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "truehd", Channels: 8, Default: true},
		{Language: "fra", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrackPreferringPlayable(tracks, "eng", nil, playableByCodec("aac", "mp3", "opus")); got != 0 {
		t.Fatalf("SelectAudioTrackPreferringPlayable() = %d, want the English track to keep language over codec", got)
	}
}

// TestSelectAudioTrackDoesNotTradeRoleForCodec proves a same-language
// replacement does not swap the track role: the selector must not abandon a
// commentary track for the main track just because only the latter is playable.
func TestSelectAudioTrackDoesNotTradeRoleForCodec(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "truehd", Channels: 8, Title: "Commentary", Default: true},
		{Language: "eng", Codec: "aac", Channels: 2, Title: "Main"},
	}
	if got := playback.SelectAudioTrackPreferringPlayable(tracks, "eng", nil, playableByCodec("aac")); got != 0 {
		t.Fatalf("SelectAudioTrackPreferringPlayable() = %d, want the commentary track to keep its role", got)
	}
}

// TestSelectAudioTrackSavedIndexDecidesAmongPlayableTracks proves the saved
// index still chooses which track is used when several playable candidates
// exist.
func TestSelectAudioTrackSavedIndexDecidesAmongPlayableTracks(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "aac", Channels: 2, Title: "Main"},
		{Language: "eng", Codec: "aac", Channels: 6, Layout: "5.1", Title: "Main"},
	}
	pref := &playback.AudioTrackPreference{AudioTrackIndex: 1, AudioLanguage: "eng"}
	if got := playback.SelectAudioTrackPreferringPlayable(tracks, "eng", pref, playableByCodec("aac")); got != 1 {
		t.Fatalf("SelectAudioTrackPreferringPlayable() = %d, want the saved index 1 among playable tracks", got)
	}
}

// TestSelectAudioTrackWithoutClientCapabilitiesKeepsHistoricalOrder proves
// callers without a client predicate (catalog metadata, cross-version remap)
// keep the pre-existing selection.
func TestSelectAudioTrackWithoutClientCapabilitiesKeepsHistoricalOrder(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "truehd", Channels: 8, Default: true},
		{Language: "eng", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracks, "", nil); got != 0 {
		t.Fatalf("SelectAudioTrack() = %d, want the default track when no playability predicate is given", got)
	}
}

// TestSelectAudioTrackAllNonPlayableKeepsDefault proves that when no track is
// playable the historical selection stands and the planner transcodes.
func TestSelectAudioTrackAllNonPlayableKeepsDefault(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "truehd", Channels: 8, Default: true},
		{Language: "eng", Codec: "dts", Channels: 6},
	}
	if got := playback.SelectAudioTrackPreferringPlayable(tracks, "", nil, playableByCodec("aac", "mp3", "opus")); got != 0 {
		t.Fatalf("SelectAudioTrackPreferringPlayable() = %d, want the default track when nothing is playable", got)
	}
}

// TestAudioTrackPlayableFuncV3GatesPassthroughOnExactEvidence proves the
// selector's predicate shares audioEligibilityV3's rule rather than trusting the
// passthrough codec list alone: a passthrough codec with declared (not exact)
// evidence or without a matching layout entry is not directly renderable.
func TestAudioTrackPlayableFuncV3GatesPassthroughOnExactEvidence(t *testing.T) {
	passthrough := &playback.AudioPassthroughV3{
		PassthroughCodecs: []string{"eac3"},
		Entries:           []playback.AudioPassthroughEntryV3{{Codec: "eac3", ChannelCounts: []int{6}, Layouts: []string{"5.1"}}},
	}
	base := playback.StartRequestV3{
		ClientFeatures: []string{playback.FeatureLayoutPassthrough},
		Capabilities: playback.ClientCodecCapabilitiesV3{
			CodecsAudio:      []string{"aac"},
			AudioEvidence:    playback.EvidenceDeclaredV3,
			AudioPassthrough: passthrough,
		},
	}
	eac3 := models.AudioTrack{Codec: "eac3", Channels: 6, Layout: "5.1"}
	if playback.AudioTrackPlayableFuncV3(base)(eac3) {
		t.Fatal("passthrough codec must not be playable without exact audio evidence")
	}

	base.Capabilities.AudioEvidence = playback.EvidenceExactV3
	if !playback.AudioTrackPlayableFuncV3(base)(eac3) {
		t.Fatal("exact-evidence passthrough codec with a matching layout entry must be playable")
	}

	unmatched := models.AudioTrack{Codec: "eac3", Channels: 2, Layout: "stereo"}
	if playback.AudioTrackPlayableFuncV3(base)(unmatched) {
		t.Fatal("passthrough codec without a matching channel/layout entry must not be playable")
	}

	aac := models.AudioTrack{Codec: "aac", Channels: 2}
	if !playback.AudioTrackPlayableFuncV3(base)(aac) {
		t.Fatal("declared decode codec must remain playable")
	}
}
