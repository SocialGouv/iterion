package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// routerHarness wires the minimum a routing pass needs: a real FS store
// (it carries the decision-registry capability) and a real runview
// service over the same store (so a merge decision performs a REAL
// claimed merge).
type routerHarness struct {
	s   *Server
	st  *store.FilesystemRunStore
	dir string
}

func newRouterHarness(t *testing.T) *routerHarness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "store"), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := runview.NewService("", runview.WithLogger(iterlog.Nop()), runview.WithStore(st))
	if err != nil {
		t.Fatalf("runview.NewService: %v", err)
	}
	t.Setenv(outcomeRouterEnv, "on")
	s := newOrgTestServer(t)
	s.cfg.Store = st
	s.runs = svc
	// A router active since well before the fixtures: the activation
	// watermark bounds the sweep, and several tests age their runs into
	// the past to clear the sweep grace — those instants must stay above
	// it. Written directly (the ageRun pattern); EnsureRouterWatermark
	// reads it back.
	wm, _ := json.Marshal(map[string]time.Time{"activated_at": time.Now().Add(-48 * time.Hour).UTC()})
	if err := os.WriteFile(filepath.Join(dir, "store", "router_watermark.json"), wm, 0o644); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	return &routerHarness{s: s, st: st, dir: filepath.Join(dir, "store")}
}

// patchRunDoc rewrites fields of the run's persisted document directly
// in the store layout. No API does this, which is exactly the point:
// each caller needs a shape the store's own writers refuse to produce —
// a document that has NOT moved (the sweep must still find it), a
// terminal in the past, or the pre-episode zero counter that SaveRun
// always stamps over.
func (h *routerHarness) patchRunDoc(t *testing.T, id string, fields map[string]any) {
	t.Helper()
	p := filepath.Join(h.dir, "runs", id, "run.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse run.json: %v", err)
	}
	for k, v := range fields {
		doc[k] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal run.json: %v", err)
	}
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}
}

// ageRun backdates updated_at — what the sweep window filters on.
func (h *routerHarness) ageRun(t *testing.T, id string, to time.Time) {
	t.Helper()
	h.patchRunDoc(t, id, map[string]any{"updated_at": to.UTC().Format(time.RFC3339Nano)})
}

// ageTerminal backdates BOTH stamps that say how long ago a run reached
// its terminal: updated_at and finished_at (what the bank deadline
// measures from).
func (h *routerHarness) ageTerminal(t *testing.T, id string, to time.Time) {
	t.Helper()
	stamp := to.UTC().Format(time.RFC3339Nano)
	h.patchRunDoc(t, id, map[string]any{"updated_at": stamp, "finished_at": stamp})
}

// zeroOutcomeSeq forces the pre-episode shape (a terminal written by a
// binary that had no episode bookkeeping). Unreachable through the store
// API: SaveRun reads a status change as a transition and stampOutcome
// increments OutcomeSeq for every terminal status.
func (h *routerHarness) zeroOutcomeSeq(t *testing.T, id string) {
	t.Helper()
	h.patchRunDoc(t, id, map[string]any{"outcome_seq": 0})
	got, err := h.st.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("reload after zeroing outcome_seq: %v", err)
	}
	if got.OutcomeSeq != 0 {
		t.Fatalf("outcome_seq rewrite did not take (got %d) — the guard under test would not be exercised", got.OutcomeSeq)
	}
}

// restoreOutcomeSeq puts the episode back, so a test can prove its
// silence came from the zero-seq guard and not from the run being
// unroutable for some other reason.
func (h *routerHarness) restoreOutcomeSeq(t *testing.T, id string, seq int64) {
	t.Helper()
	h.patchRunDoc(t, id, map[string]any{"outcome_seq": seq})
}

// seedRun creates a terminal run with the given shape. outputs nil ⇒ no
// checkpoint.
func (h *routerHarness) seedRun(t *testing.T, id string, mut func(r *store.Run)) *store.Run {
	t.Helper()
	ctx := context.Background()
	if _, err := h.st.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := h.st.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	mut(r)
	if err := h.st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	// Reload: SaveRun stamps the outcome bookkeeping (status change ⇒
	// episode) on the persisted copy, not the argument.
	r, err = h.st.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return r
}

