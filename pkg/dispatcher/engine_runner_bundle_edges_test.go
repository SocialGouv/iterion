package dispatcher

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// skillsOnlyBundle writes a bundle marked by skills/ ALONE — no manifest,
// so the opened Bundle has a nil Manifest — whose main.bot references a
// prompt living only in prompts/, so a bare compile of it fails C003 and a
// green runner is the promotion itself.
func skillsOnlyBundle(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sk")
	for _, sub := range []string{"skills", "prompts"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "mission.md"), []byte("Do the thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "schema out:\n  ok: bool\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEngineRunnerPromotedMainBotWithoutManifestHasNoName: a promoted
// main.bot marked by skills/ alone carries a bundle with a nil Manifest.
// bundleName() — read on EVERY dispatch for the run's BotID — is "" there,
// never a nil dereference that takes the dispatcher daemon down.
func TestEngineRunnerPromotedMainBotWithoutManifestHasNoName(t *testing.T) {
	dir := skillsOnlyBundle(t)
	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	r, err := NewEngineRunner(filepath.Join(dir, "main.bot"), logger)
	if err != nil {
		t.Fatalf("NewEngineRunner on the bare main.bot: %v", err)
	}
	defer r.Close()
	if r.bundle == nil {
		t.Fatal("the bare main.bot was not promoted to its skills-only bundle")
	}
	if r.bundle.Manifest != nil {
		t.Fatal("the fixture carries a manifest; the nil case is not exercised")
	}
	if got := r.bundleName(); got != "" {
		t.Fatalf("bundleName() = %q, want \"\" for a bundle without a manifest", got)
	}
}

// TestEngineRunnerRefusesABundleThatDoesNotOpen: a main.bot beside a
// manifest that does not decode is a bundle by pkg/bundle's markers; the
// dispatcher refuses it at construction, naming the manifest, instead of
// dispatching a bare compile without its prompts and skills.
func TestEngineRunnerRefusesABundleThatDoesNotOpen(t *testing.T) {
	dir := skillsOnlyBundle(t)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.OpenDir(dir); err == nil {
		t.Fatal("the fixture manifest still opens; the case it guards is not exercised")
	}
	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	r, err := NewEngineRunner(filepath.Join(dir, "main.bot"), logger)
	if err == nil {
		r.Close()
		t.Fatal("NewEngineRunner accepted a main.bot whose bundle does not open")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("err = %v, want the manifest named", err)
	}
}
