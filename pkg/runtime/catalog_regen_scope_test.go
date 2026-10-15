package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// catalogSentinel stands in for a COMMITTED generated catalog rendered by
// another build of the generator: bytes the current generator never
// produces, so any regeneration is observable byte-wise.
const catalogSentinel = "---\nname: iterion-bot-catalog\n---\nRENDERED BY ANOTHER BUILD\n"

// catalogGeneratedRel is the generated catalog's path inside a bundle.
var catalogGeneratedRel = filepath.Join("skills", "iterion-bot-catalog.md")

// catalogRegenWorkspace vendors three bundles under a fresh workspace: the
// TWO catalog owners the repo ships (whats-next and issue-triage — the
// hand-authored template plus a committed generated skill holding
// catalogSentinel) and a non-owner (review-pr, skills but no template).
// manifestNames overrides a bundle's manifest file name — how a bundle
// bot discovery does not accept as one is built. It returns the workspace
// root and the owners' names, Nexie's first.
func catalogRegenWorkspace(t *testing.T, manifestNames map[string]string) (ws string, owners []string) {
	t.Helper()
	ws = t.TempDir()
	stub := "agent x:\n  model: \"test\"\n"
	manifest := func(name string) string {
		if n, ok := manifestNames[name]; ok {
			return n
		}
		return bundle.ManifestFile
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	for _, o := range []struct{ name, persona string }{
		{"whats-next", "Nexie"},
		{"issue-triage", "Triagy"},
	} {
		dir := filepath.Join(ws, "bots", o.name)
		write(filepath.Join(dir, manifest(o.name)),
			fmt.Sprintf("name: %s\ndisplay_name: %s\ndescription: Routes work.\n", o.name, o.persona))
		write(filepath.Join(dir, bundle.MainBotFile), stub)
		write(filepath.Join(dir, "iterion-bot-catalog-static.md"),
			"---\nname: iterion-bot-catalog\n---\nPREAMBLE\n\n"+
				"<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n"+
				"<!-- ITERION:CATALOG:GENERATED:END -->\n")
		write(filepath.Join(dir, catalogGeneratedRel), catalogSentinel)
		owners = append(owners, o.name)
	}

	nonOwner := filepath.Join(ws, "bots", "review-pr")
	write(filepath.Join(nonOwner, manifest("review-pr")), "name: review-pr\ndisplay_name: Revi\ndescription: Reviewer.\n")
	write(filepath.Join(nonOwner, bundle.MainBotFile), stub)
	write(filepath.Join(nonOwner, "skills", "review.md"), "---\nname: review\ndescription: Review a diff.\n---\nBODY\n")
	return ws, owners
}

// openVendoredBundle resolves a bundle vendored in the fixture workspace
// through the real loader, so the run carries the bundle a dispatched run
// carries.
func openVendoredBundle(rel ...string) func(*testing.T, string) *bundle.Bundle {
	return func(t *testing.T, ws string) *bundle.Bundle {
		t.Helper()
		dir := filepath.Join(append([]string{ws}, rel...)...)
		b, err := bundle.OpenDir(dir)
		if err != nil {
			t.Fatalf("OpenDir %s: %v", dir, err)
		}
		return b
	}
}

// TestRunPersistWorkspace_CatalogRegenScopedToItsOwner pins which catalog
// a run-start regeneration may write. Only the run's OWN bundle skills
// reach the workspace (mirrorBundleSkills), so every other owner's
// generated catalog is a file this run has no copy of: in a git workspace
// it lands as an uncommitted edit to a tracked file whenever the running
// generator renders differently from the checked-out tree — the standing
// case for a review run, whose binary is older than the head it reviews.
// A plain .bot run — no bot id on its document, no bundle — keeps the
// workspace-wide refresh, so no operator workspace silently goes stale,
// while a BOT run holding no bundle mirrors nothing and therefore writes
// nothing; a bundle that claims a catalog the write set does not hold is
// refused out loud, never traded for a write into another bot's bundle.
func TestRunPersistWorkspace_CatalogRegenScopedToItsOwner(t *testing.T) {
	wf := &ir.Workflow{Name: "catalog-scope", Nodes: map[string]ir.Node{}}

	cases := []struct {
		name string
		// bundle backing the run; nil means no bundle is in hand. The
		// second argument is the fixture workspace.
		bundle func(*testing.T, string) *bundle.Bundle
		// the bot id the launcher recorded on the run document; "" is a
		// plain .bot launch (store.Run.BotID's own contract).
		botID string
		// manifest file name overrides for the fixture's bundles.
		manifests map[string]string
		// owners whose generated catalog this run must refresh; every
		// other owner's copy must stay byte-identical to the sentinel.
		wantRefreshed []string
		// the catalog reaches <workDir>/.claude/skills/, i.e. the run has
		// a copy of its own — asserted against its bundle's source file.
		wantMirror bool
		wantWarn   string
	}{
		{
			name:   "a non-owner bundle leaves every catalog byte-untouched",
			bundle: openVendoredBundle("bots", "review-pr"),
			botID:  "review-pr",
		},
		{
			name:          "the catalog owner's own run refreshes its own copy, and reads it",
			bundle:        openVendoredBundle("bots", "whats-next"),
			botID:         "whats-next",
			wantRefreshed: []string{"whats-next"},
			wantMirror:    true,
		},
		{
			name:          "a plain .bot run (no bot id, no bundle) keeps the workspace-wide refresh",
			wantRefreshed: []string{"whats-next", "issue-triage"},
		},
		{
			// The runner resolves no bundle when the bot id is not
			// resolvable, when opening its directory fails, and —
			// silently — when ITERION_BOTS_PATH is unset
			// (pkg/runner/loop.go's `len(BotsPaths) > 0` guard). Such a
			// run mirrors NOTHING (mirrorBundleSkills returns on a nil
			// bundle), so every owner's copy is a file it cannot read:
			// the workspace-wide refresh would be pure collateral.
			name:  "a bot run whose bundle is not in hand writes nothing",
			botID: "review-pr",
		},
		{
			name:       "an owner no discovery accepts is refused, and said",
			bundle:     openVendoredBundle("bots", "whats-next"),
			manifests:  map[string]string{"whats-next": bundle.ManifestFileAlt},
			wantMirror: true, // its own frozen copy, which this run did not rewrite
			wantWarn:   "bot catalog regen",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, owners := catalogRegenWorkspace(t, tc.manifests)
			botregistry.ClearSchemaCache()

			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			runID := "catalog-scope"
			run, err := s.CreateRun(context.Background(), runID, wf.Name, nil)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			// The bot id rides the run document, stamped by the launcher
			// that knows (runview.ResolveBotID locally, the cloud
			// publisher's queued row); the engine reads it rather than
			// re-deriving one from what it happens to hold.
			run.BotID = tc.botID
			if err := s.SaveRun(context.Background(), run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}

			var logs bytes.Buffer
			opts := []EngineOption{WithWorkDir(ws), WithLogger(iterlog.New(iterlog.LevelWarn, &logs))}
			var runBundle *bundle.Bundle
			if tc.bundle != nil {
				runBundle = tc.bundle(t, ws)
				opts = append(opts, WithBundle(runBundle))
			}
			eng := New(wf, s, nil, opts...)
			if err := eng.runPersistWorkspace(context.Background(), runID, run, false, worktreeContext{}); err != nil {
				t.Fatalf("runPersistWorkspace: %v", err)
			}

			// The verdict is asserted on EVERY owner under the workspace,
			// not only the run's own: the collateral write is the defect.
			for _, owner := range owners {
				source := filepath.Join(ws, "bots", owner, catalogGeneratedRel)
				raw, err := os.ReadFile(source)
				if err != nil {
					t.Fatalf("read %s: %v", source, err)
				}
				got := string(raw)
				if slices.Contains(tc.wantRefreshed, owner) {
					if got == catalogSentinel {
						t.Errorf("the catalog this run owns was NOT refreshed: %s still holds the committed sentinel", source)
						continue
					}
					if row := fmt.Sprintf("| %s | `%s`", personaOf(owner), owner); !strings.Contains(got, row) {
						t.Errorf("refreshed catalog %s missing %q, the persona row rendered from the live manifests:\n%s", source, row, got)
					}
					continue
				}
				if got != catalogSentinel {
					t.Errorf("this run rewrote %s, a catalog it owns no copy of (collateral edit to a tracked file of the workspace):\n%s", source, got)
				}
			}

			// What makes a write readable — or collateral: the mirror only
			// copies the run's OWN bundle skills into the workspace.
			mirrored := filepath.Join(ws, ".claude", "skills", "iterion-bot-catalog.md")
			body, readErr := os.ReadFile(mirrored)
			switch {
			case !tc.wantMirror:
				if readErr == nil {
					t.Errorf("%s was mirrored for a run whose bundle does not ship it", mirrored)
				}
			case readErr != nil:
				t.Errorf("the owner's run did not get its catalog mirrored at %s: %v", mirrored, readErr)
			default:
				own, err := os.ReadFile(filepath.Join(runBundle.Dir, catalogGeneratedRel))
				if err != nil {
					t.Fatalf("read the run's own catalog: %v", err)
				}
				if !bytes.Equal(body, own) {
					t.Errorf("the mirrored catalog differs from the run's own bundle source:\nmirrored:\n%s\nsource:\n%s", body, own)
				}
			}

			switch {
			case tc.wantWarn != "":
				if !strings.Contains(logs.String(), tc.wantWarn) {
					t.Errorf("the refused regeneration was not reported; logs:\n%s", logs.String())
				}
				if !strings.Contains(logs.String(), runBundle.Dir) {
					t.Errorf("the report does not name the bundle it refused to regenerate for; logs:\n%s", logs.String())
				}
			case strings.Contains(logs.String(), "bot catalog regen"):
				t.Errorf("unexpected catalog regen warning on a run that decided cleanly; logs:\n%s", logs.String())
			}
		})
	}
}

// personaOf maps a fixture owner to the display_name its manifest carries.
func personaOf(owner string) string {
	if owner == "whats-next" {
		return "Nexie"
	}
	return "Triagy"
}
