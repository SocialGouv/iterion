package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// writeSiblingEntryBundle writes the golden-master shape of #1367: a
// bundle whose manifest sits beside main.bot AND a second root-level
// entry, extend.bot — an entry of its own, launched by path, not a
// fragment (those live under lib/).
func writeSiblingEntryBundle(t *testing.T, manifest string) (dir, sibling string) {
	t.Helper()
	dir = filepath.Join("bots", "gm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", manifest)
	write("main.bot", "workflow main_w:\n  entry: done\n")
	write("extend.bot", "workflow extend_w:\n  entry: done\n")
	return dir, filepath.Join(dir, "extend.bot")
}

// A sibling entry opened as a file is the bundle's, exactly as the
// directory form is: the manifest is read and its engine floor is held
// against this build (C250), so `validate bots/gm/extend.bot` on a bundle
// requiring `>= 9999.0.0` refuses — where the bare-file fall-through
// validated OK, reading no manifest at all (#1367, facet 1).
func TestRunValidate_PromotesASiblingEntryToItsBundle(t *testing.T) {
	inTempWorkspace(t)
	prevV, prevC, prevM := appinfo.Version, appinfo.Commit, appinfo.Modified
	t.Cleanup(func() { appinfo.Version, appinfo.Commit, appinfo.Modified = prevV, prevC, prevM })
	appinfo.Version, appinfo.Commit, appinfo.Modified = "3.0.0", "", false
	dir, sibling := writeSiblingEntryBundle(t, "name: gm\nversion: 0.1.0\nrequires:\n  iterion: \">= 9999.0.0\"\n")

	jp, out := jsonPrinter()
	err := RunValidate(sibling, jp)
	if err == nil {
		t.Fatalf("validate %s against a floor this build cannot reach: err = nil — the manifest was not read:\n%s", sibling, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "C250") {
		t.Fatalf("no C250 for the sibling entry — its bundle's floor was not held against this build:\n%s", s)
	}
	if !strings.Contains(s, `"bundle_name": "gm"`) {
		t.Fatalf("no bundle identity in the result — the sibling was validated as a loose file:\n%s", s)
	}
	// The directory form gives the same verdict: one bundle, one answer.
	jp, out = jsonPrinter()
	if err := RunValidate(dir, jp); err == nil || !strings.Contains(out.String(), "C250") {
		t.Fatalf("the directory form: err = %v\n%s", err, out.String())
	}
}

// The sibling's own syntax lifts the bundle's floor: an extend.bot
// declaring `dsl: 2` beside a profile-1 main.bot draws C252 naming
// extend.bot — the syntax walk reads every root-level entry, and the
// promotion puts the sibling under the manifest that carries the floor
// (#1367, facets 1 and 2 together).
func TestRunValidate_SiblingEntryAsksForTheBundlesFloor(t *testing.T) {
	inTempWorkspace(t)
	_, sibling := writeSiblingEntryBundle(t, "name: gm\nversion: 0.1.0\n")
	if err := os.WriteFile(sibling, []byte("dsl: 2\n\nworkflow extend_w:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidate(sibling, jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C252") || !strings.Contains(s, "extend.bot") {
		t.Fatalf("the sibling's own profile was not held against the manifest (C252 naming extend.bot):\n%s", s)
	}
}

// The manifest's trigger bindings belong to the canonical main, never to a
// sibling entry run by path: a sibling validated under a manifest whose
// dispatch_vars/invocations main.bot declares draws NO C200/C204 — the
// directory form's verdict, not a misreading of the sibling as the main
// (#1367 review, HIGH 2).
func TestRunValidate_SiblingEntryDrawsNoMainBoundManifestFindings(t *testing.T) {
	inTempWorkspace(t)
	dir, sibling := writeSiblingEntryBundle(t, "name: gm\nversion: 0.1.0\ndispatch_vars:\n  issue: ref\ninvocations:\n  - kind: command\n    mode: board\n    args_var: issue\n    command: { name: gm }\n")
	// main.bot declares the var the manifest binds; the sibling does not.
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow main_w:\n  vars:\n    issue: string\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidate(sibling, jp); err != nil {
		t.Fatalf("validate %s: %v\n%s", sibling, err, out.String())
	}
	for _, code := range []string{"C200", "C204"} {
		if strings.Contains(out.String(), code) {
			t.Fatalf("%s against the sibling — the manifest binds to the canonical main, which declares the var:\n%s", code, out.String())
		}
	}
}

// The two error-severity traps of the review: a manifest chat: section
// valid against main.bot must not refuse the sibling (C205), and a sibling
// using per-bot memory must not trip the name-stability check (C230) —
// both read against the canonical main, as the directory form reads them.
func TestRunValidate_SiblingEntryIsNotInvalidForTheMainsManifestContract(t *testing.T) {
	inTempWorkspace(t)
	manifest := "name: gm\nversion: 0.1.0\nchat:\n  nodes:\n    ask: {kind: human, text_field: message}\n"
	dir, sibling := writeSiblingEntryBundle(t, manifest)
	// main.bot carries the chat node the manifest names; the sibling does
	// not, and it uses per-bot memory under a workflow name that is not the
	// bundle's — both fine for an entry that is not the main.
	mainBot := "schema chat_answer:\n  message: string\n\nhuman ask:\n  description: \"the operator's turn\"\n  output: chat_answer\n\nworkflow main_w:\n  entry: ask\n  ask -> done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(mainBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("agent mem:\n  backend: \"claw\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  memory:\n    enabled: true\n    visibility: \"bot\"\n    scope: \"x\"\n\nworkflow extend_w:\n  entry: mem\n  mem -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidate(sibling, jp); err != nil {
		t.Fatalf("validate %s: %v\n%s", sibling, err, out.String())
	}
	for _, code := range []string{"C205", "C230"} {
		if strings.Contains(out.String(), code) {
			t.Fatalf("%s against the sibling — the chat surface and the memory-name invariant bind to the canonical main:\n%s", code, out.String())
		}
	}
	// The directory form is clean under the same manifest: one verdict.
	jp, out = jsonPrinter()
	if err := RunValidate(dir, jp); err != nil {
		t.Fatalf("the directory form: %v\n%s", err, out.String())
	}
}