func mergePolicy(actions ...string) *store.RoutingPolicy {
	p := &store.RoutingPolicy{
		Version:        1,
		SuccessWhen:    "outputs.gate.converged",
		BlockWhen:      []string{"outputs.gate.blocked"},
		AllowedActions: actions,
	}
	p.Hash = p.ComputeHash()
	return p
}

func outputs(converged, blocked bool) *store.Checkpoint {
	return &store.Checkpoint{Outputs: map[string]map[string]any{
		"gate": {"converged": converged, "blocked": blocked},
	}}
}

func (h *routerHarness) decisions(t *testing.T, id string) []store.RouteDecision {
	t.Helper()
	ds, err := h.st.ListRouteDecisions(context.Background(), id)
	if err != nil {
		t.Fatalf("ListRouteDecisions: %v", err)
	}
	return ds
}

// The six measured incidents, replayed as fixtures against the decision
// path (F9: the shadow-mode comparison was rejected — the router is
// validated against an independent truth, these incidents ARE it).
func TestOutcomeRouter_IncidentFixtures(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()

	// I3 — a redelivery already owns the run (the wall-redelivery
	// incident): the router must not even leave a row.
	r := h.seedRun(t, "i3-redelivery", func(r *store.Run) {
		r.Status = store.RunStatusFailedResumable
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
	})
	// Stamp the continuation the way the engine's interruption path does.
	if _, err := h.st.UpdateRunOutcome(ctx, r.ID, store.RunStatusFailedResumable, "interrupted",
		store.RunOutcomeMeta{Code: store.FailureInterrupted, Continuation: store.ContinuationRedeliveryPending}, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("I3: a pending redelivery must be left alone, got %+v", ds)
	}

	// I4 — converged but carrying an explicit blocker (the landing that
	// reddened the base): escalate, never merge.
	r = h.seedRun(t, "i4-blocked", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, true)
		r.FinalBranch, r.FinalCommit = "iterion/run/i4", "abc"
	})
	h.s.routeOutcomeOffer(ctx, r.ID)
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].Decision != "escalate" || !strings.Contains(ds[0].Reason, "block_when[0] held") {
		t.Fatalf("I4: want one escalate row on the blocker, got %+v", ds)
	}
	if got, _ := h.st.LoadRun(ctx, r.ID); got.MergeStatus == store.MergeStatusMerged {
		t.Fatal("I4: the blocked run was merged")
	}

	// I5 — success claimed but the bank is empty (work_gate passed,
	// store empty): never merged, and — per the adversarial review —
	// never DECIDED either: the terminal status lands before the bank
	// push, so consuming the episode here would burn it while a
	// legitimate branch is still on its way. Silence; an explicit bank
	// ERROR (FinalBranchError) escalates instead.
	r = h.seedRun(t, "i5-empty-bank", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
	})
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds = h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("I5: an empty bank must not consume the episode, got %+v", ds)
	}
	if got, _ := h.st.LoadRun(ctx, r.ID); got.MergeStatus == store.MergeStatusMerged {
		t.Fatal("I5: the bankless run was merged")
	}
	r = h.seedRun(t, "i5b-bank-error", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
		r.FinalBranchError = "branch create failed: reflog only"
	})
	h.s.routeOutcomeOffer(ctx, r.ID)
	ds = h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].Decision != "escalate" || !strings.Contains(ds[0].Reason, "bank recorded an error") {
		t.Fatalf("I5b: want escalate on the recorded bank error, got %+v", ds)
	}

	// I6 — an interrupted run whose STALE checkpoint still shows green
	// gates (the autoscaler kill): the status gate demotes merge.
	r = h.seedRun(t, "i6-stale-gates", func(r *store.Run) {
		r.Status = store.RunStatusFailedResumable
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
		r.FinalBranch, r.FinalCommit = "iterion/run/i6", "abc"
		r.ContinuationState = store.ContinuationFinal
	})
	if _, err := h.st.UpdateRunOutcome(ctx, r.ID, store.RunStatusFailedResumable, "orphaned",
		store.RunOutcomeMeta{Code: store.FailureProcessOrphaned, Continuation: store.ContinuationFinal}, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	h.s.routeOutcomeOffer(ctx, r.ID)
	ds = h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].Decision != "escalate" || !strings.Contains(ds[0].Reason, "earlier pass") {
		t.Fatalf("I6: want escalate on stale outputs, got %+v", ds)
	}

	// Operator cancel: never auto-routed, no row.
	r = h.seedRun(t, "cancelled", func(r *store.Run) {
		r.Status = store.RunStatusCancelled
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
	})
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("cancelled must never be routed, got %+v", ds)
	}
}

