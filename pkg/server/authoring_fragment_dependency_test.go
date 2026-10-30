package server

import (
	"net/http"
	"strings"
	"testing"
)

// A bundle manifest declaring a shared-fragment dependency (ADR-145): the
// consumer's `import "lib/forge-auth.bot"` is the line the dependency
// exists for, and no file of the bundle itself provides it.
const depManifest = "name: demo\ndependencies:\n  fragments:\n    - name: forge-auth\n      from: app-dev\n"

// depMain imports the declared fragment beside the one the bundle ships;
// the workflow uses only the shipped fragment's node, so the miss of the
// DEPENDENCY is the one thing any load of this unit finds.
const depMain = "import \"lib/nodes.bot\"\nimport \"lib/forge-auth.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"

// TestTheApplyNamesTheDeclaredDependencyBehindAMissingFragment: the cloud
// editor's per-file apply still REFUSES an edit that introduces the import
// of an unmaterialized fragment — whether it should resolve or tolerate a
// declared dependency instead is #2865's authoring-surface decision, not
// taken here — but the refusal names the actor the miss belongs to
// (ADR-145 clause 3): the manifest's dependencies.fragments entry, not the
// import line, which is the one line that is right. An undeclared miss in
// the same bundle keeps the generic refusal, naming no dependency.
func TestTheApplyNamesTheDeclaredDependencyBehindAMissingFragment(t *testing.T) {
	s := &Server{}
	files := map[string]any{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes, "manifest.yaml": depManifest}

	added := strings.Replace(unitFixtureMain, "import \"lib/nodes.bot\"\n", "import \"lib/nodes.bot\"\nimport \"lib/forge-auth.bot\"\n", 1)
	rec, _ := parseCall(t, s, map[string]any{"files": files, "main": "main.bot", "file": "main.bot", "source": added})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("adding the import a declared dependency materializes: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"lib/forge-auth.bot", "dependencies.fragments", "forge-auth", "app-dev", "iterion bots sync"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the refusal does not name the dependency (%q missing):\n%s", want, body)
		}
	}

	// The control: an UNDECLARED miss of the same shape names no dependency.
	stray := strings.Replace(unitFixtureMain, "import \"lib/nodes.bot\"\n", "import \"lib/nodes.bot\"\nimport \"lib/stray.bot\"\n", 1)
	rec, _ = parseCall(t, s, map[string]any{"files": files, "main": "main.bot", "file": "main.bot", "source": stray})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("adding an undeclared miss: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dependencies.fragments") {
		t.Fatalf("an undeclared miss reads as a dependency's:\n%s", rec.Body.String())
	}
}

// TestTheSaveNamesTheDeclaredDependencyBehindAMissingFragment: a bundle on
// disk that declares the dependency and carries the import line (authored
// git-natively, before `iterion bots sync` materializes the fragment) opens
// and takes unrelated edits — the miss is the stored unit's existing
// business — and the save's refusal, unchanged, names the dependency rather
// than sending the author to "fix" files of theirs.
func TestTheSaveNamesTheDeclaredDependencyBehindAMissingFragment(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{
		"demo/main.bot":      depMain,
		"demo/lib/nodes.bot": unitFixtureNodes,
		"demo/manifest.yaml": depManifest,
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}

	edited := strings.Replace(depMain, "entry: worker", "entry: worker\n  ## a note", 1)
	rec, applied := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": edited})
	if rec.Code != http.StatusOK {
		t.Fatalf("an unrelated edit of a bot whose dependency is not materialized: %d %s", rec.Code, rec.Body.String())
	}

	rec, _ = savePathClaimed(t, s, "demo/main.bot", applied.Document, opened.Unit.Revision, applied.Unit.Files)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the save of a unit that does not load: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"lib/forge-auth.bot", "dependencies.fragments", "forge-auth", "app-dev"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the save's refusal does not name the dependency (%q missing):\n%s", want, body)
		}
	}
	if got := readFixture(t, workdir, "demo/main.bot"); got != depMain {
		t.Fatalf("the refused save wrote:\n%s", got)
	}
}

// TestTheWriteGateNamesTheDeclaredDependencyBehindAMissingFragment: the
// bot-source write gate (validateBundleCompileSelected) still refuses the
// bundle — a dependency nothing materialized is not compiled away — and
// its diagnostic names the manifest entry the miss belongs to, so a
// git-native pusher learns why a bundle that installs fine is refused here.
// The same miss undeclared names no dependency.
func TestTheWriteGateNamesTheDeclaredDependencyBehindAMissingFragment(t *testing.T) {
	files := map[string]string{
		"main.bot":      depMain,
		"lib/nodes.bot": unitFixtureNodes,
		"manifest.yaml": depManifest,
	}
	diags := validateBundleCompileSelected(files, nil)
	if len(diags) == 0 {
		t.Fatal("a bundle whose declared fragment is not there compiled clean")
	}
	joined := strings.Join(diags, "; ")
	for _, want := range []string{"no such fragment", "lib/forge-auth.bot", "dependencies.fragments", "forge-auth", "app-dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the write gate does not name the dependency (%q missing): %s", want, joined)
		}
	}

	delete(files, "manifest.yaml")
	diags = validateBundleCompileSelected(files, nil)
	if len(diags) == 0 {
		t.Fatal("the undeclared miss compiled clean")
	}
	if strings.Contains(strings.Join(diags, "; "), "dependencies.fragments") {
		t.Fatalf("an undeclared miss reads as a dependency's: %v", diags)
	}
}
