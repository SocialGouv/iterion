package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestRunValidate_SaysWhenAManifestBesideMainBotWasNotRead: `iterion
// validate bots/x/main.bot` beside a `manifest.yaml` that does NOT mark
// the bundle — a typo in its only distinctive key — validates the file
// alone and SAYS so (C223, naming the key): the one outcome that would
// otherwise be silent. Fixed, the manifest marks and the warning is gone.
func TestRunValidate_SaysWhenAManifestBesideMainBotWasNotRead(t *testing.T) {
	inTempWorkspace(t)
	dir := filepath.Join("bots", "typo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainBot := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(mainBot, []byte("workflow typo:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: typo\ndisplay_nmae: Typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bundle.DirForMainBot(mainBot); got != "" {
		t.Fatalf("the fixture manifest marks (%q); the case it guards is not exercised", got)
	}
	jp, out := jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C223") || !strings.Contains(s, "display_nmae") {
		t.Fatalf("validate said nothing about the manifest it did not read:\n%s", s)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: typo\ndisplay_name: Typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate after the fix: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "C223") {
		t.Fatalf("a manifest that marks still reported as not read:\n%s", out.String())
	}
}
