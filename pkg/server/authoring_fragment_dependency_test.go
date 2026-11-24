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
// import line, which is the one line that is right — and, when that miss is
// the ONLY thing refused, the refusal does not also say "fix the import",
// which would send the author to repair that very line. An undeclared miss
// in the same bundle keeps the generic refusal AND its remedy, naming no
// dependency; an apply refused for both carries both.
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

	// The wrong actor's imperative: every refused diagnostic here is the
	// declared dependency's, so no sentence tells the author to fix an
	// import of theirs.
	for _, unwanted := range []string{"fix the import", "under the bot's lib/ directory"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("the refusal still sends the author to the import (%q present):\n%s", unwanted, body)
		}
	}

	// The control: an UNDECLARED miss of the same shape names no dependency
	// and keeps the remedy, which is then the author's own.
	stray := strings.Replace(unitFixtureMain, "import \"lib/nodes.bot\"\n", "import \"lib/nodes.bot\"\nimport \"lib/stray.bot\"\n", 1)
	rec, _ = parseCall(t, s, map[string]any{"files": files, "main": "main.bot", "file": "main.bot", "source": stray})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("adding an undeclared miss: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dependencies.fragments") {
		t.Fatalf("an undeclared miss reads as a dependency's:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fix the import") {
		t.Fatalf("an undeclared miss lost the remedy that is the author's own:\n%s", rec.Body.String())
	}

	// And an apply refused for BOTH carries both: the dependency named, and
	// the remedy for the import that really is the author's to fix.
	mixed := strings.Replace(unitFixtureMain, "import \"lib/nodes.bot\"\n", "import \"lib/nodes.bot\"\nimport \"lib/forge-auth.bot\"\nimport \"lib/stray.bot\"\n", 1)
	rec, _ = parseCall(t, s, map[string]any{"files": files, "main": "main.bot", "file": "main.bot", "source": mixed})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("adding a declared miss beside an undeclared one: %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"dependencies.fragments", "lib/stray.bot", "fix the import"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("a mixed refusal drops %q:\n%s", want, rec.Body.String())
		}
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

// subEntryMain is a bundle entry that does NOT sit at the bundle root. Its
// `import "lib/forge-auth.bot"` resolves BESIDE it — `sub/lib/forge-auth.bot`
// for a reading keyed from the bundle root — and the install materializes a
// declared dependency at the bundle root's own lib/ (ADR-145 clause 2), so
// no declared dependency is what this import misses.
const subEntryMain = "workflow w:\n  entry: worker\n  worker -> done\n"

// TestADeclaredFragmentIsKeyedAtTheBundleRoot: the path a declared
// dependencies.fragments entry materializes at is the BUNDLE's
// `lib/<name>.bot` — the one key the install, the launch's freeze and an
// `import` of a root entry all mean. Keyed instead on the directory of
// whatever workflow is being read, the annotation fires for a miss BESIDE a
// subdirectory entry, where nothing materializes, and tells the author a
// file is a dependency's when no dependency could ever provide it — and the
// resolution that follows the same key would place the fragment where no
// install ever will.
func TestADeclaredFragmentIsKeyedAtTheBundleRoot(t *testing.T) {
	s := &Server{}
	files := map[string]any{
		"main.bot":      unitFixtureMain,
		"lib/nodes.bot": unitFixtureNodes,
		"sub/other.bot": subEntryMain,
		"manifest.yaml": depManifest,
	}

	// The apply of a subdirectory entry that newly imports the declared
	// name: refused, and named as the author's own import — not as the
	// dependency's, which materializes at the bundle root.
	added := "import \"lib/forge-auth.bot\"\n\n" + subEntryMain
	rec, _ := parseCall(t, s, map[string]any{"files": files, "main": "sub/other.bot", "file": "sub/other.bot", "source": added})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a subdirectory entry importing the declared name: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sub/lib/forge-auth.bot") {
		t.Fatalf("the refusal does not name the path the import resolved to:\n%s", body)
	}
	if strings.Contains(body, "dependencies.fragments") {
		t.Fatalf("a miss beside a subdirectory entry reads as the declared dependency's, which materializes at the bundle root:\n%s", body)
	}
	if !strings.Contains(body, "fix the import") {
		t.Fatalf("the refusal drops the remedy that is the author's own here:\n%s", body)
	}

	// The write gate reads the same bundle through a load rooted BESIDE the
	// entry, so its rel is `lib/forge-auth.bot` in a frame that is not the
	// bundle's: the declaration must not be read into it either.
	gate := map[string]string{
		"main.bot":      unitFixtureMain,
		"lib/nodes.bot": unitFixtureNodes,
		"sub/other.bot": added,
		"manifest.yaml": depManifest,
	}
	diags := validateBundleCompileSelected(gate, []string{"sub/other.bot"})
	if len(diags) == 0 {
		t.Fatal("a subdirectory entry importing a fragment nothing provides compiled clean")
	}
	joined := strings.Join(diags, "; ")
	if !strings.Contains(joined, "no such fragment") {
		t.Fatalf("the write gate does not report the miss: %s", joined)
	}
	if strings.Contains(joined, "dependencies.fragments") {
		t.Fatalf("the write gate reads the bundle's declaration into the entry's own frame: %s", joined)
	}

	// The control, same manifest: at the bundle ROOT the two keys are the
	// same key, and the dependency IS named.
	root := strings.Replace(unitFixtureMain, "import \"lib/nodes.bot\"\n", "import \"lib/nodes.bot\"\nimport \"lib/forge-auth.bot\"\n", 1)
	rec, _ = parseCall(t, s, map[string]any{"files": files, "main": "main.bot", "file": "main.bot", "source": root})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the root entry importing the declared name: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dependencies.fragments") {
		t.Fatalf("the root entry's miss is not named as the dependency's:\n%s", rec.Body.String())
	}
}

