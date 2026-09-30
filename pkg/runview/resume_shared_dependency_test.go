package runview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// namesSourceChange reports whether a refusal also names a source change.
func namesSourceChange(err error) bool {
	return err != nil && strings.Contains(err.Error(), "the workflow source has also changed")
}

// sharedDependency writes a bundle that exports the workflow "child" and
// returns its dir and the export's path.
func sharedDependency(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shared")
	child := filepath.Join(dir, "workflows", "child.bot")
	if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(dir, "main.bot"):      "\nworkflow main:\n  entry: done\n",
		child:                               "\nworkflow child:\n  entry: done\n",
		filepath.Join(dir, "README.md"):     "v1\n",
		filepath.Join(dir, "manifest.yaml"): "name: shared-planner\nversion: 1.0.0\nschema_version: 1\nexports:\n  workflows:\n    - id: child\n      path: workflows/child.bot\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, child
}

// TestResume_aSharedDependencyChangeComesAfterTheLossItWouldWaive: a run of a
// shared dependency's exported workflow whose scratch — or whose lineage —
// does not travel, and whose dependency changed since it started (a file
// other than the workflow). Both surfaces refuse the loss first and name the
// change, and nothing is published: a force offered for the change alone
// would waive a loss the operator was never shown. A digest the source check
// accepts as legacy does not hide the change. Without a loss the change is
// refused; forced, the resume goes through.
func TestResume_aSharedDependencyChangeComesAfterTheLossItWouldWaive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		loss   string
		legacy bool
	}{
		{"a scratch that did not travel", "scratch", false},
		{"a lineage that does not travel", "lineage", false},
		{"a scratch, under a legacy digest", "scratch", true},
		{"no loss", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bdir, child := sharedDependency(t)
			opened, err := bundle.OpenDir(bdir)
			if err != nil {
				t.Fatal(err)
			}
			bhash, err := bundle.ContentHashDir(bdir)
			if err != nil {
				t.Fatal(err)
			}
			_, cs, _, err := compileForLaunch(child, "", bdir)
			if err != nil {
				t.Fatalf("compileForLaunch: %v", err)
			}
			hash := cs.Hash
			if tc.legacy {
				src, err := os.ReadFile(opened.IterPath)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(src)
				hash = hex.EncodeToString(sum[:])
			}
			publisher := &operatorResumePublisher{}
			svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(publisher))
			if err != nil {
				t.Fatal(err)
			}
			const runID = "run-shared-child"
			seedPausedOperatorRun(t, svc, runID, hash)
			ctx := context.Background()
			r, err := svc.store.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			r.FilePath = child
			r.BundlePath = bdir
			r.BundleHash = bhash
			r.BundleName = opened.Manifest.Name
			r.BundleVersion = opened.Manifest.Version
			r.BundleWorkflow = "child"
			if tc.loss == "lineage" {
				r.ParentRunID = "run-parent"
			}
			if err := svc.store.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			switch tc.loss {
			case "scratch":
				_, err = svc.store.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
					"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
				}})
			case "lineage":
				_, err = svc.store.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxShared, Data: map[string]any{
					"adopted": true, "driver": "kubernetes", "parent_run": "run-parent", "copy_based": false, "scratch_container_local": true,
				}})
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				if err := svc.PreflightResume(ctx, ResumeSpec{RunID: runID, FilePath: child}); !scratchRefused(err) || namesSourceChange(err) {
					t.Fatalf("precondition: the legacy digest with the dependency unchanged: %v, want SCRATCH_NOT_PORTABLE naming no change", err)
				}
			}

			if err := os.WriteFile(filepath.Join(bdir, "README.md"), []byte("v2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			pfErr := svc.PreflightResume(ctx, ResumeSpec{RunID: runID, FilePath: child})
			_, rErr := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: child})
			if publisher.resumeCalls != 0 {
				t.Fatalf("an unforced resume over a changed dependency published %d", publisher.resumeCalls)
			}
			for surface, err := range map[string]error{"PreflightResume": pfErr, "Resume": rErr} {
				if tc.loss == "" {
					if !runtime.IsWorkflowSourceChanged(err) {
						t.Fatalf("%s over a changed dependency: %v, want the source refusal", surface, err)
					}
					continue
				}
				if !scratchRefused(err) || runtime.IsWorkflowSourceChanged(err) || !namesSourceChange(err) {
					t.Fatalf("%s over a changed dependency and a %s that does not travel: %v, want SCRATCH_NOT_PORTABLE first, naming the change", surface, tc.loss, err)
				}
			}
			if tc.loss != "" {
				if err := svc.PreflightResume(ctx, ResumeSpec{RunID: runID, FilePath: child, Force: true}); !scratchRefused(err) {
					t.Fatalf("a preflight forced for the change alone: %v, want the %s's loss still refused", err, tc.loss)
				}
				if _, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: child, Force: true}); !scratchRefused(err) || publisher.resumeCalls != 0 {
					t.Fatalf("a resume forced for the change alone: %v, published %d, want the %s's loss still refused", err, publisher.resumeCalls, tc.loss)
				}
			}
			if _, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: child, Force: true, AcceptScratchLoss: true}); err != nil || publisher.resumeCalls != 1 {
				t.Fatalf("the resume given both consents: %v, published %d, want it through", err, publisher.resumeCalls)
			}
		})
	}
}
