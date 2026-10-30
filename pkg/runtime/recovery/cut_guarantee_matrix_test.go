package recovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The guarantee #2456 asks to be proven: {run cancel, node `timeout:`, run
// `max_duration`} × the three accounts a cut crossing gives of its death,
// driven through the REAL engine with the dispatcher production wires
// (pkg/runner/loop.go: recovery.Dispatch(recovery.DefaultRecipes())),
// asserting each run's outcome AND the number of times the cut node ran.
//
// What is a fixture here and what is not: the engine, the dispatcher and the
// recipes are the real ones, and the node-timeout rows carry the REAL
// *model.ErrNodeDeadline its producer builds — that the producer builds it
// is proven at its own site, against the real executor
// (pkg/backend/model/node_deadline_test.go). The three accounts are
// fixtures: a sandboxed crossing needs a container, which no deterministic
// gate has, and what the engine must classify is precisely the WORDS and the
// CHAIN they arrive with.
type cutMatrixExecutor struct {
	cutNode string
	// waitForCut blocks the node until the ENGINE's own context ends it —
	// what a run cancel and a duration budget do. A node's own `timeout:`
	// is not the engine's context: it expires inside the executor, which
	// returns while the engine's context is still live.
	waitForCut bool
	// needDeadline arms the duration-budget row: the node refuses to block
	// without the engine's per-node budget deadline on its context, so a
	// regression that stops arming one fails on the fixture's own words in
	// milliseconds instead of holding the harness to its timeout.
	needDeadline bool
	// spendCap, when set, runs once the node is holding its deadline: it
	// spends the run's duration cap on the row's own clock (see
	// matrixBudgetClock), so the engine's post-exec verdict reads a budget
	// that is genuinely out of time.
	spendCap func()
	// armed records that the cut node held a budget deadline — the premise
	// the duration-budget row judges itself by before any verdict.
	armed    atomic.Bool
	errOf    func() error
	attempts atomic.Int32
	// started is closed when the cut node has begun, so a row that cuts
	// the run from the outside waits on the CONDITION it needs rather than
	// on a duration: the engine's own startup (the run document, its first
	// events) outran a 100 ms timer on a loaded machine and the cancel
	// landed before the node existed.
	started     chan struct{}
	startedOnce sync.Once
}

func (x *cutMatrixExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() != x.cutNode {
		return map[string]any{"ok": true}, nil
	}
	x.attempts.Add(1)
	x.startedOnce.Do(func() { close(x.started) })
	if x.waitForCut {
		if x.needDeadline {
			if _, ok := ctx.Deadline(); !ok {
				return nil, fmt.Errorf("fixture: node %q got no budget deadline to aim at", node.NodeID())
			}
			x.armed.Store(true)
			if x.spendCap != nil {
				x.spendCap()
			}
		}
		<-ctx.Done()
	}
	return nil, x.errOf()
}

// matrixBudgetClock is a run budget's clock a row advances by hand. Frozen
// until the row spends the cap from inside the cut node, the cap cannot be
// consumed by anything upstream of that node — the engine's construction,
// the store, the entry node — so the row decides on the order it staged
// (the node holding, THEN the cap spent), never on a race between the
// engine's own startup and a 300 ms cap on a loaded runner.
type matrixBudgetClock struct {
	base   time.Time
	offset atomic.Int64
}

