package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestMaxSyntaxRequirementsCountsRootEntries: a root-level sibling entry
// executes under the bundle's manifest without any edge from main.bot
// reaching it, so its syntax lifts the bundle's floor in BOTH forms of the
// walk — the directory on disk (MaxSyntaxRequirementsDir, what C252 and
// `dsl migrate` read) and the files map a bot source carries
// (MaxSyntaxRequirements). A sibling declaring `dsl: 2` while main.bot
// does not is the #1367 case: invisible to the walk, it left C252 silent
// and the catalogue's consistency gate unable to redden. A fragment under
// lib/ is no entry: the walk reaches it through the unit's imports only.
func TestMaxSyntaxRequirementsCountsRootEntries(t *testing.T) {
	mainSrc := "workflow main_w:\n  entry: done\n"
	entrySrc := "dsl: 2\n\nworkflow extend_w:\n  entry: done\n"
	fragSrc := "dsl: 2\n\nagent frag:\n  description: \"x\"\n"

	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.bot", mainSrc)
	write("extend.bot", entrySrc)
	write("lib/frag.bot", fragSrc)

	d := MaxSyntaxRequirementsDir(dir)
	if d.Profile != 2 || !reflect.DeepEqual(d.DeclaredBy, []string{"extend.bot"}) {
		t.Fatalf("dir: profile %d declaredBy %v — the root entry was not walked, or the unimported lib/ fragment was", d.Profile, d.DeclaredBy)
	}

	m := MaxSyntaxRequirements(map[string]string{"main.bot": mainSrc, "extend.bot": entrySrc})
	if m.Profile != 2 || !reflect.DeepEqual(m.DeclaredBy, []string{"extend.bot"}) {
		t.Fatalf("map: profile %d declaredBy %v — the root entry was not walked", m.Profile, m.DeclaredBy)
	}
	// An unimported fragment under lib/ counts in neither form.
	m = MaxSyntaxRequirements(map[string]string{"main.bot": mainSrc, "lib/frag.bot": fragSrc})
	if m.Profile != 1 {
		t.Fatalf("map with an unimported lib/ fragment: profile %d, want 1", m.Profile)
	}
}

// TestMaxSyntaxRequirementsCountsACaseFoldedEntry: DirForEntry promotes
// EXTEND.BOT (workflowfile.IsWorkflowFile folds case), so the floor walk
// must see it too — a sibling promoted by path but invisible to C250–C253
// and `dsl migrate` is the two-doors divergence the fold exists to close.
func TestMaxSyntaxRequirementsCountsACaseFoldedEntry(t *testing.T) {
	mainSrc := "workflow main_w:\n  entry: done\n"
	entrySrc := "dsl: 2\n\nworkflow extend_w:\n  entry: done\n"

	d := MaxSyntaxRequirementsDir(t.TempDir())
	if d.Profile != 0 {
		t.Fatalf("empty dir: profile %d, want 0", d.Profile)
	}
	m := MaxSyntaxRequirements(map[string]string{"main.bot": mainSrc, "EXTEND.BOT": entrySrc})
	if m.Profile != 2 || !reflect.DeepEqual(m.DeclaredBy, []string{"EXTEND.BOT"}) {
		t.Fatalf("map: profile %d declaredBy %v — the case-folded root entry was not walked", m.Profile, m.DeclaredBy)
	}

	dir := t.TempDir()
	for rel, body := range map[string]string{"main.bot": mainSrc, "EXTEND.BOT": entrySrc} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if d := MaxSyntaxRequirementsDir(dir); d.Profile != 2 {
		t.Fatalf("dir: profile %d, want 2 — the case-folded root entry was not walked", d.Profile)
	}
}
