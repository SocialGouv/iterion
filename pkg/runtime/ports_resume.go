package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

var ErrPortEffectUncertain = errors.New("runtime: native effect outcome requires recovery evidence")

// Native resumes never enter legacy checkpoint reconstruction. Inspect the
// captured identity before admission, answers, workspace setup or any claim.
// The same Engine infrastructure then restores the execution environment.
func (e *Engine) resumePortRun(ctx context.Context, r *store.Run, answers map[string]any) (resultErr error) {
	if len(answers) != 0 {
		return fmt.Errorf("runtime: native resume does not accept legacy gate answers")
	}
	if !r.Status.CanNativeResume() {
		return fmt.Errorf("runtime: native run %s is not resumable (%s)", r.ID, r.Status)
	}
	inputs, err := e.nativeRootInputs(r.Inputs)
	if err != nil {
		return err
	}
	fresh, err := e.newPortExecution(ctx, r.ID, inputs)
	if err != nil {
		return err
	}
	state := r.PortExecution
	if state != nil {
		if err := store.ValidatePortExecution(state); err != nil {
			return err
		}
		if !reflect.DeepEqual(state.Identity, fresh.Identity) && !e.forceResume {
			return fmt.Errorf("%w: native source, inputs, contracts or execution policies differ; use --force only for a deliberate compatible migration", ErrWorkflowSourceChanged)
		}
	}
	if r.Worktree {
		if err := checkWorktreeLinkage(r.WorkDir); err != nil {
			return err
		}
	}
	if err := e.refuseResumeOfSharedChild(ctx, r); err != nil {
		return err
	}
	if err := e.refuseBundleRequiringNewerEngine(); err != nil {
		return err
	}
	if err := e.admitRun(ctx, r.ID, r); err != nil {
		return err
	}
	// Admission may update metadata. Re-read, check the execution snapshot
	// and inputs we just validated, and claim with the full document CAS.
	claimed, err := e.store.LoadRun(ctx, r.ID)
	if err != nil {
		return err
	}
	if claimed.Status != r.Status || !reflect.DeepEqual(claimed.PortExecution, state) || !reflect.DeepEqual(claimed.Inputs, r.Inputs) || claimed.WorkDir != r.WorkDir {
		return fmt.Errorf("runtime: native resume snapshot changed before claim: %w", store.ErrRunConflict)
	}
	claimed.Status, claimed.Error = store.RunStatusRunning, ""
	claimed.FinishedAt = nil
	if err := e.store.SaveRun(ctx, claimed); err != nil {
		return err
	}
	// All failures after the claim must leave an actionable resumable run,
	// including errors before the coordinator can take ownership.
	defer func() {
		if resultErr != nil {
			_, _ = e.store.UpdateRunStatusIf(context.WithoutCancel(ctx), r.ID, store.RunStatusFailedResumable, resultErr.Error(), []store.RunStatus{store.RunStatusRunning})
		}
	}()
	if err := e.restoreResumeWorkspace(claimed); err != nil {
		return err
	}
	repoRoot := r.RepoRoot
	if repoRoot == "" {
		repoRoot = engineRepoRoot(e.workDir)
	}
	cleanup, err := e.startSandbox(ctx, r.ID, repoRoot, resolveWorktreeGitDir(repoRoot, r.WorkDir), inputs)
	if err != nil {
		return err
	}
	defer cleanup()
	rs := e.runInitState(ctx, r.ID, inputs)
	rs.resumed = true
	if state == nil {
		if err := store.SavePortExecution(ctx, e.store, r.ID, 0, fresh, store.RunStatusRunning); err != nil {
			return err
		}
		state = fresh
	}
	budget := state.Budget
	rs.budget.Restore(int(budget.Consumed.Tokens), budget.Consumed.CostUSD, int(budget.Consumed.Iterations), time.Duration(budget.ElapsedNS), int(budget.UnpricedTokens), int(budget.UnpricedNodes))
	rs.startedAt = time.Now().Add(-time.Duration(budget.ElapsedNS))
	e.applySteeringState(rs, claimed)
	limit := 0
	if e.workflow.Budget != nil {
		limit = e.workflow.Budget.MaxParallelBranches
	}
	c := &portCoordinator{engine: e, rs: rs, state: state, active: map[string]func(){}, done: make(chan portCompletion), limit: limit}
	if err := c.recoverInterrupted(ctx); err != nil {
		return c.finish(ctx, err, false)
	}
	if err := c.reconcilePortIdentity(ctx, fresh); err != nil {
		return c.finish(ctx, err, false)
	}
	if err := c.retryPortInvocations(ctx); err != nil {
		return c.finish(ctx, err, false)
	}
	if err := e.markResumed(ctx, r.ID, map[string]any{"runtime_semantics": ir.RuntimeSemanticsPortsV1, "generation": c.state.Generation}); err != nil {
		return err
	}
	e.restampWorkflowSource(ctx, claimed)
	resultErr = c.execute(ctx)
	e.evictRunSessions(r.ID, resultErr)
	if wtCtx := e.reconstructWorktreeContext(claimed); wtCtx != nil {
		e.finalizeOnExit(ctx, r.ID, wtCtx, nil, resultErr)
	}
	return resultErr
}

