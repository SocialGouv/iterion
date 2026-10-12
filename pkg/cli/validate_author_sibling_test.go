package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// The twin of a sibling entry is validated as its .bot is — in the bundle:
// the prompts/ it references resolve (no C003) and the manifest's floor is
// held against its own profile (C252) — never compiled bare beside them
// (#1367 review, MEDIUM 1).
func TestValidatePromotesASiblingTwinToItsBundle(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\n")
	writeFixture(t, dir, "main.bot", "workflow main_w:\n  entry: done\n")
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "prompts"), "mission.md", "Do the thing.\n")
	// Pinned model and backend: the compile refuses C018 on a
	// credential-less host, and the test measures the promotion.
	bot := "agent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n\nworkflow extend_w:\n  entry: worker\n  worker -> done\n"
	writeFixture(t, dir, "extend.bot", bot)
	doc := writeFixture(t, dir, "extend.bot.yaml", "dsl: 2\nnodes:\n  - agent: worker\n    backend: claude_code\n    model: anthropic/claude-opus-4-8\n    system: mission\nworkflow:\n  name: extend_w\n  entry: worker\n  edges:\n    - worker -> done\n")

	res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if err != nil || !res.Valid {
		t.Fatalf("the twin of a sibling entry did not validate in its bundle: %v\n%s", err, out)
	}
	if hasDiagCode(res, "C003") {
		t.Fatalf("the bundle's prompts/ were not in scope for the twin:\n%s", out)
	}
	if res.BundleName != "probe" || res.BotPath != filepath.Join(dir, "extend.bot") {
		t.Fatalf("bundle=%q bot=%q, want the sibling's bundle and .bot:\n%s", res.BundleName, res.BotPath, out)
	}
	if !hasDiagCode(res, "C252") {
		t.Fatalf("the twin's own profile was not held against the manifest (no C252):\n%s", out)
	}
}
