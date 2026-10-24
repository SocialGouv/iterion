package bundle

import (
	"strconv"
	"strings"
	"testing"
)

func TestLoadManifestRejectsHandoffFromWithSurroundingWhitespace(t *testing.T) {
	for _, from := range []string{" teamonly-bot", "teamonly-bot ", "teamonly-bot\t", " "} {
		_, err := LoadManifest(writeManifestForTest(t, "name: consumer\nschema_version: 1\nconsumes:\n  - kind: review\n    var: prior_review\n    from: "+strconv.Quote(from)+"\n"))
		if err == nil || !strings.Contains(err.Error(), "must equal its trimmed form") {
			t.Errorf("from %q got %v, want a trimmed-form rejection instead of changing the exact Run.BotID lookup", from, err)
		}
	}
}

func TestLoadManifestAcceptsExactHandoffFrom(t *testing.T) {
	m, err := LoadManifest(writeManifestForTest(t, "name: consumer\nschema_version: 1\nconsumes:\n  - kind: review\n    var: prior_review\n    from: teamonly-bot\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Consumes) != 1 || m.Consumes[0].From != "teamonly-bot" {
		t.Fatalf("consumes = %+v, want the exact producer binding", m.Consumes)
	}
}