// A durable dispatch without completion is not a successful invocation.
// Preserve uncertain effects until the captured policy or an explicit
// attempt-bound operator/verifier decision permits replay.
func (c *portCoordinator) recoverInterrupted(ctx context.Context) error {
	next, err := c.state.Clone()
	if err != nil {
		return err
	}
	changed := false
	for id, invocation := range next.Invocations {
		switch invocation.Status {
		case store.PortAdmitted:
			invocation.Status, invocation.Failure = store.PortCanceled, "interrupted before effect dispatch"
			delete(next.Budget.Reservations, id)
			changed = true
		case store.PortRunning:
			invocation.Status, invocation.Failure = store.PortFailed, "execution interrupted before a committed output"
			next.Budget.Consumed.Iterations++
			if invocation.EffectDispatched {
				invocation.Status = store.PortUncertain
			} else {
				delete(next.Budget.Reservations, id)
			}
			changed = true
		}
		if len(invocation.Resources) != 0 {
			invocation.Resources = nil // no process from the old claim owns these leases
			changed = true
		}
	}
	if changed {
		if err := c.commit(ctx, next, store.RunStatusRunning); err != nil {
			return err
		}
	}
	// Resolve decisions separately so an interrupted running effect has an
	// explicit uncertain checkpoint before any retry becomes possible.
	next, err = c.state.Clone()
	if err != nil {
		return err
	}
	changed = false
	var unresolved error
	for id, invocation := range next.Invocations {
		if invocation.Status != store.PortUncertain {
			continue
		}
		instance := c.engine.workflow.Ports.Nodes[invocation.Node]
		idempotent := false
		if instance != nil && instance.Policy.Identity == invocation.Identity.Policy {
			idempotent = len(instance.Policy.Effects) > 0
			for _, effect := range instance.Policy.Effects {
				idempotent = idempotent && effect.Recovery == "idempotent"
			}
		}
		decision := invocation.RecoveryDecision != "" && invocation.RecoveryAttempt == invocation.Attempt
		if !idempotent && !decision {
			unresolved = errors.Join(unresolved, fmt.Errorf("%w: %s attempt %d; verify the external outcome and record a decision for this exact attempt", ErrPortEffectUncertain, id, invocation.Attempt))
			continue
		}
		if !decision {
			invocation.RecoveryDecision = "captured technical policy declares idempotent recovery"
			invocation.RecoveryAttempt = invocation.Attempt
		}
		invocation.Status = store.PortFailed
		if amount, reserved := next.Budget.Reservations[id]; reserved && (amount.Tokens > 0 || amount.CostUSD > 0) {
			// A dispatch has no receipt. Do not label its unknown usage free;
			// finite cost admission stays blocked until usage is reconciled.
			next.Budget.UnpricedNodes++
		}
		delete(next.Budget.Reservations, id)
		changed = true
	}
	if changed {
		if err := c.commit(ctx, next, store.RunStatusRunning); err != nil {
			return err
		}
	}
	return unresolved
}

