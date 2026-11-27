package catalogclaims

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// Mutation: restore either stale catalogue sentence (two-phase judge or a
// lang_scan router edit) and both generated copies redden with the manifest.
func TestDeepsec239DocsAndCatalogsUseCurrentRuntime(t *testing.T) {
	root := sweepwalk.ModuleRoot(t)
	paths := []string{
		"bots/sec-audit-source/manifest.yaml",
		"bots/sec-audit-source/skills/external-security-tools.md",
		"bots/whats-next/skills/iterion-bot-catalog.md",
		"bots/issue-triage/skills/iterion-bot-catalog.md",
		"docs/security-bots.md",
		"docs/references-bootstrap.md",
	}
	contents := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Generated catalogues and YAML wrap prose; compare text, not line breaks.
		contents[path] = strings.Join(strings.Fields(string(raw)), " ")
		for _, stale := range []string{
			"two-phase judge (anti-façade)",
			"entry in the `lang_scan` router",
			"node $ITERION_REFERENCES_ROOT/deepsec/cli.mjs",
			"WARN once per run",
			"recognizes the export by the realpath",
			"selects deepsec by the realpath",
		} {
			if strings.Contains(contents[path], stale) {
				t.Errorf("%s still carries stale contract %q", path, stale)
			}
		}
	}
	for _, path := range []string{paths[0], paths[2], paths[3]} {
		for _, want := range []string{"three independent disprove voters", "`iterion:scanners` block", "deepsec CLI on PATH plus Node 22+", "OwnerSummary object", "Finding / Recommendation"} {
			if !strings.Contains(contents[path], want) {
				t.Errorf("%s does not carry %q", path, want)
			}
		}
	}
	for _, want := range []string{"never executes it as a fallback", "`deepsec` on `PATH`", "Node 22+", "scanner_output.errors[]", "inode identity", "OwnerSummary", "{name, slug}", "` (lines ", "## Owners", "exporter supplied teams", "exporter-derived", "oncall exists", "owner data exists"} {
		if !strings.Contains(contents[paths[1]], want) {
			t.Errorf("external-security-tools does not carry %q", want)
		}
	}
	for _, want := range []string{"Three-voter majority", "skill-driven language coverage", "`deepsec` CLI on `PATH` and Node 22+", "inode identity", "OwnerSummary", "{name, slug}", "` (lines ", "## Owners", "exporter supplied teams", "exporter-derived", "oncall exists", "owner data exists"} {
		if !strings.Contains(contents[paths[4]], want) {
			t.Errorf("security-bots does not carry %q", want)
		}
	}
	for _, want := range []string{"runtime executable is the installed CLI", "command -v deepsec", "never executes `cli.mjs`, `dist/cli.mjs`", "rebuilding the sec sandbox image", "only makes the diagnostic more precise"} {
		if !strings.Contains(contents[paths[5]], want) {
			t.Errorf("references-bootstrap does not carry %q", want)
		}
	}
}
