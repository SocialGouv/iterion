package runner

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// circuitFileStore is a real filesystem store that ALSO carries the optional
// RetryCircuitStore capability. A filesystem store has none, so without this
// wrapper executeRun's reset branch is unreachable outside a Mongo
// deployment — which is exactly how the branch shipped it untested.
type circuitFileStore struct {
	store.RunStore
	mu        sync.Mutex
	successes []string
}

func (c *circuitFileStore) RecordRetryFailure(context.Context, string, string, time.Time, int, time.Duration) (*store.RetryCircuitState, error) {
	return nil, nil
}

func (c *circuitFileStore) RetryCircuitOpen(context.Context, string, time.Time) (*store.RetryCircuitState, error) {
	return nil, nil
}

func (c *circuitFileStore) RecordRetrySuccess(_ context.Context, key string, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.successes = append(c.successes, key)
	return nil
}

func (c *circuitFileStore) resetKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.successes...)
}

// TestExecuteRun_SuccessClosesTheSharedBreaker is the WIRING test for "a
// successful run clears the streak" — the half of the circuit contract that
// makes it safe to open one at all. Without it a recovered provider keeps
// padding every retry of the revision until the streak decays on its own.
//
// It is deliberately end-to-end through executeRun rather than a call to
// retrycoord.RecordSuccess: the reset reads the run document for its
// workflow revision, and the thing that can silently break is the branch it
// hangs off, not the store call.
func TestExecuteRun_SuccessClosesTheSharedBreaker(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := fs.CreateRun(ctx, "run-reset", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The breaker keys on the workflow revision, so the fixture has to carry
	// one — a run with neither hash nor name must not reset a shared circuit.
	run.WorkflowHash = "deadbeef"
	if err := fs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	// Terminal-only workflow: it succeeds without an LLM, a network or a
	// credential, which is all this test needs from the engine.
	pr := parser.Parse("main.bot", "workflow main:\n  entry: done\n")
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}

	cs := &circuitFileStore{RunStore: fs}
	r := &Runner{cfg: Config{Store: cs, WorkDir: t.TempDir(), Logger: iterlog.New(iterlog.LevelError, io.Discard)}}
	msg := &queue.RunMessage{RunID: "run-reset", WorkflowName: "main", IRCompiled: body}

	if execErr := r.executeRun(ctx, msg, nil); execErr != nil {
		t.Fatalf("executeRun on a terminal-only workflow: %v", execErr)
	}
	if got := cs.resetKeys(); len(got) != 1 || got[0] != "workflow:deadbeef" {
		t.Fatalf("RecordRetrySuccess keys = %v, want [workflow:deadbeef] — a successful run did not close the shared breaker", got)
	}
}

// TestExecuteRun_FailureLeavesTheBreakerAlone is the other side of the
// branch: only a run the engine completed may clear a streak. A run that
// failed is evidence FOR the breaker, and clearing it there would make the
// circuit unable to stay open through the storm it exists to damp.
func TestExecuteRun_FailureLeavesTheBreakerAlone(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := fs.CreateRun(ctx, "run-fail", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "deadbeef"
	if err := fs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	// `fail` is a terminal FAILURE node: intentional workflow termination,
	// so engine.Run returns an error with no LLM call.
	pr := parser.Parse("main.bot", "workflow main:\n  entry: fail\n")
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}

	cs := &circuitFileStore{RunStore: fs}
	r := &Runner{cfg: Config{Store: cs, WorkDir: t.TempDir(), Logger: iterlog.New(iterlog.LevelError, io.Discard)}}
	msg := &queue.RunMessage{RunID: "run-fail", WorkflowName: "main", IRCompiled: body}

	if execErr := r.executeRun(ctx, msg, nil); execErr == nil {
		t.Fatal("executeRun returned nil for a workflow whose entry is a fail node")
	}
	if got := cs.resetKeys(); len(got) != 0 {
		t.Errorf("RecordRetrySuccess called %v for a FAILED run — a failure must not clear the shared streak", got)
	}
}