func newMatrixBudgetClock() *matrixBudgetClock {
	return &matrixBudgetClock{base: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
}

func (c *matrixBudgetClock) Now() time.Time { return c.base.Add(time.Duration(c.offset.Load())) }

// spend returns the closure that exhausts a cap of d, plus the sliver a
// real clock adds between the cut and the verdict, so the engine reads the
// cap as past rather than exactly on it.
func (c *matrixBudgetClock) spend(d time.Duration) func() {
	return func() { c.offset.Add(int64(d) + int64(time.Millisecond)) }
}

// cutMatrixWorkflow: an entry node that answers at once, the node the cut
// lands on, and a done. maxDuration empty leaves the run unbounded.
func cutMatrixWorkflow(maxDuration time.Duration) *ir.Workflow {
	wf := &ir.Workflow{
		Name:  "cut_guarantee_matrix",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"slow": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "slow"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "slow"},
			{From: "slow", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	if maxDuration > 0 {
		wf.Budget = &ir.Budget{MaxDuration: maxDuration.String()}
	}
	return wf
}

func matrixStore(t *testing.T) store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return s
}

func TestTheCutGuaranteeMatrixThroughTheRealEngine(t *testing.T) {
	const bound = 20 * time.Millisecond
	// Each row names the cut, builds the run, and says what the run must end
	// as. Every row asserts ONE attempt: no cut re-runs the node it killed.
	rows := []struct {
		cut string
		// run drives the engine and returns the store it wrote to, the run
		// id, the executor that counted the attempts, and the run's error.
		// observed is the account the backend gives of the cut.
		run func(t *testing.T, observed error) (store.RunStore, string, *cutMatrixExecutor, error)
		// want is the engine code the run must end with, read from the
		// persisted run as well as from the returned error. Empty means the
		// row asserts a sentinel instead (see wantCancelled).
		want runtime.ErrorCode
		// wantArmed says the row's premise is the cut node holding the
		// engine's per-node budget deadline; the row fails before any
		// verdict when it didn't.
		wantArmed     bool
		wantCancelled bool
	}{
		{
			// The node's own bound: the engine's context is live, and what
			// comes back is the typed cut its producer names.
			cut: "the node's own timeout:",
			run: func(t *testing.T, observed error) (store.RunStore, string, *cutMatrixExecutor, error) {
				x := &cutMatrixExecutor{cutNode: "slow", started: make(chan struct{}), errOf: func() error {
					return &model.ErrNodeDeadline{NodeID: "slow", Timeout: bound, Observed: observed}
				}}
				s, runID := matrixStore(t), "run-cut-node-timeout"
				eng := runtime.New(cutMatrixWorkflow(0), s, x,
					runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
				return s, runID, x, eng.Run(context.Background(), runID, nil)
			},
			want: runtime.ErrCodeTimeout,
		},
		{
			// The run's duration budget: the engine froze the deadline into
			// the node's context and knows it fired, whatever came back.
			// The budget's clock is the row's, frozen until the cut node
			// holds its deadline: the cap measures how long the NODE is
			// held, never how long the engine took to reach it — a loaded
			// runner can otherwise spend the cap upstream, take the pre-exec
			// refusal, and leave the row reading a cut with no node in it.
			cut: "the run's max_duration",
			run: func(t *testing.T, observed error) (store.RunStore, string, *cutMatrixExecutor, error) {
				clock := newMatrixBudgetClock()
				x := &cutMatrixExecutor{
					cutNode:      "slow",
					waitForCut:   true,
					needDeadline: true,
					spendCap:     clock.spend(300 * time.Millisecond),
					started:      make(chan struct{}),
					errOf:        func() error { return observed },
				}
				s, runID := matrixStore(t), "run-cut-max-duration"
				eng := runtime.New(cutMatrixWorkflow(300*time.Millisecond), s, x,
					runtime.WithBudgetClock(clock.Now),
					runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
				return s, runID, x, eng.Run(context.Background(), runID, nil)
			},
			want:      runtime.ErrCodeBudgetExceeded,
			wantArmed: true,
		},
		{
			// The run cancelled: the engine reads its own context at the
			// node boundary, and the backend's words are the detail.
			cut: "the run cancelled",
			run: func(t *testing.T, observed error) (store.RunStore, string, *cutMatrixExecutor, error) {
				x := &cutMatrixExecutor{cutNode: "slow", waitForCut: true, started: make(chan struct{}), errOf: func() error { return observed }}
				s, runID := matrixStore(t), "run-cut-cancelled"
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				go func() {
					<-x.started
					cancel()
				}()
				eng := runtime.New(cutMatrixWorkflow(0), s, x,
					runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
				return s, runID, x, eng.Run(ctx, runID, nil)
			},
			wantCancelled: true,
		},
	}

	for _, row := range rows {
		for voice, observed := range cutAccounts {
			t.Run(row.cut+" × "+voice, func(t *testing.T) {
				s, runID, x, err := row.run(t, observed)
				if err == nil {
					t.Fatalf("the run ended cleanly; want the cut to end it")
				}
				if row.wantArmed && !x.armed.Load() {
					t.Fatalf("the cut node held no budget deadline (%v): the per-node duration deadline is gone and the row has nothing to judge", err)
				}
				if row.wantCancelled {
					if !errors.Is(err, runtime.ErrRunCancelled) {
						t.Errorf("the run ended with %v; want a cancellation — the engine reads the cut from its own context, never from the crossing's words", err)
					}
				} else {
					var rt *runtime.RuntimeError
					if !errors.As(err, &rt) || rt.Code != row.want {
						t.Errorf("the run ended with %v; want code %s", err, row.want)
					}
					run, loadErr := s.LoadRun(context.Background(), runID)
					if loadErr != nil {
						t.Fatalf("load run: %v", loadErr)
					}
					if run.FailureCode != row.want {
						t.Errorf("persisted failure_code = %q, want %q", run.FailureCode, row.want)
					}
				}
				if n := x.attempts.Load(); n != 1 {
					t.Errorf("the cut node ran %d times; want once — no cut re-runs the node it killed", n)
				}
			})
		}
	}
}
