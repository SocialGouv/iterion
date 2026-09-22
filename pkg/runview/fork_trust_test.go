package runview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A forked child of a repo-targeted run clones the SAME tree as its parent —
// Fork deliberately carries RepoURL, RepoSHA and SecretOverrides across for
// that reason. Trust has to travel in the same statement block, because
// carrying the clone coordinates and the secret pins WITHOUT it is the
// dangerous half: the child would re-resolve the tenant's workflow secrets,
// against the parent's pins, onto an outsider's working tree.
//
// POST /api/runs/{id}/fork reaches this from an authenticated surface, so it
// is not a theoretical path.
func TestFork_CarriesTrustOntoARepoTargetedChild(t *testing.T) {
	forkFrom := func(t *testing.T, trust store.RunTrust, expectedSHA string) *store.Run {
		t.Helper()
		dir := t.TempDir()
		logger := iterlog.Nop()
		st, err := store.New(dir, store.WithLogger(logger))
		if err != nil {
			t.Fatalf("seed store: %v", err)
		}
		ctx := context.Background()
		const parentID = "run-trust-parent"
		if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
			t.Fatalf("create parent: %v", err)
		}
		parent, err := st.LoadRun(ctx, parentID)
		if err != nil {
			t.Fatalf("load parent: %v", err)
		}
		// A repo-targeted (cloud) parent: RepoURL set and no RepoRoot, which
		// is the arm that carries the clone coordinates.
		parent.RepoURL = "https://github.com/o/r.git"
		parent.RepoSHA = "refs/pull/42/head"
		parent.ProjectPath = "o/r"
		parent.BotID = "review-pr"
		parent.SecretOverrides = map[string]string{"forge_token": "sec-1"}
		parent.Trust = trust
		parent.RepoSHAExpected = expectedSHA
		parent.Status = store.RunStatusCancelled
		parent.Checkpoint = &store.Checkpoint{NodeID: "step1", ArtifactsKnown: true}
		if err := st.SaveRun(ctx, parent); err != nil {
			t.Fatalf("save parent: %v", err)
		}
		if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
			RunID: parentID, NodeID: "step1", LoopIter: 0, TurnIndex: 0,
			Backend: "claw", FinishReason: "tool_use",
			MessagesRef: "step1/0/0.messages.json",
			Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
			WrittenAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("write turn: %v", err)
		}
		svc, err := NewService(dir, WithLogger(logger))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		res, err := svc.Fork(ctx, ForkSpec{RunID: parentID, NodeID: "step1", TurnIndex: -1})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		child, err := st.LoadRun(ctx, res.NewRunID)
		if err != nil {
			t.Fatalf("load child: %v", err)
		}
		return child
	}

	t.Run("a fork-trust parent yields a fork-trust child", func(t *testing.T) {
		child := forkFrom(t, store.RunTrustFork, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		// The witness first: if the clone coordinates did not travel, the
		// assertion below would be about an arm that never ran.
		if child.RepoURL != "https://github.com/o/r.git" {
			t.Fatalf("child.RepoURL = %q — the repo-targeted arm did not run, so this test proves nothing", child.RepoURL)
		}
		if child.SecretOverrides["forge_token"] != "sec-1" {
			t.Fatalf("child.SecretOverrides = %+v — the pins did not travel, so this test proves nothing", child.SecretOverrides)
		}
		if child.Trust != store.RunTrustFork {
			t.Fatalf("child.Trust = %q, want %q — a child that clones an outsider's tree with the parent's secret pins and NO trust marker re-resolves the tenant's secrets onto it", child.Trust, store.RunTrustFork)
		}
		if child.RepoSHAExpected != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
			t.Fatalf("child.RepoSHAExpected = %q, want the parent's pin — a child that re-clones a movable ref without it takes whatever the ref points at now", child.RepoSHAExpected)
		}
	})

	t.Run("a trusted parent yields a trusted child", func(t *testing.T) {
		child := forkFrom(t, store.RunTrustDefault, "")
		if !child.Trust.Trusted() {
			t.Fatalf("child.Trust = %q, want the trusted default — the carry must not invent a restriction either", child.Trust)
		}
	})
}

