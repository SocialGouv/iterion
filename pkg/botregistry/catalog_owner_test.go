package botregistry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// catalogSentinel stands in for a COMMITTED generated catalog rendered by
// another build of the generator: bytes this build never produces, so any
// write is observable byte-wise.
const catalogSentinel = "---\nname: iterion-bot-catalog\n---\nRENDERED BY ANOTHER BUILD\n"

// catalogTemplateBody is the hand-authored template a catalog owner ships.
const catalogTemplateBody = "---\nname: iterion-bot-catalog\n---\nPREAMBLE\n\n" +
	catalogGeneratedBegin + "\n" + catalogGeneratedEnd + "\n"

// twoOwnerWorkspace vendors the two catalog owners the repo ships
// (whats-next and issue-triage, each with the template plus a committed
// generated copy holding catalogSentinel) and one non-owner, under a fresh
// workspace. manifestNames overrides a bundle's manifest file name, which
// is how a bundle that discovery does not accept as one is built.
func twoOwnerWorkspace(t *testing.T, manifestNames map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	stub := "agent x:\n  model: \"test\"\n"
	manifest := func(name string) string {
		if n, ok := manifestNames[name]; ok {
			return n
		}
		return bundle.ManifestFile
	}
	for _, name := range []string{"whats-next", "issue-triage"} {
		dir := filepath.Join(ws, "bots", name)
		writeFile(t, filepath.Join(dir, manifest(name)), "name: "+name+"\ndisplay_name: P-"+name+"\ndescription: Routes work.\n")
		writeFile(t, filepath.Join(dir, bundle.MainBotFile), stub)
		writeFile(t, filepath.Join(dir, catalogStaticName), catalogTemplateBody)
		writeFile(t, filepath.Join(dir, bundle.DirSkills, catalogGeneratedName), catalogSentinel)
	}
	nonOwner := filepath.Join(ws, "bots", "review-pr")
	writeFile(t, filepath.Join(nonOwner, manifest("review-pr")), "name: review-pr\ndisplay_name: Revi\ndescription: Reviewer.\n")
	writeFile(t, filepath.Join(nonOwner, bundle.MainBotFile), stub)
	ClearSchemaCache()
	return ws
}

// generatedIn returns the generated catalog path of a vendored bundle.
func generatedIn(ws, name string) string {
	return filepath.Join(ws, "bots", name, bundle.DirSkills, catalogGeneratedName)
}

// assertSentinel fails when a generated catalog moved off the committed
// sentinel — i.e. when this regeneration wrote a file it was not asked to.
func assertSentinel(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(body) != catalogSentinel {
		t.Errorf("%s was rewritten (collateral write: nobody asked for this file):\n%s", path, body)
	}
}

