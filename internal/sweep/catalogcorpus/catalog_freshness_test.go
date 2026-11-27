package catalogcorpus

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// catalogTemplateRoot and exampleCatalogTemplateRoot are the two roots of
// the catalog templates — every catalog template
// RegenerateWhatsNextCatalog's own write set can reach. The recursive walk
// this replaced SkipDir'd a missing examples/, leaving the guard green over
// a write set it no longer mirrored; catalogRootFiles floors the bots/ root
// on its own. No example ships a template today, so the examples root
// carries floor 0: a template added there must have its generated catalog
// committed, and its presence is what the root tracks. A template nested one level deeper
// than today's flat layout reddens the routing guard before it can reach
// regen's write set unseen.
var (
	catalogTemplateRoot        = catalogRoot{"*/iterion-bot-catalog-static.md", 2, "every router bundle's generated-catalog template"}
	exampleCatalogTemplateRoot = catalogRoot{"../examples/*/iterion-bot-catalog-static.md", 0, "catalog templates under examples/ — none ships today; a new one must commit its generated catalog"}
)

func catalogTemplates(t *testing.T) []string {
	return append(catalogRootFiles(t, catalogTemplateRoot), catalogRootFiles(t, exampleCatalogTemplateRoot)...)
}

// catalogRegenInputs are the roots RegenerateWhatsNextCatalog discovers bots
// under (botregistry.DefaultPaths): the tracked files under them are the
// whole of what the regeneration reads.
var catalogRegenInputs = []string{botregistry.BotsDirName, "examples", ".botz"}

// TestCommittedCatalogIsFresh fails when the committed generated catalog
// (bots/whats-next/skills/iterion-bot-catalog.md) no longer matches what the
// current manifests render. A stale committed catalog is not cosmetic: the
// run of the bot that OWNS it refreshes that copy at start, so when its
// bundle is the one inside the run's workspace (Nexie is `worktree: none`)
// the regen produces a real diff against HEAD and the finalize wip-banks an
// otherwise-clean run (observed on back-to-back smoke runs, 2026-07-07).
// When this fails, run `iterion bots regen-catalog` and commit the result
// alongside the manifest change.
//
// The regeneration runs on a COPY of the tracked inputs, never on the
// checkout: it rewrites every owner's catalog in place, and a test that
// rewrites tracked files races every other test binary reading them — go
// test runs packages concurrently, and catalogclaims reads these very
// catalogs. The copy is compared file by file with the checkout, so whatever
// the regeneration rewrites is judged, an owner this test does not name
// included.
func TestCommittedCatalogIsFresh(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	scratch, copied := copyTrackedInputs(t, repoRoot, catalogRegenInputs...)

	// Every bundle shipping the static template owns a generated catalog
	// (whats-next, issue-triage, …), committed beside it. The template roots
	// are floored at two (both router bundles ship one), so a discovery that
	// lost them reddens there instead of judging nothing.
	for _, tmpl := range catalogTemplates(t) {
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(tmpl), "skills", "iterion-bot-catalog.md"))
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(repoRoot, abs)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := copied[filepath.ToSlash(rel)]; !ok {
			t.Fatalf("%s ships a catalog template and its generated catalog %s is not committed — "+
				"run `iterion bots regen-catalog` and commit the result", tmpl, rel)
		}
	}

	if _, err := botregistry.RegenerateWhatsNextCatalog(context.Background(), scratch); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	var stale []string
	for rel, before := range copied {
		after, err := os.ReadFile(filepath.Join(scratch, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read the regenerated copy of %s: %v", rel, err)
		}
		if !bytes.Equal(after, before) {
			stale = append(stale, rel)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("committed catalog(s) STALE vs the current manifests: %s.\n"+
			"Every run-start regen will dirty the worktree (and wip-bank clean runs).\n"+
			"Fix: run `iterion bots regen-catalog` and commit the listed file(s) with your manifest change.",
			strings.Join(stale, ", "))
	}
}

// copyTrackedInputs copies the files git tracks under the named roots of
// repoRoot into a fresh temporary directory, at the same relative paths and
// modes, and returns that directory with the bytes it copied, keyed by
// slash-separated path. The bytes are the checkout's, so an edit not yet
// committed is judged; a file git does not track is not an input, so the
// verdict does not move with what lies around in the checkout.
func copyTrackedInputs(t *testing.T, repoRoot string, roots ...string) (string, map[string][]byte) {
	t.Helper()
	args := append([]string{"ls-files", "-z", "--"}, roots...)
	out, err := gittest.Cmd(repoRoot, gitlib.NoRunHooks(args...)...).Output()
	if err != nil {
		t.Fatalf("git ls-files %v in %s: %v", roots, repoRoot, err)
	}
	scratch := t.TempDir()
	copied := map[string][]byte{}
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		src := filepath.Join(repoRoot, filepath.FromSlash(rel))
		info, err := os.Lstat(src)
		if err != nil {
			t.Fatalf("stat the tracked file %s: %v", rel, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is tracked as a %v, which this copy does not reproduce — teach copyTrackedInputs that kind", rel, info.Mode().Type())
		}
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		dst := filepath.Join(scratch, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, body, info.Mode().Perm()|fs.FileMode(0o200)); err != nil {
			t.Fatal(err)
		}
		copied[rel] = body
	}
	if len(copied) == 0 {
		t.Fatalf("git tracks no file under %v in %s — the regeneration would judge nothing", roots, repoRoot)
	}
	return scratch, copied
}
