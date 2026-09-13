package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `iterion validate` on a bundle written in profile 2 whose manifest
// declares no engine floor says so (C252), through the real command; with
// the floor declared, the warning is gone.
func TestRunValidate_AsksAProfileTwoBundleForItsFloor(t *testing.T) {
	inTempWorkspace(t)
	dir := filepath.Join("bots", "p2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainBot := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(mainBot, []byte("dsl: 2\n\nworkflow p2:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: p2\ndisplay_name: P2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C252") || !strings.Contains(s, "main.bot") {
		t.Fatalf("validate did not ask for the floor:\n%s", s)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: p2\ndisplay_name: P2\nrequires:\n  iterion: \">= 3.141.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate with the floor: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "C252") {
		t.Fatalf("the floor is declared, yet asked for:\n%s", out.String())
	}
}