func (c *portCoordinator) reconcilePortIdentity(ctx context.Context, fresh *store.PortExecution) error {
	invalid := map[string]bool{}
	for _, invocation := range c.state.Invocations {
		instance := c.engine.workflow.Ports.Nodes[invocation.Node]
		if instance == nil {
			invalid[invocation.Node] = true
			continue
		}
		candidate, err := c.engine.newPortInvocation(instance, invocation.ID, invocation.MapIndex)
		if err != nil {
			return err
		}
		before, after := invocation.Identity, candidate.Identity
		before.Inputs, after.Inputs = "", ""
		if !reflect.DeepEqual(before, after) || c.state.Identity.Policy != fresh.Identity.Policy {
			invalid[invocation.Node] = true
		}
		for _, revision := range invocation.Inputs {
			value := c.state.Publications[revision]
			if value != nil && value.Producer == "input" && !reflect.DeepEqual(value, fresh.Publications[revision]) {
				invalid[invocation.Node] = true
			}
		}
	}
	files := store.AsRunFilesStore(c.engine.store)
	verifiedFiles := map[store.PortFileRef]bool{}
	for _, value := range c.state.Publications {
		for _, file := range value.Files {
			if verifiedFiles[file] {
				continue
			}
			verifiedFiles[file] = true
			if files == nil {
				return fmt.Errorf("runtime: native file recovery needs a run-files store")
			}
			body, _, openErr := files.OpenRunFile(ctx, file.RunID, file.Path)
			missing := errors.Is(openErr, os.ErrNotExist) || errors.Is(openErr, blob.ErrArtifactNotFound)
			if openErr != nil && !missing {
				return fmt.Errorf("runtime: cannot verify published file %s; preserving committed work: %w", file.Path, openErr)
			}
			corrupt := false
			if openErr == nil {
				verifyErr := store.VerifyPortFile(ctx, file, body)
				closeErr := body.Close()
				if closeErr != nil {
					return fmt.Errorf("runtime: cannot close published file %s; preserving committed work: %w", file.Path, closeErr)
				}
				corrupt = errors.Is(verifyErr, store.ErrPortFileCorrupt)
				if verifyErr != nil && !corrupt {
					return fmt.Errorf("runtime: cannot verify published file %s; preserving committed work: %w", file.Path, verifyErr)
				}
			}
			if missing || corrupt {
				if file.Producer == "input" {
					return fmt.Errorf("runtime: root input file is missing or corrupt: %s", file.Path)
				}
				owner := c.state.Invocations[file.Producer]
				if owner == nil {
					return fmt.Errorf("runtime: published file %s has no invocation owner", file.Path)
				}
				invalid[owner.Node] = true
			}
		}
	}
	if len(invalid) == 0 && reflect.DeepEqual(c.state.Identity, fresh.Identity) {
		return nil
	}
	// Propagate using the actual data graph, not a linear control suffix.
	for _, id := range c.engine.workflow.Ports.Order {
		for _, dependency := range c.engine.workflow.Ports.Nodes[id].Dependencies {
			if invalid[dependency] {
				invalid[id] = true
			}
		}
	}
	next, err := fresh.Clone()
	if err != nil {
		return err
	}
	next.Generation = c.state.Generation + 1
	next.Budget = c.state.Budget
	for id, invocation := range c.state.Invocations {
		if c.engine.workflow.Ports.Nodes[invocation.Node] == nil {
			continue
		}
		if invalid[invocation.Node] {
			if replacement := next.Invocations[id]; replacement != nil {
				replacement.Attempt = invocation.Attempt + 1
			}
			continue
		}
		next.Invocations[id] = invocation
		for _, revision := range invocation.Outputs {
			next.Publications[revision] = c.state.Publications[revision]
		}
	}
	for id, collection := range c.state.Collections {
		if invalid[collection.Node] || c.engine.workflow.Ports.Nodes[collection.Node] == nil {
			continue
		}
		next.Collections[id] = collection
		for _, revision := range collection.Outputs {
			next.Publications[revision] = c.state.Publications[revision]
		}
	}
	return c.commit(ctx, next, store.RunStatusRunning)
}

func (c *portCoordinator) retryPortInvocations(ctx context.Context) error {
	next, err := c.state.Clone()
	if err != nil {
		return err
	}
	changed := false
	for _, invocation := range next.Invocations {
		if invocation.Status != store.PortFailed && invocation.Status != store.PortCanceled {
			continue
		}
		invocation.Attempt++
		invocation.Status, invocation.Failure, invocation.EffectDispatched = store.PortPending, "", false
		changed = true
	}
	if !changed {
		return nil
	}
	return c.commit(ctx, next, store.RunStatusRunning)
}
