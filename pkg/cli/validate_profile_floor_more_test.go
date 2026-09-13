package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bundle known by its skills/ alone — no manifest — written in profile 2
// draws C252 through the real command: there is no floor and no manifest
// to carry one. A floor declared but below the release draws it too.
func TestRunValidate_AsksAManifestLessOrLowFlooredBundleForItsFloor(t *testing.T) {
	inTempWorkspace(t)
	dir := filepath.Join("bots", "nm")
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainBot := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(mainBot, []byte("dsl: 2\n\nworkflow nm:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C252") {
		t.Fatalf("a manifest-less profile-2 bundle was not asked for its floor:\n%s", s)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: nm\ndisplay_name: NM\nrequires:\n  iterion: \">= 0.0.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate with a low floor: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C252") || !strings.Contains(s, "3.141.0") {
		t.Fatalf("a floor below the release was not refused:\n%s", s)
	}
}
