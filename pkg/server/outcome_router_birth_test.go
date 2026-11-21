package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// swapBeforeLoadStore runs swap right before the first LoadRun after arming
// reads, then delegates unchanged; it applies no identity policy of its own.
type swapBeforeLoadStore struct {
	store.RunStore
	swap func()
}

func (s *swapBeforeLoadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if hook := s.swap; hook != nil {
		s.swap = nil
		hook()
	}
	return s.RunStore.LoadRun(ctx, id)
}

// The router decides on run A (its auto_merge intent) and the merge reloads
// the run by ID. B — no auto_merge, the same episode number — recreated under
// the ID before that load is never merged by A's decision: the merge expects
// A's birth and refuses B, and B's target branch does not move.
func TestOutcomeRouter_MergeLandsOnlyOnTheDecidedBirth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRouterHarness(t)
	ctx := context.Background()
	const id = "decided-birth"
	a := intentBankedRun(t, h, id, nil)
	spy := &swapBeforeLoadStore{RunStore: h.st}
	h.s.runs = newTestRunviewService(t, "", runview.WithLogger(iterlog.Nop()), runview.WithStore(spy))
	var b *store.Run
	var bMain string
	spy.swap = func() {
		if err := h.st.DeleteRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b = intentBankedRun(t, h, id, func(r *store.Run) { r.AutoMerge = false })
		bMain = strings.TrimSpace(gittest.Run(t, b.RepoRoot, "rev-parse", "main"))
	}
	h.s.routeOutcomeOffer(ctx, id)
	if b == nil {
		t.Fatalf("fixture: the swap never ran (decisions %+v)", h.decisions(t, id))
	}
	if b.BirthID == a.BirthID || b.OutcomeSeq != a.OutcomeSeq || b.AutoMerge {
		t.Fatalf("fixture: A birth %q seq %d, B birth %q seq %d auto_merge %t", a.BirthID, a.OutcomeSeq, b.BirthID, b.OutcomeSeq, b.AutoMerge)
	}
	got, err := h.st.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.BirthID != b.BirthID || got.MergeStatus == store.MergeStatusMerged || got.MergedCommit != "" {
		t.Fatalf("A's routing decision merged successor B (auto_merge=false): stored birth %q merge_status %q merged_commit %q", got.BirthID, got.MergeStatus, got.MergedCommit)
	}
	if head := strings.TrimSpace(gittest.Run(t, b.RepoRoot, "rev-parse", "main")); head != bMain {
		t.Fatalf("A's routing decision moved B's target branch: main %s -> %s", bMain, head)
	}
}
