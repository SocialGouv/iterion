package runview

import (
	"context"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A delete tombstone is read everywhere as PROOF the run is gone (the
// board launch authorities admit a fresh run on it) — so deleting a run
// that is still ALIVE must be refused at the one choke both the HTTP
// handler and the MCP/CLI escape hatch cross. The studio only disabled
// the button; the API accepted it.
func TestDeleteRunCtx_RefusesALiveRun(t *testing.T) {
	dir := t.TempDir()
	rs, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{store: rs}
	ctx := context.Background()
	mk := func(id string, status store.RunStatus) {
		t.Helper()
		if _, err := rs.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatal(err)
		}
		r, _ := rs.LoadRun(ctx, id)
		r.Status = status
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, alive := range []store.RunStatus{
		store.RunStatusRunning, store.RunStatusQueued,
		store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator,
	} {
		id := "run-" + string(alive)
		mk(id, alive)
		err := svc.DeleteRunCtx(ctx, id)
		if err == nil {
			t.Fatalf("deleted a %s run — its tombstone lets a second run launch while the engine keeps burning", alive)
		}
		if !strings.Contains(err.Error(), "cancel it first") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
	}
	// Terminal runs stay deletable — the guard must not close the
	// documented cleanup path.
	mk("run-done", store.RunStatusFinished)
	if err := svc.DeleteRunCtx(ctx, "run-done"); err != nil {
		t.Fatalf("a finished run must stay deletable: %v", err)
	}
}
