package settingscontract

import (
	"encoding/json"
	"strings"
	"testing"
)

// keyVersionSort is the profile-scope version-list preference. It is referenced
// here rather than imported from the generated bindings so this low-level
// package keeps its existing test convention of transparent key literals.
const keyVersionSort = "playback.version_sort"

// TestVersionSortValueSchema pins the attribute vocabulary and the shape rules
// the manifest enforces. The server never evaluates the list; it is a
// client-side preference over the watch payload's version list, so the accepted
// attributes have to be exactly the ones that payload exposes.
func TestVersionSortValueSchema(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatalf("loading manifest: %v", err)
	}
	def, ok := manifest.Lookup(keyVersionSort)
	if !ok {
		t.Fatal("playback.version_sort is not registered")
	}
	if !def.ValueSchema.Nullable {
		t.Fatal("playback.version_sort must be nullable so an unset profile inherits the ranking")
	}
	if string(def.DefaultValue) != "null" {
		t.Fatalf("default_value = %s, want null", def.DefaultValue)
	}
	if def.Category != "playback" {
		t.Fatalf("category = %q, want playback", def.Category)
	}

	sixteen := versionSortList(16)
	valid := map[string]string{
		"empty list":              `[]`,
		"null":                    `null`,
		"one criterion":           `[{"attribute":"size","direction":"desc"}]`,
		"several criteria":        `[{"attribute":"resolution","direction":"desc"},{"attribute":"bitrate","direction":"desc"},{"attribute":"hdr","direction":"asc"}]`,
		"all seven attributes":    `[{"attribute":"size","direction":"desc"},{"attribute":"bitrate","direction":"desc"},{"attribute":"resolution","direction":"desc"},{"attribute":"audio_channels","direction":"desc"},{"attribute":"bit_depth","direction":"desc"},{"attribute":"hdr","direction":"asc"},{"attribute":"score","direction":"desc"}]`,
		"exactly sixteen entries": sixteen,
	}
	for name, value := range valid {
		t.Run("accepts "+name, func(t *testing.T) {
			if err := def.ValueSchema.ValidateValue(json.RawMessage(value), objSchemas); err != nil {
				t.Fatalf("valid value %s rejected: %v", value, err)
			}
		})
	}

	seventeen := versionSortList(17)
	rejected := map[string]string{
		// The wider quality-profile vocabulary is deliberately not accepted:
		// these attributes are not on the watch payload the list sorts.
		"server-only source attribute":      `[{"attribute":"source","direction":"asc"}]`,
		"server-only confirmed attribute":   `[{"attribute":"confirmed","direction":"asc"}]`,
		"server-only language attribute":    `[{"attribute":"language","direction":"asc"}]`,
		"unknown attribute":                 `[{"attribute":"filesize","direction":"desc"}]`,
		"unknown direction":                 `[{"attribute":"size","direction":"sideways"}]`,
		"uppercase direction":               `[{"attribute":"size","direction":"DESC"}]`,
		"missing direction":                 `[{"attribute":"size"}]`,
		"missing attribute":                 `[{"direction":"desc"}]`,
		"empty criterion":                   `[{}]`,
		"extra criterion field":             `[{"attribute":"size","direction":"desc","extra":true}]`,
		"criterion is a string":             `["size"]`,
		"attribute is not a string":         `[{"attribute":7,"direction":"desc"}]`,
		"direction is not a string":         `[{"attribute":"size","direction":7}]`,
		"object instead of array":           `{"attribute":"size","direction":"desc"}`,
		"string instead of array":           `"size"`,
		"seventeen entries exceeds the cap": seventeen,
	}
	for name, value := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			if err := def.ValueSchema.ValidateValue(json.RawMessage(value), objSchemas); err == nil {
				t.Fatalf("invalid value %s was accepted", value)
			}
		})
	}
}

// versionSortList builds a JSON array of n identical criteria so the test can
// reach the maxItems boundary without hand-writing sixteen objects.
func versionSortList(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = `{"attribute":"score","direction":"desc"}`
	}
	return "[" + strings.Join(items, ",") + "]"
}