// I1 — the converged run that waited overnight: with a contract it
// MERGES, via the claimed merge path, and the registry records it.
// Also I2's shape: a SECOND episode gets its own independent decision.
func TestOutcomeRouter_MergesAndCountsEpisodes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRouterHarness(t)
	ctx := context.Background()

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	// Repo-local identity: PerformMergeCtx's squash commit runs without
	// the helper's env, and CI has no global git identity.
	git("config", "user.email", "t@t.t")
	git("config", "user.name", "t")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f.txt")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "iterion/run/i1")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-qam", "work")
	sha := func() string {
		out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}()
	git("checkout", "-q", "main")

	r := h.seedRun(t, "i1-converged", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.Worktree = true
		r.RepoRoot = repo
		r.WorkDir = repo
		r.FinalBranch = "iterion/run/i1"
		r.FinalCommit = sha
		r.MergeStrategy = store.MergeStrategySquash
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
	})
	if r.OutcomeSeq == 0 {
		t.Fatalf("seed must have an episode, got %+v", r.OutcomeSeq)
	}

	h.s.routeOutcomeOffer(ctx, r.ID)

	got, _ := h.st.LoadRun(ctx, r.ID)
	if got.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("I1: MergeStatus=%q, want merged (decisions: %+v)", got.MergeStatus, h.decisions(t, r.ID))
	}
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].Decision != "merge" || ds[0].State != store.RouteDecisionSucceeded {
		t.Fatalf("I1: registry = %+v", ds)
	}
	if ds[0].PolicyHash != got.RoutingPolicy.Hash {
		t.Fatalf("I1: decision must pin the contract hash")
	}

	// A double offer of the SAME episode is one read, zero new rows.
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 1 {
		t.Fatalf("double offer minted a second row: %+v", ds)
	}
}

// A contract-decided merge that hits a CONTENT CONFLICT must stop, not
// retry. PerformMergeCtx persists merge_status=conflicted and leaves the
// tree conflicted on purpose so the resolver UI can take over — but a
// conflicted run stays mergeClaimable, so a retryable "failed" row would
// have the next sweep run `git merge --squash` against the conflicted
// index, fail on a non-conflict error, and overwrite merge_status with
// "failed" — the exact status requireConflict needs. The operator's only
// path out of the conflict would vanish, unattended, in 60 seconds.
func TestOutcomeRouter_MergeConflictIsNotRetried(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRouterHarness(t)
	ctx := context.Background()
	sink := &advSink{}
	h.s.opsAlerts = &alert.OpsDispatcher{Sinks: []alert.Sink{sink}, Logger: iterlog.Nop()}

	// A repo where main and the run's branch changed the SAME line: the
	// squash merge cannot apply cleanly.
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t.t")
	git("config", "user.name", "t")
	git("config", "commit.gpgsign", "false")
	write("base\n")
	git("add", "f.txt")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "iterion/run/conflict")
	write("from the run\n")
	git("commit", "-qam", "run work")
	sha := func() string {
		out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}()
	git("checkout", "-q", "main")
	write("from main\n")
	git("commit", "-qam", "divergent")

	r := h.seedRun(t, "merge-conflict", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.Worktree = true
		r.RepoRoot = repo
		r.WorkDir = repo
		r.FinalBranch = "iterion/run/conflict"
		r.FinalCommit = sha
		r.MergeStrategy = store.MergeStrategySquash
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
	})

	h.s.routeOutcomeOffer(ctx, r.ID)
	got, _ := h.st.LoadRun(ctx, r.ID)
	if got.MergeStatus != store.MergeStatusConflicted {
		t.Fatalf("setup: want merge_status=conflicted after the router's merge, got %q (decisions %+v)", got.MergeStatus, h.decisions(t, r.ID))
	}
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].State != store.RouteDecisionRequiresAction {
		t.Fatalf("a content conflict must finish requires_action, got %+v", ds)
	}
	if !strings.Contains(ds[0].Error, "conflict") {
		t.Fatalf("the row must name the conflict: %+v", ds[0])
	}

	// The regression itself: further offers — the sweep runs one every
	// 60s — must neither re-claim nor touch the conflicted status.
	h.ageRun(t, r.ID, time.Now().Add(-routerSweepGrace-time.Minute))
	for i := 0; i < 3; i++ {
		h.s.outcomeRouterSweepPass(ctx)
	}
	if ds := h.decisions(t, r.ID); len(ds) != 1 || ds[0].State != store.RouteDecisionRequiresAction || ds[0].Attempts != 1 {
		t.Fatalf("requires_action must be terminal for the sweep, got %+v", ds)
	}
	got, _ = h.st.LoadRun(ctx, r.ID)
	if got.MergeStatus != store.MergeStatusConflicted {
		t.Fatalf("the sweep destroyed the conflict resolver's status: merge_status=%q", got.MergeStatus)
	}
	// And a settled episode leaves the sweep list rather than clogging it.
	ids, err := h.st.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == r.ID {
			t.Fatalf("requires_action run still occupies the sweep batch: %v", ids)
		}
	}
	if len(sink.kinds()) == 0 {
		t.Fatal("a conflict the router cannot resolve must reach the operator")
	}
}