// TestTheOpenedUnitsVerdictNamesTheDeclaredDependency: the load verdict the
// cloud editor OPENS with — unitInfo.LoadError, the string the studio's
// Diagnostics panel shows — is the surface the author actually reads, and it
// went out generic while three refusal sites named the dependency. Every
// answer that quotes one load diagnostic now goes through the one
// annotation (fragmentDependencies.annotate).
func TestTheOpenedUnitsVerdictNamesTheDeclaredDependency(t *testing.T) {
	s := &Server{}
	files := map[string]any{"main.bot": depMain, "lib/nodes.bot": unitFixtureNodes, "manifest.yaml": depManifest}
	rec, out := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK || out.Unit == nil {
		t.Fatalf("opening a bundle whose declared fragment is not materialized: %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"no such fragment", "dependencies.fragments", "forge-auth", "app-dev"} {
		if !strings.Contains(out.Unit.LoadError, want) {
			t.Fatalf("the opened unit's load verdict does not name the dependency (%q missing): %q", want, out.Unit.LoadError)
		}
	}
}

// TestTheCloudSaveNamesTheDeclaredDependency: the cloud editor's write-back
// (unparseUnitFiles) answered the generic "the bundle's files do not load as
// one unit" for the very miss the apply names — kimi's own named follow-up
// on #2865. It is the same class and now the same sentence.
func TestTheCloudSaveNamesTheDeclaredDependency(t *testing.T) {
	s := &Server{}
	files := map[string]any{"main.bot": depMain, "lib/nodes.bot": unitFixtureNodes, "manifest.yaml": depManifest}
	rec, opened := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = unparseCall(t, s, map[string]any{"document": opened.Document, "files": files, "main": "main.bot", "revision": opened.Unit.Revision})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the cloud save of a unit that does not load: %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"dependencies.fragments", "forge-auth", "app-dev"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the cloud save's refusal does not name the dependency (%q missing):\n%s", want, rec.Body.String())
		}
	}
}

// TestTheClaimedFileListNamesTheDeclaredDependency: finishClaimed answers
// the unit a document's claimed file list makes of the stored one, and its
// refusal — "the files the document claims do not load as one unit" — was
// the fifth site of the class, generic while the others named the actor. A
// client whose claim adds the declared import reaches it directly.
func TestTheClaimedFileListNamesTheDeclaredDependency(t *testing.T) {
	s := &Server{}
	files := map[string]any{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes, "manifest.yaml": depManifest}
	rec, opened := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	claimed := make([]unitFileInfo, 0, len(opened.Unit.Files))
	for _, f := range opened.Unit.Files {
		if f.Rel == "main.bot" {
			f.Imports = append(append([]string{}, f.Imports...), "lib/forge-auth.bot")
		}
		claimed = append(claimed, f)
	}
	rec, _ = unparseCall(t, s, map[string]any{
		"document":   opened.Document,
		"files":      files,
		"main":       "main.bot",
		"revision":   opened.Unit.Revision,
		"unit_files": claimed,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a claim that adds the declared import: %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"the files the document claims", "dependencies.fragments", "forge-auth", "app-dev"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the claim's refusal does not name the dependency (%q missing):\n%s", want, rec.Body.String())
		}
	}
}
