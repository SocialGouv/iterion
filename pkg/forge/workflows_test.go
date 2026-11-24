package forge

import (
	"strings"
	"testing"
)

// A workflow file name lands in a forge API path
// (/actions/workflows/{file}/dispatches): only a plain *.yml / *.yaml
// basename is one, everything that could address another resource is not.
func TestValidWorkflowFile(t *testing.T) {
	for _, name := range []string{"renovate.yml", "ci.yaml", "release-please.yml", "a_b.c.yml", "x.yaml.yml"} {
		if !ValidWorkflowFile(name) {
			t.Errorf("ValidWorkflowFile(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"", "renovate", "renovate.sh", ".yml", ".yaml", ".github.yml",
		"../renovate.yml", "dir/renovate.yml", "/renovate.yml", "renovate.yml/",
		"renovate%2F.yml", "renovate.yml?ref=main", "renovate.yml#x", " renovate.yml",
		"re novate.yml", "renovate.yml\n", "renovate\\x.yml", "rénovate.yml", "12345",
		strings.Repeat("a", 252) + ".yml",
	} {
		if ValidWorkflowFile(name) {
			t.Errorf("ValidWorkflowFile(%q) = true, want false", name)
		}
	}
}
