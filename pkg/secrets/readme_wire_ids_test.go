package secrets

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The setup prompt shown on four public surfaces names the provider wire ids
// a member may pass to `iterion remote api-keys create --provider`. The four
// copies stay identical through scripts/prompt-parity.sh, which diffs them
// against each other only — so this test pins the README copy (the
// reference) to the vocabulary ParseProvider actually accepts: a provider
// rename that misses the prose leaves an agent pasting a dead id, and this
// is the one choke point the parity guard cannot provide.
func TestReadmeWireIdsAreValidProviders(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	segment := string(raw)
	start := strings.Index(segment, "wire ids: `")
	if start < 0 {
		t.Fatal("README.md carries no `wire ids:` list — update this test together with the prompt copies")
	}
	segment = segment[start:]
	if end := strings.Index(segment, ")"); end >= 0 {
		segment = segment[:end]
	}
	ids := regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(segment, -1)
	if len(ids) < 5 {
		t.Fatalf("parsed %d wire ids from README.md, expected the full provider list", len(ids))
	}
	for _, m := range ids {
		if _, err := ParseProvider(m[1]); err != nil {
			t.Errorf("README.md names provider id %q, which ParseProvider refuses: %v", m[1], err)
		}
	}
}