// A run without the episode bookkeeping (outcome_seq 0 — a terminal
// written by a pre-episode binary) is left alone: no stable claim key
// exists, so the registry cannot make the action idempotent and the
// router must not act unclaimed.
//
// The shape is unreachable through the store API — SaveRun treats the
// status change as a transition and stampOutcome increments OutcomeSeq
// for every terminal — so the counter is zeroed by a raw run.json
// rewrite, the same way ageRun backdates a timestamp. The earlier
// version of this test tried to reach it through SaveRun, always
// observed seq 1 and returned before its only assertion, so it could
// never fail; and its claim that the guard was "covered by the queued-run
// path in the fixtures test" did not hold either — every run there goes
// terminal through seedRun and so carries seq >= 1.
func TestOutcomeRouter_NoEpisodeNoAction(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	// Everything else about this run says "route me": finished, a merge
	// contract, converged gates, a banked branch. Only the episode
	// counter is missing.
	r := h.seedRun(t, "no-episode", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
		r.FinalBranch, r.FinalCommit = "iterion/run/ne", "abc"
	})
	if r.OutcomeSeq == 0 {
		t.Fatalf("seed should have stamped an episode; the rewrite below would be a no-op")
	}
	h.zeroOutcomeSeq(t, r.ID)

	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("a zero-seq run must be left alone, got %+v", ds)
	}
	// Through the real sweep too — its window is where such a run would
	// otherwise be re-offered every 60s.
	h.ageTerminal(t, r.ID, time.Now().Add(-routerSweepGrace-time.Minute))
	h.zeroOutcomeSeq(t, r.ID)
	h.s.outcomeRouterSweepPass(ctx)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("the sweep acted on a zero-seq run, got %+v", ds)
	}
	if got, _ := h.st.LoadRun(ctx, r.ID); got.MergeStatus == store.MergeStatusMerged {
		t.Fatal("a zero-seq run was merged unclaimed")
	}

	// Non-vacuous: restore the episode and the SAME run is decided, so
	// the two silences above were the zero-seq guard and nothing else.
	h.restoreOutcomeSeq(t, r.ID, r.OutcomeSeq)
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 1 {
		t.Fatalf("with its episode back the run must be decided, got %+v", ds)
	}
}

// The sweep net — end to end, no bypass — finds a policy-carrying
// terminal run the bus never mentioned (the six silent terminal paths)
// once it leaves the grace, and waits while inside it.
func TestOutcomeRouter_SweepFindsSilentTerminal(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	r := h.seedRun(t, "silent-terminal", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, true) // blocker → escalate, no git needed
		r.FinalBranch, r.FinalCommit = "iterion/run/s", "abc"
	})
	// Fresh run is inside the sweep grace → untouched.
	h.s.outcomeRouterSweepPass(ctx)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("inside grace, sweep must wait: %+v", ds)
	}
	// Age the persisted document past the grace: the REAL sweep pass —
	// ListRoutableRuns included — must now decide it.
	h.ageRun(t, r.ID, time.Now().Add(-routerSweepGrace-time.Minute))
	h.s.outcomeRouterSweepPass(ctx)
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].Decision != "escalate" {
		t.Fatalf("sweep must decide the silent terminal: %+v", ds)
	}
}

