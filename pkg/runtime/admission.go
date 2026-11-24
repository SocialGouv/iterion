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
// Each verdict is saved with the version it judged. A conflict reloads and
// recomputes every check; it never applies a stale verdict to a fresh document.
func (e *Engine) admitRun(ctx context.Context, runID string, run *store.Run) error {
	if run == nil {
		return fmt.Errorf("runtime: admission: missing run %s", runID)
	}
	if e.store == nil {
		return errors.New("run store unavailable")
	}
	var bindErr error
	ctx, bindErr = store.BindRunValueFormat(ctx, run)
	if bindErr != nil {
		return bindErr
	}
	initialEpoch := run.ExecutionEpoch
	// A prepared queued entry has its own acquisition, even without accounting.
	// Retain that response throughout admission; metadata retries are not grants.
	preparedStart, _ := ctx.Value(preparedQueuedStartKey{}).(*preparedQueuedStart)
	preparedGrant := preparedStart != nil && preparedStart.grant != nil
	if preparedGrant && !preparedStart.grant.Matches(run) {
		return fmt.Errorf("%w: prepared admission execution changed", ErrAdmissionStopped)
	}
	initialStatus := run.Status
	initialQueuedAt := run.QueuedAt
	current := *run
	writeCtx := context.WithoutCancel(ctx)
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		decision, refusal := e.computeAdmission(ctx, runID, &current)
		current.Admission = &decision
		if err := e.store.SaveRun(writeCtx, &current); err == nil {
			// The caller continues with the admitted snapshot, including its
			// updated CAS version and any concurrent descriptive metadata.
			*run = current
			return refusal
		} else if !errors.Is(err, store.ErrRunConflict) {
			return fmt.Errorf("runtime: persist admission: %w", err)
		}
		_, fresh, err := store.LoadRunWithValueFormat(writeCtx, e.store, runID)
		if err != nil {
			return err
		}
		// A new queue attempt may already have been claimed and returned to
		// the same status. Keep its marker distinct from a status-only stop:
		// the runner preserves the existing publication/rollback reoffer.
		if err := e.supersededBy(fresh); err != nil {
			return err
		}
		if (fresh.QueuedAt == nil) != (initialQueuedAt == nil) || (fresh.QueuedAt != nil && initialQueuedAt != nil && !fresh.QueuedAt.Equal(*initialQueuedAt)) {
			return fmt.Errorf("%w: admission queue attempt changed", ErrResumeSuperseded)
		}
		if (e.correctionProtocolCapable() || preparedGrant) && fresh.ExecutionEpoch != initialEpoch {
			return fmt.Errorf("%w: admission execution epoch changed", ErrAdmissionStopped)
		}
		if fresh.Status != initialStatus {
			return fmt.Errorf("%w: %s became %s", ErrAdmissionStopped, initialStatus, fresh.Status)
		}
		current = *fresh
	}
	return fmt.Errorf("run %s admission CAS conflicted after 3 attempts", runID)
}

// computeAdmission reads one snapshot without persisting its verdict.
func (e *Engine) computeAdmission(ctx context.Context, runID string, run *store.Run) (store.AdmissionDecision, error) {
	if run == nil {
		return store.AdmissionDecision{}, fmt.Errorf("runtime: admission: missing run %s", runID)
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
		if d, err := e.computeUntrustedAdmission(run); err != nil {
			return d, err
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
		return decision, nil
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
	// The wire context repeats the launch declaration. Only the effective
	// store's capability is an independent authority for this comparison.
	storeObservation := e.runStoreObservation(ctx, c.RunStore)
	if storeObservation != "" && c.RunStore.Required {
		violations = append(violations, storeObservation)
	}

	policy := c.Policy
	if policy == "" {
		policy = store.ContextPolicyLegacy
	}
	decision.Policy = policy
	switch {
	case len(violations) == 0 && storeObservation != "":
		decision.Code = "context_store_optional"
		decision.Reason = storeObservation + "; run_store.required=false waives only the store requirement"
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
	if len(violations) != 0 && storeObservation != "" && !c.RunStore.Required {
		decision.Reason += "; " + storeObservation + "; run_store.required=false waives only the store requirement"
	}
	if decision.Decision == "denied" {
		return decision, &RuntimeError{
			Code:    store.FailureLaunchFailed,
			Message: "execution context admission denied",
			Hint:    "align the run store, workflow revision and parent/workspace context, or use report/legacy policy while migrating",
			Cause:   errors.New(decision.Reason),
		}
	}
	return decision, nil
}

// runStoreObservation never persists a capability error or an untrusted
// declaration: either may contain a connection string or other private data.
func (e *Engine) runStoreObservation(ctx context.Context, declared store.ContextRef) string {
	if store.AsContextIdentityStore(e.store) == nil {
		return "effective run store has no identity capability; comparison is unvalidated"
	}
	effective, err := store.ResolveStoreContextRef(ctx, e.store)
	if err != nil {
		return "effective run store identity is unavailable; comparison is unvalidated"
	}
	if !store.HasStoreContextIdentity(declared) {
		return "declared run store has a legacy identity; comparison is unvalidated"
	}
	if !store.SameContextRefIdentity(declared, effective) {
		return "declared run store identity differs from the effective run store"
	}
	return ""
}

// computeUntrustedAdmission is the PR2b floor (#1773): a fork run must
// not execute inside the runner's trust domain. It asks the ONE sandbox
// question the engine's own start will ask — RunWillBeSandboxed, the same
// spec resolution and driver selection startSandbox walks — and refuses when
// the answer is "no container": the override (`none`), no sandbox
// configuration, or a mode=auto host with no container runtime (the degraded
// case #1564 taught every consumer to ask this exact predicate, not the mode
// alone).
//
// admitRun persists this verdict through the same CAS loop as context checks.
func (e *Engine) computeUntrustedAdmission(run *store.Run) (store.AdmissionDecision, error) {
	if RunWillBeSandboxed(e.workflow, e.sandboxOverride, e.sandboxDefault, admissionRepoRoot(run, e.workDir), e.sandboxDrivers) {
		return store.AdmissionDecision{}, nil
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
	return decision, &RuntimeError{
		Code:    store.FailureLaunchFailed,
		Message: reason,
		Hint:    "run fork-lane work only on a runner class whose sandbox resolves active — unset ITERION_SANDBOX_OVERRIDE=none on the runners that serve the fork lane (the override keeps meaning what it means for trusted runs; it does not qualify a deployment to run forks), or stop admitting fork-lane launches",
		Cause:   errors.New(reason),
	}
}

// admissionRepoRoot is the repo root the admission's sandbox probe reads,
// through the engine's one resolver (runRepoRoot): the run document's
// recorded baseline, else derived from the run's workspace, else the
// engine's configured workdir — so the probe and the run's own sandbox
// start read the same repository.
func admissionRepoRoot(run *store.Run, engineWorkDir string) string {
	if run == nil {
		return ""
	}
	return runRepoRoot(run, engineWorkDir)
}
