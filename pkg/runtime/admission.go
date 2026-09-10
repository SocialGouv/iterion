package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// admitRun is the single runtime admission gate. It runs after the run
// document exists (so the decision is durable) and before attachments,
// workspace setup or the first executor/model call. Legacy and report-only
// contexts are allowed but still leave an explainable decision behind.
func (e *Engine) admitRun(ctx context.Context, runID string, run *store.Run) error {
	if run == nil {
		return fmt.Errorf("runtime: admission: missing run %s", runID)
	}
	decision := store.AdmissionDecision{
		Decision:  "allowed",
		Phase:     "pre_model",
		Policy:    store.ContextPolicyLegacy,
		CheckedAt: time.Now().UTC(),
	}
	var violations []string
	if run.ExecutionContext == nil {
		decision.Code = "legacy_context"
		decision.Reason = "run predates the versioned execution-context contract"
		if err := e.persistAdmission(ctx, runID, run, decision); err != nil {
			return fmt.Errorf("runtime: persist admission: %w", err)
		}
		return nil
	}

	c := run.ExecutionContext.Clone()
	decision.Policy = c.Policy
	decision.WorkflowRevision = c.Workflow.WorkflowRevision
	if err := c.Normalize(); err != nil {
		violations = append(violations, err.Error())
	}
	decision.ContextVersion = c.Version
	if c.Workflow.WorkflowRevision != "" && run.WorkflowHash != "" && c.Workflow.WorkflowRevision != run.WorkflowHash {
		violations = append(violations, fmt.Sprintf("workflow revision %q does not match run hash %q", c.Workflow.WorkflowRevision, run.WorkflowHash))
	}
	if c.Lineage.ParentRunID != "" && c.Lineage.ParentRunID != run.ParentRunID {
		violations = append(violations, fmt.Sprintf("parent run %q does not match persisted parent %q", c.Lineage.ParentRunID, run.ParentRunID))
	}
	if c.Lineage.ParentRunID == "" && run.ParentRunID != "" {
		violations = append(violations, "context omitted the persisted parent run")
	}
	if expected := e.executionContext; expected != nil {
		if expected.Workflow.WorkflowRevision != "" && expected.Workflow.WorkflowRevision != c.Workflow.WorkflowRevision {
			violations = append(violations, "runner context workflow revision differs from persisted context")
		}
		if expected.Lineage.ParentRunID != "" && expected.Lineage.ParentRunID != c.Lineage.ParentRunID {
			violations = append(violations, "runner context parent differs from persisted context")
		}
		if expected.Policy == store.ContextPolicyEnforce && c.Policy != store.ContextPolicyEnforce {
			violations = append(violations, "runner requested enforce policy but persisted context is not enforce")
		}
	}

	policy := c.Policy
	if policy == "" {
		policy = store.ContextPolicyLegacy
	}
	decision.Policy = policy
	switch {
	case len(violations) == 0:
		decision.Code = "context_match"
		decision.Reason = "resolved execution context matches the run record"
	case policy == store.ContextPolicyEnforce:
		decision.Decision = "denied"
		decision.Code = "context_mismatch"
		decision.Reason = strings.Join(violations, "; ")
	case policy == store.ContextPolicyReport:
		decision.Code = "context_report_only"
		decision.Reason = strings.Join(violations, "; ")
	default:
		decision.Code = "legacy_context_unvalidated"
		decision.Reason = strings.Join(violations, "; ")
	}
	if err := e.persistAdmission(ctx, runID, run, decision); err != nil {
		return fmt.Errorf("runtime: persist admission: %w", err)
	}
	if decision.Decision == "denied" {
		return &RuntimeError{
			Code:    store.FailureLaunchFailed,
			Message: "execution context admission denied",
			Hint:    "align the run store, workflow revision and parent/workspace context, or use report/legacy policy while migrating",
			Cause:   errors.New(decision.Reason),
		}
	}
	return nil
}

// persistAdmission uses the run CAS contract and never blindly overwrites a
// newer run document. A concurrent writer is reloaded and the decision is
// applied to that fresh copy, bounded to three attempts.
func (e *Engine) persistAdmission(ctx context.Context, runID string, run *store.Run, decision store.AdmissionDecision) error {
	if e.store == nil {
		return errors.New("run store unavailable")
	}
	current := run
	writeCtx := context.WithoutCancel(ctx)
	for attempt := 0; attempt < 3; attempt++ {
		copyDecision := decision
		current.Admission = &copyDecision
		if err := e.store.SaveRun(writeCtx, current); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrRunConflict) {
			return err
		}
		fresh, loadErr := e.store.LoadRun(writeCtx, runID)
		if loadErr != nil {
			return loadErr
		}
		current = fresh
	}
	return fmt.Errorf("run %s admission CAS conflicted after 3 attempts", runID)
}