// H2 regression: an empty bank is NOT a decision. The terminal status
// lands before the bank push; deciding "escalate" there would burn the
// episode while the branch is still on its way. The router waits, and
// once the bank lands the SAME episode merges.
func TestOutcomeRouter_EmptyBankWaitsForThePush(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	r := h.seedRun(t, "bank-in-flight", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
		// No FinalBranch/FinalCommit yet: the push is still running.
	})
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("an in-flight bank must not be decided: %+v", ds)
	}
	// The bank lands (same episode — the bank write does not transition
	// status). Merge preconditions are checked against a real repo in
	// TestOutcomeRouter_MergesAndCountsEpisodes; here assert the
	// decision path claims the SAME episode once the bank exists.
	raw, _ := h.st.LoadRun(ctx, r.ID)
	seq := raw.OutcomeSeq
	raw.FinalBranch, raw.FinalCommit = "iterion/run/late-bank", "abc"
	if err := h.st.SaveRun(ctx, raw); err != nil {
		t.Fatal(err)
	}
	h.s.routeOutcomeOffer(ctx, r.ID)
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].OutcomeSeq != seq {
		t.Fatalf("the same episode must be decided once the bank lands: %+v", ds)
	}
}

// The other half of the empty-bank contract: waiting is right while the
// push may still land, but a bank that never arrives must not be refused
// SILENTLY FOREVER. A run that committed nothing (finalizeWorktree no-ops
// on an unchanged HEAD) has an empty FinalCommit for good, so before this
// it was re-offered every 60s with no row and no line, then aged out of
// the lookback with nothing ever revisiting it — the router's own
// motivating incident, recreated by the router. Past the bank deadline it
// escalates, which also frees the sweep-batch slot it would otherwise
// hold for a whole lookback.
func TestOutcomeRouter_BanklessRunEscalatesPastTheDeadline(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	sink := &advSink{}
	h.s.opsAlerts = &alert.OpsDispatcher{Sinks: []alert.Sink{sink}, Logger: iterlog.Nop()}
	r := h.seedRun(t, "no-commits-ever", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, false)
		// No FinalBranch/FinalCommit — and none is coming.
	})

	// Inside the deadline: still silent, the push may yet land.
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("inside the bank deadline the router must wait: %+v", ds)
	}

	// Past it, through the REAL sweep (the only path that ever revisits
	// such a run): one escalate row naming the missing bank.
	h.ageTerminal(t, r.ID, time.Now().Add(-routerBankGrace-time.Minute))
	h.s.outcomeRouterSweepPass(ctx)
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].Decision != "escalate" {
		t.Fatalf("a bank that never lands must be escalated, got %+v", ds)
	}
	if !strings.Contains(ds[0].Reason, "no storage branch was banked") {
		t.Fatalf("the row must name the missing bank: %+v", ds[0])
	}
	if got, _ := h.st.LoadRun(ctx, r.ID); got.MergeStatus == store.MergeStatusMerged {
		t.Fatal("the bankless run was merged")
	}
	// The operator hears it, and the settled episode leaves the sweep.
	if kinds := sink.kinds(); len(kinds) == 0 || kinds[0] != alert.KindRouteEscalated {
		t.Fatalf("want a route_escalated alert, got %v", kinds)
	}
	ids, err := h.st.ListRoutableRuns(ctx, time.Now().Add(-24*time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == r.ID {
			t.Fatalf("a decided bankless run still clogs the sweep batch: %v", ids)
		}
	}
}

// A draining server stops routing. A single candidate can perform a real
// merge — a server-side clone and a forge push for a repo-targeted run —
// taking minutes, so a pass that started just before Shutdown would keep
// landing merges through the lame-duck window and could be SIGKILLed
// mid-merge past the grace period. The gate is s.draining (set at the top
// of Shutdown), NOT s.shutdown (closed last, after the run drain and the
// HTTP teardown — by which point stopping the sweep buys nothing).
func TestOutcomeRouter_DrainStopsTheSweep(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	r := h.seedRun(t, "drain-candidate", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, true) // blocker → escalate, no git needed
		r.FinalBranch, r.FinalCommit = "iterion/run/d", "abc"
	})
	h.ageRun(t, r.ID, time.Now().Add(-routerSweepGrace-time.Minute))

	h.s.draining.Store(true)
	h.s.outcomeRouterSweepPass(ctx)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("a draining server kept routing: %+v", ds)
	}
	// The bus fast path is gated too — its handler can sit just as long.
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("a draining server kept routing off the bus: %+v", ds)
	}
	// The control that keeps this non-vacuous: not draining, same run,
	// same pass — it IS decided, so the silence above was the gate.
	h.s.draining.Store(false)
	h.s.outcomeRouterSweepPass(ctx)
	if ds := h.decisions(t, r.ID); len(ds) != 1 {
		t.Fatalf("the candidate was not routable to begin with: %+v", ds)
	}
}

