package runner

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"testing"
	"time"
)

func TestAdmissionStoppedReaders(t *testing.T) {
	err := fmt.Errorf("%w: running became cancelled", runtime.ErrAdmissionStopped)
	if errors.Is(err, runtime.ErrResumeSuperseded) {
		t.Fatal("new stop takes the queue rollback route")
	}
	out := classifyExecResult(err, "cas-stopped")
	t.Logf("action=%v status=%s fire=%v bankable=%v", out.action, out.finalStatus, outcomeSideEffectsFire(err, out.action), bankableStatus(out.finalStatus))
	if out.action != actionAck || outcomeSideEffectsFire(err, out.action) || bankableStatus(out.finalStatus) {
		t.Fatal("stopped admission causes outcome side effects")
	}
}

func TestAdmissionStoppedLeavesLeaseUntouched(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1})
	ctx := context.Background()
	superseded := fmt.Errorf("%w: run run-1 was queued at T2, this delivery was published at T1", runtime.ErrAdmissionStopped)

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1"}, usageWith(1.5, 900), superseded, false)

	hist, err := h.leases.ListByDonor(ctx, "donor", 10)
	if err != nil || len(hist) != 1 {
		t.Fatalf("donor history = (%d, %v), want 1 lease", len(hist), err)
	}
	if hist[0].Closed {
		t.Fatal("a superseded delivery closed the run's lease, which is the newer attempt's")
	}
	day, _, err := h.ledger.Usage(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code", ""), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 0 {
		t.Fatalf("donor charged %v by a delivery that ran nothing", day.CostUSD)
	}
}
