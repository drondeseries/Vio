package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestStringSlicesEqualFoldNormalizesBeforeComparing pins G1: the language-list
// comparison must normalize (trim, case-fold, drop blanks) before it compares
// lengths, so a blank token can never shift an index and panic, and the
// comparison stays symmetric. The pre-fix helper compared raw lengths, stripped
// blanks during normalization, and then indexed the shorter normalized slice,
// panicking on ["en"] vs [""] and reporting an asymmetric false equality on
// [""] vs ["en"].
func TestStringSlicesEqualFoldNormalizesBeforeComparing(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"nil vs nil", nil, nil, true},
		{"nil vs empty", nil, []string{}, true},
		{"empty vs nil", []string{}, nil, true},
		{"empty vs empty", []string{}, []string{}, true},
		{"single vs blank", []string{"en"}, []string{""}, false},
		{"blank vs single", []string{""}, []string{"en"}, false},
		{"blank vs blank", []string{""}, []string{""}, true},
		{"blank vs whitespace", []string{" "}, []string{"\t\n"}, true},
		{"equal", []string{"en"}, []string{"en"}, true},
		{"case folded", []string{"EN"}, []string{"en"}, true},
		{"trimmed", []string{" en "}, []string{"en"}, true},
		{"reordered", []string{"en", "fr"}, []string{"fr", "en"}, true},
		{"reordered with blanks", []string{" en ", "", "fr"}, []string{"fr", "EN"}, true},
		{"duplicates matter", []string{"en", "en"}, []string{"en"}, false},
		{"duplicates equal", []string{"en", "en"}, []string{"en", "EN"}, true},
		{"different values", []string{"en"}, []string{"fr"}, false},
		{"different count", []string{"en", "fr"}, []string{"en", "fr", "de"}, false},
		{"all blanks vs empty", []string{"", " "}, []string{}, true},
		{"all blanks vs nil", []string{"", " "}, nil, true},
		{"blank plus value", []string{"", "en"}, []string{"en"}, true},
		{"value plus blank", []string{"en", ""}, []string{"en"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("stringSlicesEqualFold(%q, %q) panicked: %v", tc.a, tc.b, r)
				}
			}()
			if got := stringSlicesEqualFold(tc.a, tc.b); got != tc.want {
				t.Fatalf("stringSlicesEqualFold(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Symmetry is part of the contract: swapping the operands must not
			// change the verdict (the pre-fix helper was asymmetric).
			if got := stringSlicesEqualFold(tc.b, tc.a); got != tc.want {
				t.Fatalf("stringSlicesEqualFold(%q, %q) = %v, want %v (asymmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestAudioLayoutsEqualBlanksDoNotPanic drives the public audio-layout drift
// check over the same blank/empty language-list shapes, proving the G1 bug is
// unreachable through the exported comparison and that blank-only language
// lists never count as a difference.
func TestAudioLayoutsEqualBlanksDoNotPanic(t *testing.T) {
	base := models.AudioTrack{Index: 1, Codec: "eac3", Language: "mul", Channels: 6}
	withLanguages := func(langs ...string) []models.AudioTrack {
		track := base
		track.Languages = langs
		return []models.AudioTrack{track}
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AudioLayoutsEqual panicked on blank language lists: %v", r)
		}
	}()
	if !AudioLayoutsEqual(withLanguages("en"), withLanguages("EN")) {
		t.Fatal("case-folded language lists are not equal")
	}
	if !AudioLayoutsEqual(withLanguages(" en "), withLanguages("en")) {
		t.Fatal("trimmed language lists are not equal")
	}
	if AudioLayoutsEqual(withLanguages("en"), withLanguages("")) {
		t.Fatal("blank-only list equalled a populated list")
	}
	if !AudioLayoutsEqual(withLanguages("", "en"), withLanguages("en", " ")) {
		t.Fatal("blank tokens made equal normalized lists differ")
	}
}

// TestAudioLayoutsEqualReorderStable proves the exported audio-layout
// comparison keeps its order-independent language semantics after the G1 fix:
// reordered language lists on an otherwise identical track are equal, a changed
// language is not, and blank language lists compare stably.
func TestAudioLayoutsEqualReorderStable(t *testing.T) {
	track := func(langs []string) []models.AudioTrack {
		return []models.AudioTrack{{
			Index: 2, Codec: "eac3", Language: "mul", Channels: 6, Languages: langs,
		}}
	}
	if !AudioLayoutsEqual(track([]string{"en", "fr"}), track([]string{"fr", "en"})) {
		t.Fatal("reordered language lists are not equal")
	}
	if AudioLayoutsEqual(track([]string{"en", "fr"}), track([]string{"en", "de"})) {
		t.Fatal("changed language list compared equal")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AudioLayoutsEqual panicked on blank language list: %v", r)
		}
	}()
	if AudioLayoutsEqual(track([]string{"en"}), track([]string{""})) {
		t.Fatal("blank language list compared equal to a populated one")
	}
	if !AudioLayoutsEqual(track([]string{"EN", ""}), track([]string{"", "en"})) {
		t.Fatal("normalized-equal language lists compared unequal")
	}
}
