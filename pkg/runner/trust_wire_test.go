package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestExecuteRunStampsTheWireTrustOnTheRunDocument is the main-loop twin of
// TestSubbotRunnerTheChildDocumentCarriesThePool: the wire message carries
// Trust and RepoSHAExpected (the clone gate reads them), the subbot child
// already stamps its document, and without the twin here a TOP-LEVEL
// untrusted run's document read as trusted — the fork-lane admissions (the
// PR2b no-sandbox floor, the #1768 credential refusal) never judged it. The
// zero-value stays inert: a trusted launch stamps nothing and completes as
// before (the second arm).
func TestExecuteRunStampsTheWireTrustOnTheRunDocument(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-trust-wire"
	if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
		t.Fatal(err)
	}
	pr := parsertest.Parse("main.bot", "workflow main:\n  entry: done\n")
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
		Trust: store.RunTrustFork}
	execErr := r.executeRun(ctx, msg, nil, nil)

	// The PR2b floor judges the run: an untrusted workspace resolving no
	// sandbox is refused at start — the judgment a trusted-reading document
	// would have skipped.
	var rerr *runtime.RuntimeError
	if !errors.As(execErr, &rerr) || rerr.Code != store.FailureLaunchFailed {
		t.Fatalf("top-level fork run = %v, want the untrusted-without-sandbox refusal — the wire trust never reached the engine", execErr)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Trust != store.RunTrustFork {
		t.Fatalf("the run document carries trust %q, want fork — the stamp the admissions read", run.Trust)
	}

	// The zero-value control: a trusted launch stamps nothing and completes.
	const trustedID = "run-trust-wire-trusted"
	if _, err := st.CreateRun(ctx, trustedID, "main", nil); err != nil {
		t.Fatal(err)
	}
	tmsg := &queue.RunMessage{RunID: trustedID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body}
	if execErr := r.executeRun(ctx, tmsg, nil, nil); execErr != nil {
		t.Fatalf("trusted control = %v, want a completed run", execErr)
	}
	trusted, err := st.LoadRun(ctx, trustedID)
	if err != nil {
		t.Fatal(err)
	}
	if trusted.Trust != "" {
		t.Fatalf("the trusted control's document reads trust %q, want no stamp — a trusted launch says nothing", trusted.Trust)
	}
	if trusted.Status != store.RunStatusFinished {
		t.Fatalf("the trusted control finished %v, want finished", trusted.Status)
	}
}
