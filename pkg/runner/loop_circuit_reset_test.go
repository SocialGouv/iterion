package runner

import (
	"context"
	"errors"
	"io"
	"strings"
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
	// loadErr, when set, fails every LoadRun the way an unreachable primary
	// does. Only closeRetryCircuit reads it — executeRun's own loads go
	// through a store built without it.
	loadErr    error
	successErr error

	mu        sync.Mutex
	successes []string
}

func (c *circuitFileStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	return c.RunStore.LoadRun(ctx, id)
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
	return c.successErr
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

// TestCloseRetryCircuit_ReportsWhyItCouldNotReset covers the degradations
// end-to-end coverage cannot reach: the reset runs on the way OUT of a run
// that already succeeded, so nothing downstream surfaces a failure here and
// these log lines are the only place a breaker nobody can reset is visible.
func TestCloseRetryCircuit_ReportsWhyItCouldNotReset(t *testing.T) {
	seed := func(t *testing.T, hash string) store.RunStore {
		t.Helper()
		fs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		run, err := fs.CreateRun(context.Background(), "run-x", "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.WorkflowHash = hash
		if err := fs.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return fs
	}

	t.Run("an unreadable run doc says so", func(t *testing.T) {
		var logged strings.Builder
		cs := &circuitFileStore{RunStore: seed(t, "abc"), loadErr: errors.New("primary unreachable")}
		r := &Runner{cfg: Config{Store: cs, Logger: iterlog.New(iterlog.LevelWarn, &logged)}}

		r.closeRetryCircuit(context.Background(), "run-x", r.cfg.Logger)

		if !strings.Contains(logged.String(), "primary unreachable") {
			t.Errorf("a failed reset left no trace in the log: %q", logged.String())
		}
		if got := cs.resetKeys(); len(got) != 0 {
			t.Errorf("reset attempted with no run document: %v", got)
		}
	})

	t.Run("a failing circuit write says so", func(t *testing.T) {
		var logged strings.Builder
		cs := &circuitFileStore{RunStore: seed(t, "abc"), successErr: errors.New("write concern not met")}
		r := &Runner{cfg: Config{Store: cs, Logger: iterlog.New(iterlog.LevelWarn, &logged)}}

		r.closeRetryCircuit(context.Background(), "run-x", r.cfg.Logger)

		if !strings.Contains(logged.String(), "write concern not met") {
			t.Errorf("a failed circuit write left no trace in the log: %q", logged.String())
		}
	})

	t.Run("a run with no revision keys no breaker", func(t *testing.T) {
		// CreateRun stamps WorkflowName, so blank BOTH: a keyless run must
		// not collapse into a shared document.
		fs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		run, err := fs.CreateRun(context.Background(), "run-x", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.WorkflowHash, run.WorkflowName = "", ""
		if err := fs.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		cs := &circuitFileStore{RunStore: fs}
		r := &Runner{cfg: Config{Store: cs, Logger: iterlog.New(iterlog.LevelError, io.Discard)}}

		r.closeRetryCircuit(context.Background(), "run-x", r.cfg.Logger)

		if got := cs.resetKeys(); len(got) != 0 {
			t.Errorf("reset a breaker under key(s) %v for a run with no workflow revision", got)
		}
	})

	t.Run("a store with no circuit capability is left alone", func(t *testing.T) {
		// The plain filesystem store carries no RetryCircuitStore, so the
		// probe must return before the run document is even read.
		plain := &countingLoadStore{RunStore: seed(t, "abc")}
		r := &Runner{cfg: Config{Store: plain, Logger: iterlog.New(iterlog.LevelError, io.Discard)}}

		r.closeRetryCircuit(context.Background(), "run-x", r.cfg.Logger)

		if plain.loads != 0 {
			t.Errorf("%d LoadRun(s) on a store that cannot hold a circuit — a round trip per successful run for a no-op", plain.loads)
		}
	})
}

// countingLoadStore is deliberately NOT a RetryCircuitStore.
type countingLoadStore struct {
	store.RunStore
	loads int
}

func (c *countingLoadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	c.loads++
	return c.RunStore.LoadRun(ctx, id)
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