// H1 regression: a "claimed" registry row whose holder died must not
// strand a green run forever — the claim is leased, and a later offer
// steals a stale one and completes the merge.
func TestOutcomeRouter_OrphanClaimIsStolenAfterLease(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	r := h.seedRun(t, "orphan-claim", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, true) // blocker → escalate (no git needed)
		r.FinalBranch, r.FinalCommit = "iterion/run/oc", "abc"
	})
	// A replica claimed this episode and died before acting.
	if claimed, _, err := h.st.ClaimRouteDecision(ctx, store.RouteDecision{RunID: r.ID, OutcomeSeq: r.OutcomeSeq, Decision: "merge"}, time.Now().Add(-store.RouteClaimLease)); err != nil || !claimed {
		t.Fatalf("seed claim: %v", err)
	}
	// Fresh claim holds: the offer must NOT steal it.
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); len(ds) != 1 || ds[0].State != store.RouteDecisionClaimed {
		t.Fatalf("fresh claim must hold: %+v", ds)
	}
	// Age the claim past the lease; the next offer steals and finishes.
	dsPath := filepath.Join(h.dir, "runs", r.ID, "route_decisions.json")
	raw, err := os.ReadFile(dsPath)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.RouteDecision
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	rows[0].ClaimedAt = time.Now().Add(-store.RouteClaimLease - time.Minute)
	out, _ := json.Marshal(rows)
	if err := os.WriteFile(dsPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
	h.s.routeOutcomeOffer(ctx, r.ID)
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].State != store.RouteDecisionSucceeded || ds[0].Decision != "escalate" {
		t.Fatalf("stale claim must be stolen and finished: %+v", ds)
	}
}

// backdateClaim rewrites every registry row's claimed_at directly in the
// store layout — the lease is time-based and nothing else ages a claim.
func (h *routerHarness) backdateClaim(t *testing.T, runID string, to time.Time) {
	t.Helper()
	p := filepath.Join(h.dir, "runs", runID, "route_decisions.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.RouteDecision
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		rows[i].ClaimedAt = to
	}
	out, _ := json.Marshal(rows)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// advSink is an ErrorReportingSink whose channel health the test flips.
type advSink struct {
	mu    sync.Mutex
	fail  bool
	calls []alert.Alert
}

func (s *advSink) Notify(context.Context, alert.Alert) {}
func (s *advSink) NotifyErr(_ context.Context, a alert.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, a)
	if s.fail {
		return errors.New("channel down")
	}
	return nil
}
func (s *advSink) kinds() []alert.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]alert.Kind, 0, len(s.calls))
	for _, a := range s.calls {
		out = append(out, a.Kind)
	}
	return out
}

// TestOutcomeRouter_WatermarkStopsRetroRouting proves the property the
// activation watermark sells, end to end through the sweep: a run that
// terminated BEFORE the router went live is never routed, while the same
// run above the watermark is.
func TestOutcomeRouter_WatermarkStopsRetroRouting(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	r := h.seedRun(t, "pre-activation", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, true)
		r.FinalBranch, r.FinalCommit = "iterion/run/pre", "abc"
	})
	// Terminal well clear of the sweep grace but BELOW a watermark set
	// to one hour ago: the router activated after this run died.
	h.ageRun(t, r.ID, time.Now().Add(-6*time.Hour))
	wm, _ := json.Marshal(map[string]time.Time{"activated_at": time.Now().Add(-time.Hour).UTC()})
	if err := os.WriteFile(filepath.Join(h.dir, "router_watermark.json"), wm, 0o644); err != nil {
		t.Fatal(err)
	}
	h.s.outcomeRouterSweepPass(ctx)
	if ds := h.decisions(t, r.ID); len(ds) != 0 {
		t.Fatalf("pre-activation terminal was routed: %+v", ds)
	}
	// The control that keeps this from passing vacuously: aged ABOVE the
	// watermark (still past the grace), the same run IS swept.
	h.ageRun(t, r.ID, time.Now().Add(-30*time.Minute))
	h.s.outcomeRouterSweepPass(ctx)
	if ds := h.decisions(t, r.ID); len(ds) != 1 {
		t.Fatalf("post-activation terminal not routed: %+v", ds)
	}
}

