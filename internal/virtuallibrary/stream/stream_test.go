package stream

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCanonicalLanguageBaseFoldsAliases pins the de-duplication key: a
// regional/script code, its bare ISO base, the bibliographic 3-letter variant
// and the English display name all fold onto one base language.
func TestCanonicalLanguageBaseFoldsAliases(t *testing.T) {
	cases := map[string]string{
		"EN-US":   "en",
		"en-GB":   "en",
		"ENG":     "en",
		"en":      "en",
		"English": "en",
		"ES-419":  "es",
		"SPA":     "es",
		"es":      "es",
		"FR-CA":   "fr",
		"FRE":     "fr",
		"fra":     "fr",
		"ZH-HANS": "zh",
		"ZHO":     "zh",
		"":        "",
	}
	for token, want := range cases {
		if got := CanonicalLanguageBase(token); got != want {
			t.Errorf("CanonicalLanguageBase(%q) = %q, want %q", token, got, want)
		}
	}
}

// TestDedupeLanguageAliasesPrefersTheMoreSpecificCode proves the alias collapse
// keeps one entry per base language, prefers a regional code over its bare
// base, and preserves the first position deterministically.
func TestDedupeLanguageAliasesPrefersTheMoreSpecificCode(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []string
		want []string
	}{
		"regional wins over base": {[]string{"ENG", "EN-US"}, []string{"EN-US"}},
		"base kept when alone":    {[]string{"ENG", "ENG"}, []string{"ENG"}},
		"distinct bases kept":     {[]string{"EN-US", "DEU"}, []string{"EN-US", "DEU"}},
		"first regional wins":     {[]string{"EN-GB", "EN-US"}, []string{"EN-GB"}},
		"empty dropped":           {[]string{"", "  ", "ENG"}, []string{"ENG"}},
		"nothing left":            {[]string{"", " "}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := DedupeLanguageAliases(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("DedupeLanguageAliases(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("DedupeLanguageAliases(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestParseStreamMetadataSubtitleRegionalAliasesDeduplicate is the regression
// for the duplicate subtitle list: "…en-US.srt" matched both the regional and
// base subtitle patterns and emitted "EN-US" and "ENG" as two languages.
func TestParseStreamMetadataSubtitleRegionalAliasesDeduplicate(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"Movie.2024.1080p.WEB-DL.EN-US.srt", "EN-US"},
		{"Movie.2024.1080p.WEB-DL.ES-419.srt", "ES-419"},
		{"Movie.2024.1080p.WEB-DL.FR-CA.srt", "FR-CA"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			candidate := &StreamCandidate{Name: tc.name}
			ParseStreamMetadata(candidate)
			if len(candidate.SubtitleLanguages) != 1 || candidate.SubtitleLanguages[0] != tc.want {
				t.Fatalf("SubtitleLanguages = %v, want [%s]", candidate.SubtitleLanguages, tc.want)
			}
		})
	}
}

// TestParseStreamMetadataSubtitleDistinctBasesRemain proves genuinely different
// base languages are not collapsed into one.
func TestParseStreamMetadataSubtitleDistinctBasesRemain(t *testing.T) {
	candidate := &StreamCandidate{Name: "Movie.2024.1080p.WEB-DL.EN-US.DEU.srt"}
	ParseStreamMetadata(candidate)
	if len(candidate.SubtitleLanguages) != 2 {
		t.Fatalf("SubtitleLanguages = %v, want two distinct base languages", candidate.SubtitleLanguages)
	}
	seen := map[string]bool{}
	for _, language := range candidate.SubtitleLanguages {
		seen[language] = true
	}
	if !seen["EN-US"] || !seen["DEU"] {
		t.Fatalf("SubtitleLanguages = %v, want EN-US and DEU", candidate.SubtitleLanguages)
	}
}

// TestParseStreamMetadataSubtitleBaseOnlyUnchanged proves an input that only
// carries a bare base code still yields exactly that one entry.
func TestParseStreamMetadataSubtitleBaseOnlyUnchanged(t *testing.T) {
	candidate := &StreamCandidate{Name: "Movie.2024.1080p.WEB-DL.ENG.srt"}
	ParseStreamMetadata(candidate)
	if len(candidate.SubtitleLanguages) != 1 || candidate.SubtitleLanguages[0] != "ENG" {
		t.Fatalf("SubtitleLanguages = %v, want [ENG]", candidate.SubtitleLanguages)
	}
}

// A release marker is not a language: "MULTI" in a release name must not be
// declared as an audio language on the candidate. The server's virtual-track
// merge filters it with the ISO language tagger, but the declared list should
// already be clean so Jellyfin-protocol clients and rankers see real
// languages only.
func TestParseStreamMetadataAudioLanguagesExcludeReleaseMarkers(t *testing.T) {
	s := &StreamCandidate{
		Name:        "Movie.2023.2160p.MULTI.TRUEHD.Atmos.WebLD-RGB",
		Description: "Multi audio release",
		Title:       "Movie 2023 MULTI eng fre 2160p",
	}
	ParseStreamMetadata(s)

	got := map[string]bool{}
	for _, lang := range s.AudioLanguages {
		got[lang] = true
	}
	if got["MULTI"] {
		t.Fatalf("release marker MULTI leaked into AudioLanguages: %v", s.AudioLanguages)
	}
	for _, lang := range s.AudioLanguages {
		switch lang {
		case "ENG", "FRE":
			// expected
		default:
			t.Errorf("unexpected language token %q in AudioLanguages: %v", lang, s.AudioLanguages)
		}
	}
	if len(s.AudioLanguages) == 0 {
		t.Fatal("expected at least the ENG/FRE tokens from the title, got none")
	}
}

func TestParseStreamMetadataAudioLanguagesDeduplicates(t *testing.T) {
	s := &StreamCandidate{
		Name:  "Movie.2160p.ENG.ENG.eng.Multi",
		Title: "Movie ENG FRE",
	}
	ParseStreamMetadata(s)

	counts := map[string]int{}
	for _, lang := range s.AudioLanguages {
		counts[lang]++
	}
	for lang, count := range counts {
		if count > 1 {
			t.Errorf("language %q appeared %d times, want once: %v", lang, count, s.AudioLanguages)
		}
	}
	if counts["ENG"] != 1 {
		t.Errorf("ENG appeared %d times, want 1: %v", counts["ENG"], s.AudioLanguages)
	}
}

func TestParseStreamDetailsRegionalLanguagesAndMulti(t *testing.T) {
	s := &StreamCandidate{
		Name: "Dune.Part.Two.2024.2160p.UHD.Remux.PT-BR.MULTI.TrueHD.Atmos.7.1-FLUX",
	}
	ParseStreamDetails(s)

	if !s.IsMultiAudio {
		t.Fatal("expected IsMultiAudio = true")
	}
	if s.AudioChannels != "7.1" {
		t.Fatalf("expected AudioChannels = 7.1, got %q", s.AudioChannels)
	}
	foundPTBR := false
	for _, l := range s.AudioLanguages {
		if l == "PT-BR" {
			foundPTBR = true
		}
	}
	if !foundPTBR {
		t.Fatalf("expected PT-BR in AudioLanguages, got %v", s.AudioLanguages)
	}
	if s.CodecAudio != "truehd" || !s.HasAtmos {
		t.Fatalf("expected truehd with atmos, got %s (atmos=%v)", s.CodecAudio, s.HasAtmos)
	}
}

func TestParseStreamDetailsTitleStripping(t *testing.T) {
	s := &StreamCandidate{
		Name: "Cam.2018.1080p.NF.WEB-DL.DDP5.1.x264-NTb",
	}
	ParseStreamDetailsWithTitle(s, "Cam")
	if s.Resolution != "1080p" {
		t.Fatalf("expected 1080p, got %q", s.Resolution)
	}
	if s.SourceType != "web-dl" {
		t.Fatalf("expected web-dl, got %q", s.SourceType)
	}
}

func TestCandidateLanguageMatchRank(t *testing.T) {
	candExact := StreamCandidate{AudioLanguages: []string{"PT-BR"}}
	candBase := StreamCandidate{AudioLanguages: []string{"POR"}}
	candVariant := StreamCandidate{AudioLanguages: []string{"PT-PT"}}
	candMulti := StreamCandidate{IsMultiAudio: true}
	candOther := StreamCandidate{AudioLanguages: []string{"ENG"}}

	if rank := CandidateLanguageMatchRank(candExact, "pt-BR"); rank != 0 {
		t.Fatalf("exact match rank = %d, want 0", rank)
	}
	if rank := CandidateLanguageMatchRank(candBase, "pt-BR"); rank != 1 {
		t.Fatalf("base match rank = %d, want 1", rank)
	}
	if rank := CandidateLanguageMatchRank(candVariant, "pt-BR"); rank != 2 {
		t.Fatalf("variant match rank = %d, want 2", rank)
	}
	if rank := CandidateLanguageMatchRank(candMulti, "pt-BR"); rank != 3 {
		t.Fatalf("multi match rank = %d, want 3", rank)
	}
	if rank := CandidateLanguageMatchRank(candOther, "pt-BR"); rank != -1 {
		t.Fatalf("other match rank = %d, want -1", rank)
	}
}

func TestParseStreamDetailsReleaseGroupINDNotIndonesian(t *testing.T) {
	s := &StreamCandidate{
		Name: "Alien.Romulus.2024.1080p.WEB-DL.DDP5.1.Atmos.H.264-IND",
	}
	ParseStreamDetails(s)
	for _, l := range s.AudioLanguages {
		if l == "IND" || l == "ID" {
			t.Fatalf("release group -IND was falsely identified as Indonesian: %v", s.AudioLanguages)
		}
	}
}

// TestIsMultiAudioRequiresMultiPattern proves the name-substring fallback is
// gone: titles like "Multiplicity" and "The Multiverse" must not be advertised
// as multi-audio releases, while a real MULTI token still is.
func TestIsMultiAudioRequiresMultiPattern(t *testing.T) {
	for _, name := range []string{
		"The.Multiverse.2024.1080p.WEB-DL",
		"Multiplicity.1996.1080p.WEB-DL",
	} {
		candidate := &StreamCandidate{Name: name}
		ParseStreamMetadata(candidate)
		if candidate.IsMultiAudio {
			t.Fatalf("%q parsed as multi-audio through a name substring", name)
		}
	}

	multi := &StreamCandidate{Name: "Movie.2024.MULTI.1080p.WEB-DL"}
	ParseStreamMetadata(multi)
	if !multi.IsMultiAudio {
		t.Fatal("a real MULTI release was not parsed as multi-audio")
	}

	// MULTI SUBS advertises subtitles, never audio: it must not set the
	// audio flag, or single-audio releases pass MULTI-audio profiles.
	for _, name := range []string{
		"Movie.2024.1080p.WEB-DL.MULTI.SUBS",
		"Movie.2024.1080p.WEB-DL.MULTISUBS",
		"Movie.2024.1080p.WEB-DL.MULTI-SUB",
	} {
		subs := &StreamCandidate{Name: name}
		ParseStreamMetadata(subs)
		if subs.IsMultiAudio {
			t.Fatalf("%q set IsMultiAudio for a subtitle marker", name)
		}
	}

	dual := &StreamCandidate{Name: "Movie.2024.Dual.Audio.1080p.WEB-DL"}
	ParseStreamMetadata(dual)
	if !dual.IsMultiAudio || !dual.IsDualAudio {
		t.Fatal("a dual-audio release was not parsed as multi/dual audio")
	}
}

// TestParseStreamMetadataRegionalMasking pins the regional-span masking: a
// regional code must not also emit its bare base (es-419 must not yield SPA
// from the "es" fragment, pt-BR must not yield POR), or the wrong regional
// variant gains bare-language rank and aliases inflate distinct counts.
func TestParseStreamMetadataRegionalMasking(t *testing.T) {
	es419 := &StreamCandidate{Name: "Show.S01E01.1080p.WEB-DL.DDP5.1.H.264-ES-419-GROUP"}
	ParseStreamMetadata(es419)
	if len(es419.AudioLanguages) != 1 || es419.AudioLanguages[0] != "ES-419" {
		t.Fatalf("es-419 languages = %v, want [ES-419] only", es419.AudioLanguages)
	}

	ptbr := &StreamCandidate{Name: "Movie.2024.1080p.WEB-DL.DDP5.1-PT-BR-GROUP"}
	ParseStreamMetadata(ptbr)
	if len(ptbr.AudioLanguages) != 1 || ptbr.AudioLanguages[0] != "PT-BR" {
		t.Fatalf("pt-BR languages = %v, want [PT-BR] only", ptbr.AudioLanguages)
	}

	zh := &StreamCandidate{Name: "Movie.2024.1080p.WEB-DL.ZH-HANS-GROUP"}
	ParseStreamMetadata(zh)
	if len(zh.AudioLanguages) != 1 || zh.AudioLanguages[0] != "ZH-HANS" {
		t.Fatalf("zh-hans languages = %v, want [ZH-HANS] only", zh.AudioLanguages)
	}
}

// TestCandidateHasDistinctAudioLanguages pins the alias-proof MULTI gate: a
// regional code beside its bare base counts once, while two true languages
// count twice.
func TestCandidateHasDistinctAudioLanguages(t *testing.T) {
	aliased := StreamCandidate{AudioLanguages: []string{"ES-419", "SPA"}}
	if CandidateHasDistinctAudioLanguages(aliased, 2) {
		t.Fatalf("aliased %v counted as 2 distinct languages", aliased.AudioLanguages)
	}
	if !CandidateHasDistinctAudioLanguages(aliased, 1) {
		t.Fatalf("aliased %v counted as 0 languages", aliased.AudioLanguages)
	}
	multi := StreamCandidate{AudioLanguages: []string{"ENG", "SPA"}}
	if !CandidateHasDistinctAudioLanguages(multi, 2) {
		t.Fatalf("distinct %v not counted as 2 languages", multi.AudioLanguages)
	}
	single := StreamCandidate{AudioLanguages: []string{"ENG"}}
	if CandidateHasDistinctAudioLanguages(single, 2) {
		t.Fatalf("single %v counted as 2 languages", single.AudioLanguages)
	}
}

// TestParseURLExpirationPreservesPastExpiry pins the absent/invalid/expired
// distinction: an expired-at-ingestion signed URL must parse to its past
// expiry (so the stored row reads expired and refreshes), never to zero
// (which downstream treats as no-expiry, i.e. usable forever). Future,
// absent, and malformed params keep their prior meaning.
func TestParseURLExpirationPreservesPastExpiry(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(3 * time.Hour).Unix()

	expired := &StreamCandidate{URL: "https://cdn.example/stream?expires=" + strconv.FormatInt(past, 10)}
	ParseStreamMetadata(expired)
	if expired.ExpiresAt.IsZero() {
		t.Fatal("expired-at-ingestion URL parsed to zero expiry, want its past expiry")
	}
	if !expired.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expired URL expiry = %v, want a past time", expired.ExpiresAt)
	}

	fresh := &StreamCandidate{URL: "https://cdn.example/stream?expires=" + strconv.FormatInt(future, 10)}
	ParseStreamMetadata(fresh)
	if fresh.ExpiresAt.IsZero() || !fresh.ExpiresAt.After(time.Now()) {
		t.Fatalf("future URL expiry = %v, want a future time", fresh.ExpiresAt)
	}

	plain := &StreamCandidate{URL: "https://cdn.example/stream/token=abc"}
	ParseStreamMetadata(plain)
	if !plain.ExpiresAt.IsZero() {
		t.Fatalf("expiry-less URL expiry = %v, want zero (absent)", plain.ExpiresAt)
	}

	amzPast := &StreamCandidate{URL: "https://s3.example/key?X-Amz-Expires=3600&X-Amz-Date=" + time.Now().Add(-2*time.Hour).UTC().Format("20060102T150405Z")}
	ParseStreamMetadata(amzPast)
	if amzPast.ExpiresAt.IsZero() {
		t.Fatal("lapsed SigV4 URL parsed to zero expiry, want its past expiry")
	}
	if !amzPast.ExpiresAt.Before(time.Now()) {
		t.Fatalf("lapsed SigV4 expiry = %v, want a past time", amzPast.ExpiresAt)
	}
	if !strings.Contains(amzPast.URL, "X-Amz-Expires") {
		t.Fatal("test setup lost the SigV4 param")
	}
}
