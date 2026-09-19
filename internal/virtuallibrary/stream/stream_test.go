package stream

import "testing"

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

	dual := &StreamCandidate{Name: "Movie.2024.Dual.Audio.1080p.WEB-DL"}
	ParseStreamMetadata(dual)
	if !dual.IsMultiAudio || !dual.IsDualAudio {
		t.Fatal("a dual-audio release was not parsed as multi/dual audio")
	}
}
