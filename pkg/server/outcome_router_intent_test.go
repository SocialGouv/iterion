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

// The probe, replayed as a witness: a run whose LOCAL finalize refused
// to merge (a wip-banked HEAD is recorded merge_status=skipped — the
// bank commit is unreviewed output the run itself chose not to commit)
// is LANDED anyway by the router on the intent alone. The refusal
// binds: the router declines, the banked work stays on its storage
// branch for the operator to review. The same refusal with no recorded
// target at all (a detached-HEAD child) must not grow one at merge
// time — whatever the repo root happens to have checked out.
func TestOutcomeRouter_ARefusedFinalizeIsNeverLandedByItsIntent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRouterHarness(t)
	ctx := context.Background()

	intentBankedRun(t, h, "i8-intent-wip-banked", func(r *store.Run) {
		r.MergeStatus = store.MergeStatusSkipped
	})
	intentBankedRun(t, h, "i8-intent-detached", func(r *store.Run) {
		r.MergeInto = ""
		r.MergeStatus = store.MergeStatusSkipped
	})

	h.s.routeOutcomeOffer(ctx, "i8-intent-wip-banked")
	h.s.routeOutcomeOffer(ctx, "i8-intent-detached")

	for _, id := range []string{"i8-intent-wip-banked", "i8-intent-detached"} {
		got, _ := h.st.LoadRun(ctx, id)
		if got.MergeStatus == store.MergeStatusMerged {
			t.Fatalf("run %s: a skipped finalize must never be landed by the intent, got merged", id)
		}
		if ds := h.decisions(t, id); len(ds) != 0 {
			t.Fatalf("run %s: the declined refusal must leave no row, got %+v", id, ds)
		}
	}
}

// The contract lane shares the refusal: success_when held on a skipped
// run escalates instead of merging — the intent only declines in
// silence, a contract is audible. The row is the escalation record.
func TestOutcomeRouter_AContractCannotLandARefusedFinalize(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRouterHarness(t)
	ctx := context.Background()

	r := intentBankedRun(t, h, "i9-contract-skipped", func(r *store.Run) {
		r.AutoMerge = false
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
		r.MergeStatus = store.MergeStatusSkipped
	})

	h.s.routeOutcomeOffer(ctx, r.ID)

	got, _ := h.st.LoadRun(ctx, r.ID)
	if got.MergeStatus == store.MergeStatusMerged {
		t.Fatalf("a skipped finalize must never be landed by a contract, got merged")
	}
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 {
		t.Fatalf("the refused merge must escalate audibly, got %d rows: %+v", len(ds), ds)
	}
	if ds[0].Decision != "escalate" || ds[0].State != store.RouteDecisionSucceeded {
		t.Fatalf("refusal row = %+v, want escalate/succeeded", ds[0])
	}
	if !strings.Contains(ds[0].Reason, "skipped") {
		t.Fatalf("refusal row reason = %q, want the refusal named", ds[0].Reason)
	}
}