// TestOutcomeRouter_EscalateDeliveryFailureDoesNotBurnTheCap replays the
// round-2 adversarial scenario (a webhook dead for minutes silenced an
// escalation forever) and pins the recovery contract: a delivery failure
// leaves the row CLAIMED (no cap burn, lease-paced retries), the
// exhausted claim keeps offering the alert every sweep, and the first
// healthy channel both delivers and settles the row.
func TestOutcomeRouter_EscalateDeliveryFailureDoesNotBurnTheCap(t *testing.T) {
	h := newRouterHarness(t)
	ctx := context.Background()
	sink := &advSink{fail: true}
	h.s.opsAlerts = &alert.OpsDispatcher{Sinks: []alert.Sink{sink}, Logger: iterlog.Nop()}
	r := h.seedRun(t, "escalate-dead-webhook", func(r *store.Run) {
		r.Status = store.RunStatusFinished
		r.RoutingPolicy = mergePolicy("merge")
		r.Checkpoint = outputs(true, true) // blocker → escalate
		r.FinalBranch, r.FinalCommit = "iterion/run/edw", "abc"
	})

	// Delivery fails: the row stays claimed on attempt 1 — never failed.
	h.s.routeOutcomeOffer(ctx, r.ID)
	ds := h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].State != store.RouteDecisionClaimed || ds[0].Attempts != 1 {
		t.Fatalf("delivery failure must leave the row claimed (attempt 1), got %+v", ds)
	}
	// Inside the lease a re-offer is a no-op: no cap burn, no spam.
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); ds[0].Attempts != 1 || ds[0].State != store.RouteDecisionClaimed {
		t.Fatalf("in-lease re-offer must be a no-op, got %+v", ds)
	}
	// Two lease expiries later the steal cap is spent — still claimed.
	for i := 0; i < 2; i++ {
		h.backdateClaim(t, r.ID, time.Now().Add(-store.RouteClaimLease-time.Minute))
		h.s.routeOutcomeOffer(ctx, r.ID)
	}
	ds = h.decisions(t, r.ID)
	if ds[0].State != store.RouteDecisionClaimed || ds[0].Attempts != store.MaxRouteDecisionAttempts {
		t.Fatalf("steal retries must stop at the cap while claimed, got %+v", ds)
	}
	// Exhausted + channel still dead: the row survives for the next try.
	h.backdateClaim(t, r.ID, time.Now().Add(-store.RouteClaimLease-time.Minute))
	h.s.routeOutcomeOffer(ctx, r.ID)
	if ds := h.decisions(t, r.ID); ds[0].State != store.RouteDecisionClaimed {
		t.Fatalf("exhausted claim with a dead channel must stay open, got %+v", ds)
	}
	// The channel heals: the exhausted-claim alert lands and the row
	// settles, which also removes the run from the sweep list.
	sink.mu.Lock()
	sink.fail = false
	sink.mu.Unlock()
	h.s.routeOutcomeOffer(ctx, r.ID)
	ds = h.decisions(t, r.ID)
	if len(ds) != 1 || ds[0].State != store.RouteDecisionFailed || !strings.Contains(ds[0].Error, "operator alerted") {
		t.Fatalf("healed channel must deliver and settle the row, got %+v", ds)
	}
	kinds := sink.kinds()
	if len(kinds) < 2 || kinds[len(kinds)-1] != alert.KindRouteActionFailed {
		t.Fatalf("expected failed escalate deliveries then a successful exhaustion alert, got %v", kinds)
	}
	sawEscalate := false
	for _, k := range kinds {
		if k == alert.KindRouteEscalated {
			sawEscalate = true
		}
	}
	if !sawEscalate {
		t.Fatalf("no escalate delivery was ever attempted: %v", kinds)
	}
	ids, err := h.st.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == r.ID {
			t.Fatalf("settled run still in the sweep list: %v", ids)
		}
	}
}
