package runview

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestStampWatchedFromOutput_AnUntrustedRunIsSubscribedToNothing.
//
// A node's `dispatched_ids` output subscribes its run to those board issues:
// the watch coordinator then fans their transitions back to the run. That is
// the `watch.subscribe` host capability by another door, and a run whose
// workspace its tenant did not write holds none — its output (the agent's,
// reading the outsider's code) must not choose what the tenant's board feeds
// it. The engine's onNodeFinished hook is this method on every in-process
// path (launch, resume, sub-bot child), so the witness drives it directly.
func TestStampWatchedFromOutput_AnUntrustedRunIsSubscribedToNothing(t *testing.T) {
	out := map[string]any{"dispatched_ids": []any{"native:a", "native:b"}}
	for _, row := range []struct {
		name  string
		trust store.RunTrust
		want  int
	}{
		{"trusted (the control)", store.RunTrustDefault, 2},
		{"fork", store.RunTrustFork, 0},
		{"a trust this binary does not know", store.RunTrust("vendored"), 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			st, err := store.New(dir, store.WithLogger(iterlog.Nop()))
			if err != nil {
				t.Fatal(err)
			}
			const runID = "run-watch"
			if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			r, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			r.Trust = row.trust
			if err := st.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			svc, err := NewService(dir, WithLogger(iterlog.Nop()))
			if err != nil {
				t.Fatal(err)
			}
			svc.stampWatchedFromOutput(ctx, runID, "dispatch", out)
			r, err = st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(r.WatchedIssueIDs); got != row.want {
				t.Errorf("watched issues = %v, want %d", r.WatchedIssueIDs, row.want)
			}
		})
	}
}
