package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestReconcileStalled_AwaitAnswersNeedsLiveProof(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInteraction(ctx, &store.Interaction{ID: "q", RunID: "r", NodeID: "ask", Kind: store.InteractionKindAsync, RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	c := newStallTestDispatcher(t, dir)
	for _, tc := range []struct {
		name       string
		wait       *store.AwaitAnswersWait
		wantCancel bool
	}{
		{"pending question only", nil, true},
		{"parked sync point", &store.AwaitAnswersWait{NodeID: "sync", Until: time.Now().Add(time.Hour)}, false},
		{"expired sync point", &store.AwaitAnswersWait{NodeID: "sync", Until: time.Now().Add(-time.Hour)}, true},
		{"cleared sync point", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SetAwaitAnswersWait(ctx, "r", "invocation", tc.wait); err != nil {
				t.Fatal(err)
			}
			cancelled := seedStalledEntry(c, "fake:wait", "r")
			c.reconcileStalled(ctx, c.cfg.Load())
			if *cancelled != tc.wantCancel {
				t.Fatalf("cancelled=%v, want %v", *cancelled, tc.wantCancel)
			}
		})
	}
}
