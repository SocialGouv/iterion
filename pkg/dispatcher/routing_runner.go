package dispatcher

import (
	"context"
	"errors"
	"fmt"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// RoutingRunner dispatches workflow runs based on the issue's
// `Assignee` field. Each registered assignee maps to a pre-compiled
// Runner; issues whose assignee is empty or unknown fall back to the
// default Runner. The mapping is read-only after construction so
// Dispatch is lock-free.
//
// This is how the dispatcher "auto-pilots" a kanban populated by
// whats-next.bot (or any other producer): the producer stamps each
// issue with `--assignee <bot_name>`, then a single
// `iterion.dispatcher.yaml` declares the mapping:
//
//	workflow: workflows/default.bot                    # fallback
//	assignee_workflows:
//	  feature_dev:        examples/feature_dev/main.bot
//	  whole_improve_loop: examples/whole_improve_loop/main.bot
//	  secured-renovacy:   examples/secured-renovacy/main.bot
type RoutingRunner struct {
	// Default is invoked when the issue's assignee is empty or not
	// present in ByAssignee. MUST be non-nil.
	Default Runner

	// ByAssignee maps assignee names (case-sensitive, exact match) to
	// per-assignee Runners. A nil or empty map degenerates to "all
	// dispatches go to Default" — equivalent to wrapping Default.
	ByAssignee map[string]Runner
}

// Dispatch implements Runner.
func (r *RoutingRunner) Dispatch(ctx context.Context, spec DispatchSpec) error {
	if r == nil || r.Default == nil {
		return errors.New("routing runner: default runner is nil")
	}
	if r.ByAssignee != nil && spec.Assignee != "" {
		if rn, ok := r.ByAssignee[spec.Assignee]; ok && rn != nil {
			return rn.Dispatch(ctx, spec)
		}
	}
	return r.Default.Dispatch(ctx, spec)
}

// Close releases all per-assignee runners (and the default), in any
// order. A runner that does not implement `Close() error` is skipped
// silently. The first error encountered is returned but cleanup
// continues so a partial failure does not leak the rest.
func (r *RoutingRunner) Close() error {
	if r == nil {
		return nil
	}
	var firstErr error
	closeOne := func(name string, runner Runner) {
		if runner == nil {
			return
		}
		closer, ok := runner.(interface{ Close() error })
		if !ok {
			return
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("routing runner: close %q: %w", name, err)
		}
	}
	for name, runner := range r.ByAssignee {
		closeOne(name, runner)
	}
	closeOne("default", r.Default)
	return firstErr
}

// NewRoutingRunner builds a RoutingRunner from the dispatcher config's
// Workflow (default) and AssigneeWorkflows fields. Returns the
// underlying default EngineRunner directly when AssigneeWorkflows is
// empty (no need to pay the wrapper cost). The caller is responsible
// for calling Close on the returned ManagedRunner.
func NewRoutingRunner(cfg *Config, logger *iterlog.Logger) (ManagedRunner, error) {
	if cfg == nil {
		return nil, errors.New("routing runner: config required")
	}
	if cfg.Workflow == "" {
		return nil, errors.New("routing runner: cfg.Workflow is required (default fallback)")
	}
	defaultRunner, err := NewEngineRunner(cfg.Workflow, logger)
	if err != nil {
		return nil, fmt.Errorf("routing runner: default workflow %s: %w", cfg.Workflow, err)
	}
	if len(cfg.AssigneeWorkflows) == 0 {
		return defaultRunner, nil
	}
	r := &RoutingRunner{
		Default:    defaultRunner,
		ByAssignee: make(map[string]Runner, len(cfg.AssigneeWorkflows)),
	}
	for assignee, wfPath := range cfg.AssigneeWorkflows {
		if assignee == "" {
			return nil, errors.New("routing runner: assignee_workflows contains an empty key")
		}
		runner, err := NewEngineRunner(wfPath, logger)
		if err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("routing runner: assignee %q workflow %s: %w", assignee, wfPath, err)
		}
		r.ByAssignee[assignee] = runner
	}
	return r, nil
}
