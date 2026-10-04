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
	// PR2b admission (#1773): a run whose workspace holds an OUTSIDER's code
	// (Trust untrusted — today the fork review lane) executes only inside a
	// per-run sandbox. Without one, the fork's agent runs inside the runner's
	// trust domain through several independent doors (the claude_code spawn
	// env, claw's bash tool, its unconfined write/edit tools, its egress) and
	// every pod secret — ITERION_SECRETS_KEY, MONGODB_URI, the LLM keys — is
	// one `/proc/self/environ` read away from it. The chart's
	// ITERION_SANDBOX_OVERRIDE=none keeps meaning what it means for trusted
	// runs — the override is an explicit opt-out, non-overridable by
	// design — it simply does not qualify a deployment to run forks: THIS
	// fork run is refused, typed, with the way out named.
	if !run.Trust.Trusted() {
		if err := e.refuseUntrustedWithoutSandbox(ctx, runID, run); err != nil {
			return err
		}
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

// refuseUntrustedWithoutSandbox is the PR2b floor (#1773): a fork run must
// not execute inside the runner's trust domain. It asks the ONE sandbox
// question the engine's own start will ask — RunWillBeSandboxed, the same
// spec resolution and driver selection startSandbox walks — and refuses when
// the answer is "no container": the override (`none`), no sandbox
// configuration, or a mode=auto host with no container runtime (the degraded
// case #1564 taught every consumer to ask this exact predicate, not the mode
// alone).
//
// The verdict is persisted as the run's admission decision before the
// refusal returns, so the run record explains itself (the run list, the
// studio and the failure row read Admission).
func (e *Engine) refuseUntrustedWithoutSandbox(ctx context.Context, runID string, run *store.Run) error {
	if RunWillBeSandboxed(e.workflow, e.sandboxOverride, e.sandboxDefault, admissionRepoRoot(run, e.workDir), e.sandboxDrivers) {
		return nil
	}
	// Name the tier that spoke, so the operator reads which knob decided:
	// the override when one is set, its absence when the resolution fell to
	// the workflow block or the global default.
	override := fmt.Sprintf("the ITERION_SANDBOX_OVERRIDE=%q override is set", e.sandboxOverride)
	if e.sandboxOverride == "" {
		override = "no ITERION_SANDBOX_OVERRIDE is set"
	}
	reason := fmt.Sprintf("untrusted-workspace run (trust=%s) resolves no per-run sandbox here: %s, and the resolved sandbox is no container (the override, no sandbox configuration, or a mode=auto host with no container runtime) — the fork's agent would execute inside the runner's trust domain", run.Trust, override)
	decision := store.AdmissionDecision{
		Decision:  "denied",
		Phase:     "pre_model",
		Code:      "untrusted_without_sandbox",
		Reason:    reason,
		CheckedAt: time.Now().UTC(),
	}
	if err := e.persistAdmission(ctx, runID, run, decision); err != nil {
		return fmt.Errorf("runtime: persist admission: %w", err)
	}
	return &RuntimeError{
		Code:    store.FailureLaunchFailed,
		Message: reason,
		Hint:    "run fork-lane work only on a runner class whose sandbox resolves active — unset ITERION_SANDBOX_OVERRIDE=none on the runners that serve the fork lane (the override keeps meaning what it means for trusted runs; it does not qualify a deployment to run forks), or stop admitting fork-lane launches",
		Cause:   errors.New(reason),
	}
}

// admissionRepoRoot is the repo root the admission's sandbox probe reads:
// the run document's, else derived from the run's workspace, else the
// engine's configured workdir — the same precedence seedRepoRootForResume
// applies, so the probe and the run's own sandbox start read the same
// repository.
func admissionRepoRoot(run *store.Run, engineWorkDir string) string {
	if run == nil {
		return ""
	}
	if run.RepoRoot != "" {
		return run.RepoRoot
	}
	if run.WorkDir != "" {
		return EngineRepoRoot(run.WorkDir)
	}
	return EngineRepoRoot(engineWorkDir)
}

// persistAdmission uses the run CAS contract and never blindly overwrites a
// newer run document. A concurrent writer is reload-and-reapply: the
// decision is applied to the fresh copy, bounded to three attempts.
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
