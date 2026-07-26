package dispatcher

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/runtime"
)

// DispatchSpec describes a single workflow run the dispatcher wants to
// execute on behalf of an issue. Runner.Dispatch is expected to block
// until the run reaches a terminal status or ctx is cancelled.
type DispatchSpec struct {
	RunID         string
	WorkspacePath string
	StoreDir      string
	Vars          map[string]any

	// ResumeFromRunID, when non-empty, signals the runner to resume
	// the named prior run (via runtime.Engine.Resume) instead of
	// minting a fresh execution. The dispatcher sets this on retry
	// when the prior run terminated in a resumable status — the
	// engine then picks up at the failing node, reuses the same
	// worktree, and inherits the prior checkpoint. RunID should be
	// equal to ResumeFromRunID when this is set (the resume reuses
	// the same on-disk run record).
	ResumeFromRunID string

	// Assignee is the issue's assignee at dispatch time. Empty when
	// the issue has no assignee (or the tracker doesn't carry one).
	// A RoutingRunner inspects this to pick a per-assignee workflow.
	Assignee string

	// Issue is the back-reference to the kanban issue that triggered
	// this dispatch. nil for direct CLI / studio launches; non-nil
	// when buildSpec was driven by the dispatcher actor. The
	// EngineRunner stamps these onto the run record's Source field
	// so the studio's RunHeader can link back to the ticket.
	Issue *IssueRef

	// OnEvent is invoked for every observation point of the run
	// (event_appended, node_started, …). The dispatcher uses it to
	// update its last-event watermark for stall detection. Runners
	// MUST be safe for concurrent invocation; OnEvent runs in the
	// engine goroutine.
	OnEvent func(eventName string)

	// DailyCap, when non-nil, enforces the per-(store, UTC-day) LLM
	// spend cap on this run: the engine records the run's cumulative
	// cost into the shared ledger after each node and pauses (resumable)
	// at the next node boundary once the day is over cap. The dispatcher
	// builds it from its SINGLETON SpendStore (see Dispatcher.newDailyCapGuard)
	// so every concurrent dispatched run writes the one ledger, serialising
	// on a single mutex — no lost-update race from per-run store instances.
	// nil disables the cap for this run (limit unset, or no ledger store).
	DailyCap *runtime.DailyCapGuard
}

// IssueRef captures the minimum back-reference the engine needs to
// stamp Source on the run record. Mirrors store.RunSource without
// importing it into the runner contract (the dispatcher package
// translates).
type IssueRef struct {
	ID         string
	Identifier string
	Title      string
}

// Runner abstracts the engine that turns a DispatchSpec into a running
// workflow. The production implementation wires the iterion runtime
// engine; tests provide a fake.
type Runner interface {
	Dispatch(ctx context.Context, spec DispatchSpec) error
}

// ManagedRunner is a Runner whose lifetime is owned by a Manager:
// the Manager constructs it at Start, hands it to the Dispatcher, and
// calls Close when stopping or when an error aborts startup. Both
// EngineRunner and RoutingRunner satisfy this contract.
type ManagedRunner interface {
	Runner
	Close() error
}

// StubRunner is a no-op Runner used by unit tests and bootstrap paths
// that don't actually want to execute a workflow. It records every
// dispatch it sees so tests can assert.
type StubRunner struct {
	// Handler, when non-nil, is invoked instead of returning immediately.
	// Set it to inject latency or simulate failures.
	Handler func(ctx context.Context, spec DispatchSpec) error
}

// Dispatch implements Runner.
func (s *StubRunner) Dispatch(ctx context.Context, spec DispatchSpec) error {
	if s.Handler != nil {
		return s.Handler(ctx, spec)
	}
	return nil
}
