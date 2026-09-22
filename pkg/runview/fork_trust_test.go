package runview

import (
	"context"
	"encoding/json"
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
