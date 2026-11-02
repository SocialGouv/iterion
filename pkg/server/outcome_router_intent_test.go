package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The launch intent (auto_merge) as a routing carrier: a run launched to
// land is LANDED by the instance, without a contract and without a
// session — the measured incident this closes (a campaign lot whose own
// gate said converged waited 6-9h for a human to press merge).

func intentBankedRun(t *testing.T, h *routerHarness, id string, mut func(r *store.Run)) *store.Run {
	t.Helper()
	// The bank fixture, the I1 shape: a repo, a run branch one commit
	// ahead of the target, HEAD back on the target.
	repo := t.TempDir()
	gittest.Run(t, repo, "init", "-q", "-b", "main")
	// Repo-local identity: PerformMergeCtx's squash commit runs without
	// the helper's env, and CI has no global git identity.
	gittest.Run(t, repo, "config", "user.email", "t@t.t")
	gittest.Run(t, repo, "config", "user.name", "t")
	gittest.Run(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "add", "f.txt")
	gittest.Run(t, repo, "commit", "-qm", "base")
	gittest.Run(t, repo, "checkout", "-qb", "iterion/run/"+id)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "commit", "-qam", "work")
	sha := gittest.Run(t, repo, "rev-parse", "HEAD")
	gittest.Run(t, repo, "checkout", "-q", "main")

	return h.seedRun(t, id, func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.Worktree = true
		r.RepoRoot = repo
		r.WorkDir = repo
		r.FinalBranch = "iterion/run/" + id
		r.FinalCommit = sha
		r.MergeStrategy = store.MergeStrategySquash
		r.AutoMerge = true
		if mut != nil {
			mut(r)
		}
	})
}

// The witness: a run with NO contract, carrying only the launch intent,
// is LANDED by the instance — merged into the target branch through the
// claimed merge path, the registry recording one intent-sourced row.
// Revert the offer gate (or the intent reading) and the run sits merged-
// pending forever, exactly the night the campaign lots waited 6-9h.
func TestOutcomeRouter_LandsAnAutoMergeRunByItsLaunchIntent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRouterHarness(t)
	ctx := context.Background()

	r := intentBankedRun(t, h, "i5-intent", nil)

	h.s.routeOutcomeOffer(ctx, r.ID)

	got, _ := h.st.LoadRun(ctx, r.ID)
	if got.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("intent run MergeStatus=%q, want merged (decisions: %+v)", got.MergeStatus, h.decisions(t, r.ID))
	}
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 {
		t.Fatalf("intent landing minted %d rows, want 1: %+v", len(ds), ds)
	}
	d := ds[0]
	if d.Decision != "merge" || d.State != store.RouteDecisionSucceeded {
		t.Fatalf("intent decision row = %+v, want merge/succeeded", d)
	}
	if d.PolicyHash != "" {
		t.Fatalf("an intent row must pin no contract hash, got %q", d.PolicyHash)
	}
	if !strings.Contains(d.Reason, "launch intent auto_merge") {
		t.Fatalf("intent row reason = %q, want the intent named", d.Reason)
	}

	// A double offer of the SAME episode is one read, zero new rows —
	// the claim's idempotence covers the intent carrier too.
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 1 {
		t.Fatalf("double offer minted a second row: %+v", ds)
	}
}

// The intent grants landing ONLY: a failed auto_merge run is declined in
// silence — no merge, no row, no escalation the intent never carried
// (its continuation — retry, redelivery, operator — already owns it).
func TestOutcomeRouter_AFailedIntentRunIsLeftAlone(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()

	h.seedRun(t, "i6-intent-failed", func(r *store.Run) {
		r.Status = store.RunStatusFailed
		r.AutoMerge = true
		r.FinalBranch, r.FinalCommit = "iterion/run/i6", "abc"
	})

	h.s.routeOutcomeOffer(ctx, "i6-intent-failed")

	got, _ := h.st.LoadRun(ctx, "i6-intent-failed")
	if got.MergeStatus == store.MergeStatusMerged {
		t.Fatalf("a failed intent run must never be merged, got %q", got.MergeStatus)
	}
	if ds := h.decisions(t, "i6-intent-failed"); len(ds) != 0 {
		t.Fatalf("a failed intent run must leave no row, got %+v", ds)
	}
}

// An explicit merge_into "none" is the operator's opt-out: the intent
// must not override it (the engine's own finalization skips the merge
// for the same launch), so the router declines without a row.
func TestOutcomeRouter_TheIntentHonorsAnExplicitNone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRouterHarness(t)
	ctx := context.Background()

	intentBankedRun(t, h, "i7-intent-none", func(r *store.Run) {
		r.MergeInto = "none"
	})

	h.s.routeOutcomeOffer(ctx, "i7-intent-none")

	got, _ := h.st.LoadRun(ctx, "i7-intent-none")
	if got.MergeStatus == store.MergeStatusMerged {
		t.Fatalf("a merge_into=none intent run must never be merged, got %q", got.MergeStatus)
	}
	if ds := h.decisions(t, "i7-intent-none"); len(ds) != 0 {
		t.Fatalf("an opted-out intent run must leave no row, got %+v", ds)
	}
}
