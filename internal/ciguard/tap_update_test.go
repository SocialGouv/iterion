package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The writer of the tap, scripts/update-brew-tap.sh, run on a copy of the
// repository's own formula and cask: it never moves either file to a version
// below the one it carries. A cask ahead of the formula — a desktop-only
// release dispatched into it — must survive the formula catching up with an
// older tag.

var (
	versionLine   = regexp.MustCompile(`(?m)^(\s*version ")[^"]*(")`)
	regexpVersion = regexp.MustCompile(`(?m)^\s*version "([^"]*)"`)
	// A tap value line: indentation, key, spacing, quoted value.
	regexpTapValueLine = regexp.MustCompile(`^([ \t]*)(version|sha256)([ \t]+)("[^"]*")$`)
	// The version line's value, for a rewrite that keeps everything around it.
	regexpVersionValue = regexp.MustCompile(`(?m)^([ \t]*version[ \t]+)"[0-9.]+"`)
)

// tapTree copies the script and the real tap files into a scratch tree, sets
// each file's version, and writes artefacts for the four CLI platforms and
// the desktop ZIP.
func tapTree(t *testing.T, formula, cask string) (root, artefacts string) {
	t.Helper()
	root = t.TempDir()
	for rel, version := range map[string]string{"Formula/iterion.rb": formula, "Cask/iterion-desktop.rb": cask} {
		src, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		writeFiles(t, root, map[string]string{rel: versionLine.ReplaceAllString(string(src), "${1}"+version+"${2}")})
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "update-brew-tap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"scripts/update-brew-tap.sh": string(script)})
	artefacts = t.TempDir()
	for _, name := range append(append([]string{}, completeCLI...), "iterion-desktop-darwin-universal.zip") {
		writeFiles(t, artefacts, map[string]string{name: "artefact " + name + "\n"})
	}
	return root, artefacts
}

func updateTap(t *testing.T, root, artefacts, version string) string {
	t.Helper()
	cmd := exec.Command("bash", scriptUnderTest(t, filepath.Join(root, "scripts", "update-brew-tap.sh")), version, artefacts)
	cmd.Dir = root
	cmd.Env = scriptEnv(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("update-brew-tap.sh %s: %v\n%s", version, err, out)
	}
	return string(out)
}

func versionIn(t *testing.T, root, rel string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	m := regexpVersion.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatalf("%s carries no version line", rel)
	}
	return m[1]
}

func TestTapUpdateMovesBothFilesForward(t *testing.T) {
	root, artefacts := tapTree(t, "1.0.0", "1.0.0")
	updateTap(t, root, artefacts, "1.1.0")
	for _, rel := range []string{"Formula/iterion.rb", "Cask/iterion-desktop.rb"} {
		if got := versionIn(t, root, rel); got != "1.1.0" {
			t.Errorf("%s carries %s after tapping 1.1.0, want 1.1.0", rel, got)
		}
	}
}

func TestTapUpdateNeverWalksAFileBack(t *testing.T) {
	root, artefacts := tapTree(t, "1.0.0", "1.2.0")
	out := updateTap(t, root, artefacts, "1.1.0")
	if got := versionIn(t, root, "Formula/iterion.rb"); got != "1.1.0" {
		t.Errorf("the formula carries %s after tapping 1.1.0, want 1.1.0", got)
	}
	if got := versionIn(t, root, "Cask/iterion-desktop.rb"); got != "1.2.0" {
		t.Errorf("the cask, ahead at 1.2.0, carries %s after tapping 1.1.0 — walked back:\n%s", got, out)
	}
	if !strings.Contains(out, "skipped Cask/iterion-desktop.rb (it carries a version above 1.1.0") {
		t.Errorf("the refused cask write does not say why:\n%s", out)
	}
	root, artefacts = tapTree(t, "1.2.0", "1.0.0")
	out = updateTap(t, root, artefacts, "1.1.0")
	if got := versionIn(t, root, "Formula/iterion.rb"); got != "1.2.0" {
		t.Errorf("the formula, at 1.2.0, carries %s after tapping 1.1.0 — walked back", got)
	}
	if !strings.Contains(out, "skipped Formula/iterion.rb (it carries a version above 1.1.0") {
		t.Errorf("the refused formula write does not say why:\n%s", out)
	}
}
