package handlers

import (
	"testing"

	"golang.org/x/text/language"
)

// CI reproducer: what does language.Parse return for the tokens in the
// failing fixtures? A CI Go-version difference in the x/text Unicode tables
// would flip anyProbed/dedup behavior between environments.
func TestLanguageParseDiagnostics(t *testing.T) {
	for _, value := range []string{"EN", "en-US", "FRA", "ENG", "it", "en"} {
		tag, err := language.Parse(value)
		if err != nil {
			t.Logf("%q: err=%v", value, err)
			continue
		}
		base, conf := tag.Base()
		t.Logf("%q: base=%q conf=%v (No=%v)", value, base.String(), conf, conf == language.No)
	}
}
