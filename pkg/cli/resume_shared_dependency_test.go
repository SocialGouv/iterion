package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResume_aSharedDependencyChangeComesAfterTheScratchLoss: `iterion
// resume` of a shared dependency's exported workflow whose dependency changed
// since the run started (a file other than the workflow). With a scratch that
// did not travel, the scratch is refused first and names the change: the CLI
// does not refuse the change before the engine judges the loss. Without a
// loss the change is still refused, and --force goes through.
func TestResume_aSharedDependencyChangeComesAfterTheScratchLoss(t *testing.T) {
	hermeticSandbox(t)
	t.Chdir(gittest.SourceRepo(t))
	for _, tc := range []struct {
		name string
		lost bool
	}{{"a scratch that did not travel", true}, {"no loss", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			bdir := filepath.Join(dir, "shared")
			child := filepath.Join(bdir, "workflows", "child.bot")
			if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string]string{
				filepath.Join(bdir, "main.bot"):      "\nworkflow main:\n  entry: done\n",
				child:                                "\nworkflow child:\n  entry: done\n",
				filepath.Join(bdir, "README.md"):     "v1\n",
				filepath.Join(bdir, "manifest.yaml"): "name: shared-planner\nversion: 1.0.0\nschema_version: 1\nexports:\n  workflows:\n    - id: child\n      path: workflows/child.bot\n",
			} {
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			b, err := bundle.OpenDir(bdir)
			if err != nil {
				t.Fatal(err)
			}
			bhash, err := bundle.ContentHashDir(bdir)
			if err != nil {
				t.Fatal(err)
			}
			_, hash, err := runview.CompileBundleWorkflow(child, b)
			if err != nil {
				t.Fatal(err)
			}

			storeDir := filepath.Join(dir, "store")
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.CreateRun(ctx, "shared-child", "child", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SaveCheckpoint(ctx, r.ID, &store.Checkpoint{NodeID: "done"}); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateRunStatus(ctx, r.ID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
				t.Fatal(err)
			}
			if r, err = s.LoadRun(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			r.WorkflowHash = hash
			r.FilePath = child
			r.BundlePath = bdir
			r.BundleHash = bhash
			r.BundleName = b.Manifest.Name
			r.BundleVersion = b.Manifest.Version
			r.BundleWorkflow = "child"
			if err := s.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			if tc.lost {
				if _, err := s.AppendEvent(ctx, r.ID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
					"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
				}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(bdir, "README.md"), []byte("v2\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			p, _ := newTestPrinter(cli.OutputHuman)
			err = cli.RunResumeWithFile(ctx, child, cli.ResumeOptions{RunID: r.ID, StoreDir: storeDir}, p)
			if tc.lost {
				var rt *runtime.RuntimeError
				if !errors.As(err, &rt) || rt.Code != runtime.ErrCodeScratchNotPortable || !strings.Contains(err.Error(), "the workflow source has also changed") {
					t.Fatalf("resume over a changed dependency and a lost scratch: %v, want SCRATCH_NOT_PORTABLE first, naming the change", err)
				}
				return
			}
			if !runtime.IsWorkflowSourceChanged(err) {
				t.Fatalf("resume over a changed dependency: %v, want the source refusal", err)
			}
			if err := cli.RunResumeWithFile(ctx, child, cli.ResumeOptions{RunID: r.ID, StoreDir: storeDir, Force: true}, p); err != nil {
				t.Fatalf("the forced resume: %v", err)
			}
			if r, err = s.LoadRun(ctx, r.ID); err != nil || r.Status != store.RunStatusFinished {
				t.Fatalf("the forced resume left the run %v (%v), want finished", r.Status, err)
			}
		})
	}
}