// Only the cloud publisher persists Trust onto the run document. The
// in-process path builds its run from its own field list, so an untrusted
// launch there would land as a run that READS as trusted — and every
// enforcement site downstream (the publish grant, the credential resolve, a
// resume, a forked child) reads the marker off that document.
//
// Refusing is the only safe answer: silently downgrading an untrusted launch
// to a trusted run is worse than not launching it at all.
func TestLaunch_RefusesAnUntrustedSpecOnTheInProcessPath(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	spec := LaunchSpec{
		Source: "workflow w:\n  start -> done\n",
		Trust:  store.RunTrustFork,
	}
	_, err = svc.Launch(context.Background(), spec)
	if err == nil {
		t.Fatal("Launch = nil for an untrusted spec with no publisher — the run would persist with an empty Trust and read as trusted everywhere downstream")
	}
	if !strings.Contains(err.Error(), "persists neither marker") {
		t.Fatalf("err = %v, want it to name why the path cannot carry the marker", err)
	}
	// An unrecognised trust is refused too: Trusted() admits one value.
	if _, err := svc.Launch(context.Background(), LaunchSpec{Source: spec.Source, Trust: store.RunTrust("vendored")}); err == nil {
		t.Fatal("Launch = nil for a trust this binary does not recognise — an unknown trust must fail closed")
	}
	// And the OTHER fact the path cannot keep: a pinned commit. The guard
	// names both, so it must test both — a rule that names two facts and
	// enforces one is the shape the runner's own pin guard was missing.
	if _, err := svc.Launch(context.Background(), LaunchSpec{Source: spec.Source, RepoSHAExpected: "deadbeef"}); err == nil {
		t.Fatal("Launch = nil for a pinned commit the in-process path cannot persist — the pin would be silently dropped")
	}
	// And the trusted default is untouched: this must refuse nothing that
	// works today. It gets past the guard and fails later, on its own merits.
	if _, err := svc.Launch(context.Background(), LaunchSpec{Source: spec.Source}); err != nil &&
		strings.Contains(err.Error(), "persists neither marker") {
		t.Fatalf("a trusted in-process launch was refused by the new guard: %v", err)
	}
}

// The child builder has THREE branches — worktree, repo-targeted, local —
// and which one runs is decided by the parent's workspace shape, not by its
// provenance. The first version of this carry put the two assignments inside
// the repo-targeted arm, so a parent whose workflow declares `worktree: auto`
// (the IR default) or a plain local parent produced a child reading TRUSTED.
//
// This exercises the LOCAL branch: no worktree, no RepoURL. It needs no git,
// which is the point — the branch it covers is the cheap one to forget.
func TestFork_CarriesTrustOnTheNonRepoTargetedBranchToo(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	ctx := context.Background()
	const parentID = "run-local-parent"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parent, err := st.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	// Neither worktree nor repo-targeted: the final else.
	parent.Worktree = false
	parent.RepoURL = ""
	parent.WorkDir = dir
	parent.Trust = store.RunTrustFork
	parent.RepoSHAExpected = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	parent.Status = store.RunStatusCancelled
	parent.Checkpoint = &store.Checkpoint{NodeID: "step1", ArtifactsKnown: true}
	if err := st.SaveRun(ctx, parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: parentID, NodeID: "step1", LoopIter: 0, TurnIndex: 0,
		Backend: "claw", FinishReason: "tool_use",
		MessagesRef: "step1/0/0.messages.json",
		Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write turn: %v", err)
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Fork(ctx, ForkSpec{RunID: parentID, NodeID: "step1", TurnIndex: -1})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := st.LoadRun(ctx, res.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	// The witness that this really is the local branch.
	if child.RepoURL != "" || child.Worktree {
		t.Fatalf("child took another branch (RepoURL=%q worktree=%v) — this test would not cover the local arm", child.RepoURL, child.Worktree)
	}
	if child.Trust != store.RunTrustFork {
		t.Fatalf("child.Trust = %q, want %q — provenance is a fact about the parent, not about how its workspace was materialised", child.Trust, store.RunTrustFork)
	}
	if child.RepoSHAExpected != parent.RepoSHAExpected {
		t.Fatalf("child.RepoSHAExpected = %q, want the parent's pin", child.RepoSHAExpected)
	}
}
