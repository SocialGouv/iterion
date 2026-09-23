package bundle

import "testing"

const syntaxProbeBot = "tool t:\n  command: \"true\"\n  output: out\n\nschema out:\n  ok: bool\n\nworkflow w:\n  entry: t\n  t -> done\n"

// The syntax floor of a bundle whose main is handed over as text reads that
// text — not a main.bot on disk, absent or stale — while the disk reader
// keeps reading the disk.
func TestMaxSyntaxRequirementsDirWithMainReadsTheMainHandedOver(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"manifest.yaml": "schema_version: 1\nname: probe\n"})
	if got := MaxSyntaxRequirementsDirWithMain(dir, MainBotFile, "dsl: 2\n\n"+syntaxProbeBot).Profile; got != 2 {
		t.Fatalf("profile %d, want 2 read off the text handed over (no main.bot on disk)", got)
	}
	writeTree(t, dir, map[string]string{"main.bot": "dsl: 2\n\n" + syntaxProbeBot})
	if got := MaxSyntaxRequirementsDirWithMain(dir, MainBotFile, syntaxProbeBot).Profile; got >= 2 {
		t.Fatalf("profile %d read off the stale main.bot on disk, want the text's", got)
	}
	if got := MaxSyntaxRequirementsDir(dir).Profile; got != 2 {
		t.Fatalf("the disk reader no longer reads the disk: profile %d, want 2", got)
	}
}