// TestCatalogTemplateShipped pins the one ownership test every write in
// catalog.go is gated on: shipping the hand-authored template is owning
// the generated catalog, and an answer the filesystem would not give is an
// error rather than a quiet "not an owner".
func TestCatalogTemplateShipped(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "whats-next")
	writeFile(t, filepath.Join(owner, bundle.ManifestFile), "name: whats-next\n")
	writeFile(t, filepath.Join(owner, catalogStaticName), catalogTemplateBody)
	nonOwner := filepath.Join(dir, "review-pr")
	writeFile(t, filepath.Join(nonOwner, bundle.ManifestFile), "name: review-pr\n")

	cases := []struct {
		name    string
		dir     string
		want    bool
		wantErr bool
	}{
		{"a bundle shipping the template owns its catalog", owner, true, false},
		{"a bundle without the template owns none", nonOwner, false, false},
		{"a bundle root that does not exist owns none", filepath.Join(dir, "absent"), false, false},
		{"an empty bundle root is an error, not a verdict", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := catalogTemplateShipped(tc.dir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("catalogTemplateShipped(%q) = (%v, nil), want an error", tc.dir, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("catalogTemplateShipped(%q): %v", tc.dir, err)
			}
			if got != tc.want {
				t.Errorf("catalogTemplateShipped(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

// TestCatalogTemplateShipped_UnreadableTemplateIsAnError pins the other
// half of that contract: a template whose stat fails for anything but "not
// there" answers an error, not "owns none" — a bare false would hand the
// caller a verdict the filesystem never gave, and a write into a workspace
// is not a decision to take off a guess.
//
// The unreadable verdict is built with a self-referencing symlink (ELOOP),
// not a sealed directory: a sealed root is readable by a privileged
// process, which would turn this witness into a skip exactly where the
// guard is least likely to be noticed.
func TestCatalogTemplateShipped_UnreadableTemplateIsAnError(t *testing.T) {
	dir := t.TempDir()
	looped := filepath.Join(dir, "looped-bundle")
	if err := os.MkdirAll(looped, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(looped, catalogStaticName)
	if err := os.Symlink(tmpl, tmpl); err != nil {
		t.Fatalf("symlink unavailable: %v", err)
	}

	owner, err := catalogTemplateShipped(looped)
	if err == nil {
		t.Fatalf("catalogTemplateShipped(%q) = (%v, nil), want the unreadable template reported", looped, owner)
	}
	if owner {
		t.Errorf("catalogTemplateShipped(%q) = (true, %v), want no ownership claimed alongside the error", looped, err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the looped template was reported as absent, so the witness proves nothing: %v", err)
	}
	if !strings.Contains(err.Error(), looped) {
		t.Errorf("error %q does not name the bundle root it could not read", err)
	}
}

// TestRegenerateWhatsNextCatalog_UnreadableTemplateIsAnError pins the
// workspace-wide write set on the same contract: an owner whose template
// cannot be read stops the regeneration out loud instead of being skipped
// as "not an owner" while the other owners are rewritten.
func TestRegenerateWhatsNextCatalog_UnreadableTemplateIsAnError(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	tmpl := filepath.Join(ws, "bots", "whats-next", catalogStaticName)
	if err := os.Remove(tmpl); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tmpl, tmpl); err != nil {
		t.Fatalf("symlink unavailable: %v", err)
	}

	first, err := RegenerateWhatsNextCatalog(context.Background(), ws)
	if err == nil {
		t.Fatalf("RegenerateWhatsNextCatalog = (%q, nil), want the unreadable template reported", first)
	}
	if !strings.Contains(err.Error(), filepath.Join(ws, "bots", "whats-next")) {
		t.Errorf("error %q does not name the bundle whose template it could not read", err)
	}
}

// TestRegenerateWhatsNextCatalog_ABrokenOwnerDoesNotCancelTheOthers pins
// what one bundle's filesystem problem may cost: the error it raises, and
// nothing else. The healthy owners are refreshed whichever side of the
// broken one the walk reaches them — so the write set is the rule ("every
// healthy owner") rather than the discovery order, and `iterion bots
// install` cannot leave Nexie's catalog stale because another bundle's
// template happens to be unreadable.
func TestRegenerateWhatsNextCatalog_ABrokenOwnerDoesNotCancelTheOthers(t *testing.T) {
	owners := []string{"issue-triage", "whats-next"}
	for i, broken := range owners {
		healthy := owners[(i+1)%len(owners)]
		t.Run("the unreadable owner is "+broken, func(t *testing.T) {
			ws := twoOwnerWorkspace(t, nil)
			tmpl := filepath.Join(ws, "bots", broken, catalogStaticName)
			if err := os.Remove(tmpl); err != nil {
				t.Fatal(err)
			}
			// A self-referencing symlink: ELOOP, which no privilege
			// reads either, so the verdict does not depend on who runs
			// the test.
			if err := os.Symlink(tmpl, tmpl); err != nil {
				t.Fatalf("symlink unavailable: %v", err)
			}

			first, err := RegenerateWhatsNextCatalog(context.Background(), ws)
			if err == nil {
				t.Fatalf("RegenerateWhatsNextCatalog = (%q, nil), want the unreadable template reported", first)
			}
			if !strings.Contains(err.Error(), filepath.Join(ws, "bots", broken)) {
				t.Errorf("error %q does not name the bundle whose template it could not read", err)
			}
			dest := generatedIn(ws, healthy)
			body, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(body) == catalogSentinel {
				t.Errorf("%s still holds the committed sentinel: %s's unreadable template cancelled a healthy owner's refresh", dest, broken)
			}
			if first != dest {
				t.Errorf("first = %q, want the healthy owner's catalog %q", first, dest)
			}
		})
	}
}

// TestRegenerateCatalogForBundle_WritesOnlyTheAskingBundle pins the write
// set a run-start regeneration gets: the asking bundle's own catalog, and
// no other owner's. The other owner's copy is a tracked file of the same
// workspace that this run has no copy of — rewriting it is the drift the
// scoped entry point exists to prevent.
func TestRegenerateCatalogForBundle_WritesOnlyTheAskingBundle(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	asking := filepath.Join(ws, "bots", "whats-next")

	dest, err := RegenerateCatalogForBundle(context.Background(), ws, asking)
	if err != nil {
		t.Fatalf("RegenerateCatalogForBundle: %v", err)
	}
	if want := generatedIn(ws, "whats-next"); dest != want {
		t.Fatalf("dest = %q, want %q", dest, want)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got == catalogSentinel {
		t.Errorf("the asking bundle's own catalog was NOT refreshed: %s still holds the committed sentinel", dest)
	} else if !strings.Contains(got, "| P-whats-next | `whats-next` (this bot) |") {
		t.Errorf("the refreshed catalog is missing the persona row rendered from the live manifests:\n%s", got)
	} else if !strings.Contains(got, "`issue-triage`") {
		t.Errorf("narrowing the write set narrowed the ROUTING TABLE: the other bots are missing:\n%s", got)
	}
	assertSentinel(t, generatedIn(ws, "issue-triage"))
}

// TestRegenerateCatalogForBundle_WorkspaceThroughASymlink pins the other
// side of "outside the workspace writes nothing": the same tree handed
// over under two spellings is ONE workspace. A workspace reached through
// a link read as "outside" would skip the owner's own refresh in silence
// — the stale copy the scoping must not introduce.
func TestRegenerateCatalogForBundle_WorkspaceThroughASymlink(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(ws, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	// The workspace spelled through the link, the bundle by its real path:
	// the shape a run gets when its workspace was handed over as a link.
	dest, err := RegenerateCatalogForBundle(context.Background(), link, filepath.Join(ws, "bots", "whats-next"))
	if err != nil {
		t.Fatalf("RegenerateCatalogForBundle through a symlinked workspace: %v", err)
	}
	if dest == "" {
		t.Fatalf("nothing was written: the linked workspace was read as outside itself")
	}
	body, err := os.ReadFile(generatedIn(ws, "whats-next"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == catalogSentinel {
		t.Errorf("the owner's own catalog was not refreshed through the linked workspace")
	}
	assertSentinel(t, generatedIn(ws, "issue-triage"))
}

// TestRegenerateCatalogForBundle_NothingToWriteIsQuiet pins the two shapes
// that have nothing of their own to refresh: a bundle shipping no template
// owns no catalog, and a bundle outside the workspace keeps the copy its
// packer froze — the regeneration renders from the workspace's manifests
// and never writes outside it. Neither may touch another owner's file, and
// neither is an error.
func TestRegenerateCatalogForBundle_NothingToWriteIsQuiet(t *testing.T) {
	t.Run("a bundle shipping no template", func(t *testing.T) {
		ws := twoOwnerWorkspace(t, nil)
		dest, err := RegenerateCatalogForBundle(context.Background(), ws, filepath.Join(ws, "bots", "review-pr"))
		if err != nil {
			t.Fatalf("a non-owner must be a quiet no-op, got: %v", err)
		}
		if dest != "" {
			t.Errorf("dest = %q, want no write for a bundle that owns no catalog", dest)
		}
		assertSentinel(t, generatedIn(ws, "whats-next"))
		assertSentinel(t, generatedIn(ws, "issue-triage"))
	})

	t.Run("an owner rooted outside the workspace", func(t *testing.T) {
		ws := twoOwnerWorkspace(t, nil)
		// The shape a materialized .botz takes: the bundle lives in the
		// pod's scratch, the workspace is the checkout under review.
		outside := twoOwnerWorkspace(t, nil)
		dest, err := RegenerateCatalogForBundle(context.Background(), ws, filepath.Join(outside, "bots", "whats-next"))
		if err != nil {
			t.Fatalf("a bundle outside the workspace must be a quiet no-op, got: %v", err)
		}
		if dest != "" {
			t.Errorf("dest = %q, want no write for a bundle rooted outside the workspace", dest)
		}
		assertSentinel(t, generatedIn(ws, "whats-next"))
		assertSentinel(t, generatedIn(ws, "issue-triage"))
		assertSentinel(t, generatedIn(outside, "whats-next"))
	})
}

// TestRegenerateCatalogForBundle_YmlSpelledOwnerIsInTheWriteSet pins the
// disagreement ADR-140 retired: `manifest.yml` used to be a spelling
// bundle.OpenDir honoured and discovery did not, so such a bundle backed
// runs while owning no entry in the write set. The claim reads both
// spellings now — the owner is discovered, and its catalog refreshes in
// place like any owner's.
func TestRegenerateCatalogForBundle_YmlSpelledOwnerIsInTheWriteSet(t *testing.T) {
	ws := twoOwnerWorkspace(t, map[string]string{"whats-next": bundle.ManifestFileAlt})
	asking := filepath.Join(ws, "bots", "whats-next")
	if _, err := bundle.OpenDir(asking); err != nil {
		t.Fatalf("the fixture must be a bundle a run can carry: %v", err)
	}

	dest, err := RegenerateCatalogForBundle(context.Background(), ws, asking)
	if err != nil {
		t.Fatalf("RegenerateCatalogForBundle = (%q, %v), want the .yml-spelled owner refreshed in place", dest, err)
	}
	if dest == "" {
		t.Errorf("dest = %q, want the owner's catalog written", dest)
	}
	got, err := os.ReadFile(generatedIn(ws, "whats-next"))
	if err != nil || string(got) == catalogSentinel {
		t.Fatalf("the .yml-spelled owner's catalog was not refreshed: %v", err)
	}
}

// TestRegenerateCatalogForBundle_UndiscoveredOwnerIsAnError keeps its name
// for the shape that still has the property: a directory the claim marks
// but that carries no main.bot (ADR-140) is skipped by discovery — it owns
// no entry in the write set. Refreshing its catalog would be a write no
// listed bundle owns, and refreshing nothing in silence would leave a
// caller believing its own catalog is current.
func TestRegenerateCatalogForBundle_UndiscoveredOwnerIsAnError(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	asking := filepath.Join(ws, "bots", "whats-next")
	if err := os.Remove(filepath.Join(asking, bundle.MainBotFile)); err != nil {
		t.Fatal(err)
	}
	shipped, err := catalogTemplateShipped(asking)
	if err != nil || !shipped {
		t.Fatalf("catalogTemplateShipped(%q) = (%v, %v), want the fixture to claim ownership", asking, shipped, err)
	}

	dest, err := RegenerateCatalogForBundle(context.Background(), ws, asking)
	if err == nil {
		t.Fatalf("RegenerateCatalogForBundle = (%q, nil), want the undiscovered owner reported", dest)
	}
	if !strings.Contains(err.Error(), asking) {
		t.Errorf("error %q does not name the bundle it could not place in the write set", err)
	}
	if dest != "" {
		t.Errorf("dest = %q, want nothing written when the asking bundle is not in the write set", dest)
	}
	assertSentinel(t, generatedIn(ws, "whats-next"))
	assertSentinel(t, generatedIn(ws, "issue-triage"))
}
