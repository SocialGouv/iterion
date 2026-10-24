package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

type reattachReadErrorStore struct {
	store.RunStore
	failID  string
	didFail bool
}

func (s *reattachReadErrorStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if id == s.failID && !s.didFail {
		s.didFail = true
		return nil, errors.New("temporary store read failure")
	}
	return s.RunStore.LoadRun(ctx, id)
}

// Mutation: treat a LoadRun error as a missing parent or child. The real
// subbot runner must not execute a second child while the first is unreadable.
func TestTransientReadDoesNotRespawnUndecidedChild(t *testing.T) {
	for _, failID := range []string{"parent", "child"} {
		t.Run(failID, func(t *testing.T) {
			t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "0")
			dir := t.TempDir()
			writeFile(t, dir, "child.bot", `tool create:
  command: write_file

workflow child:
  worktree: none
  sandbox: none
  entry: create
  create -> done
`)
			svc, err := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()), WithWorkDir(dir), WithSandboxDefault("none"))
			if err != nil {
				t.Fatal(err)
			}
			defer stopService(t, svc)
			ctx := context.Background()
			for _, id := range []string{"parent", "child"} {
				if _, err := svc.store.CreateRun(ctx, id, id, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.store.SetSubbotChild(ctx, "parent", "invoke", "child"); err != nil {
				t.Fatal(err)
			}
			if err := svc.store.UpdateRunStatusCoded(ctx, "child", store.RunStatusFailedResumable, "command possibly still running", store.FailureAmbiguousEffect); err != nil {
				t.Fatal(err)
			}
			original := svc.store
			svc.store = &reattachReadErrorStore{RunStore: original, failID: failID}
			_, runErr := svc.subbotRunnerFor(filepath.Join(dir, "parent.bot"), iterlog.Nop())(ctx, runtime.SubbotRequest{Source: "child.bot", Vars: map[string]any{"path": "duplicate.txt", "content": "fresh child effect"}, ParentRunID: "parent", NodeID: "invoke", ReattachKey: "invoke", WorkDir: dir})
			if runErr == nil {
				t.Error("a failed reattachment read returns success")
			}
			parent, err := original.LoadRun(ctx, "parent")
			if err != nil {
				t.Fatal(err)
			}
			if parent.SubbotChildren["invoke"] != "child" {
				t.Error("a read failure clears the child binding")
			}
			if data, err := os.ReadFile(filepath.Join(dir, "duplicate.txt")); !os.IsNotExist(err) {
				t.Fatalf("fresh child executes after %s read error despite persisted AMBIGUOUS_EFFECT: output=%q runErr=%v", failID, data, runErr)
			}
		})
	}
}
