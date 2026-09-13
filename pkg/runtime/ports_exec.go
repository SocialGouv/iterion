package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// PortBudgetEstimator is an optional NodeExecutor capability for paid work
// admitted under finite token/cost limits. The reservation is an upper bound
// supplied by the executor, not an LLM guess or a model's output-token limit.
type PortBudgetEstimator interface {
	EstimatePortExecution(context.Context, ir.Node, map[string]any) (store.PortBudgetAmount, error)
}

func (c *portCoordinator) admitReady(ctx context.Context) (bool, error) {
	if c.limit > 0 && len(c.active) >= c.limit {
		return false, nil
	}
	budgetBlocked := false
	for _, id := range portInvocationIDs(c.state, c.engine.workflow.Ports) {
		invocation := c.state.Invocations[id]
		if invocation.Status != store.PortPending {
			continue
		}
		instance := c.engine.workflow.Ports.Nodes[invocation.Node]
		bindings, ready := resolvePortBindings(c.state, instance)
		if !ready {
			continue
		}
		inputs, err := decodePortInputs(c.state, instance, bindings, invocation.MapIndex)
		if err != nil {
			return false, err
		}
		reservation, available, err := c.reserveBudget(ctx, instance, inputs)
		if err != nil {
			return false, err
		}
		if !available {
			budgetBlocked = true
			continue
		}
		needs := append([]string(nil), ir.NodeNeeds(c.engine.workflow.Nodes[invocation.Node])...)
		for _, effect := range instance.Policy.Effects {
			if effect.Resource != "" {
				needs = append(needs, effect.Resource)
			}
		}
		release, resources, available, err := c.acquireResources(needs)
		if err != nil {
			return false, err
		}
		if !available {
			continue
		}
		var area *portOutputArea
		if needsPortOutputArea(instance) {
			area, err = c.engine.preparePortOutputArea(ctx, c.rs.runID, invocation)
			if err != nil {
				release()
				return false, err
			}
			if err := c.engine.materializePortInputs(ctx, instance, bindings, inputs, c.state, area); err != nil {
				release()
				return false, err
			}
		}
		next, err := c.state.Clone()
		if err != nil {
			release()
			return false, err
		}
		admitted := next.Invocations[id]
		admitted.Inputs = bindings
		admitted.Identity.Inputs, err = portIdentity(inputs)
		if err != nil {
			release()
			return false, err
		}
		admitted.Status, admitted.Resources = store.PortAdmitted, resources
		next.Budget.Reservations[id] = reservation
		if err := c.commit(ctx, next, store.RunStatusRunning); err != nil {
			release()
			return false, err
		}
		next, err = c.state.Clone()
		if err != nil {
			release()
			return false, err
		}
		next.Invocations[id].Status = store.PortRunning
		next.Invocations[id].EffectDispatched = len(instance.Contract.Effects) > 0
		if err := c.commit(ctx, next, store.RunStatusRunning); err != nil {
			release()
			return false, err
		}
		local := cloneRunStateForBranch(c.rs)
		local.correctionScope = "ports:" + id + ":attempt:" + strconv.Itoa(invocation.Attempt)
		local.ctx = ctx
		c.active[id] = release
		workerCtx := ctx
		if area != nil {
			workerCtx = model.WithInvocationFiles(ctx, model.InvocationFiles{HostDir: area.HostDir, SandboxDir: area.SandboxDir, HasOutputFiles: hasPortFileOutputs(instance), Inputs: area.Inputs})
		}
		if len(resources) > 0 {
			inputs[leaseInputKey] = resources
		}
		go func() {
			output, err := c.engine.executePortInvocation(workerCtx, local, instance, invocation, inputs)
			c.done <- portCompletion{id: id, output: output, err: err, area: area}
		}()
		return true, nil // observe completions before considering another admission
	}
	if budgetBlocked && len(c.active) == 0 {
		return false, fmt.Errorf("%w: native root cannot reserve the next invocation", ErrBudgetExceeded)
	}
	return false, nil
}

func (c *portCoordinator) reserveBudget(ctx context.Context, instance *ir.PortInstance, inputs map[string]any) (store.PortBudgetAmount, bool, error) {
	reservation := store.PortBudgetAmount{Iterations: 1}
	budget := c.rs.budget.Status()
	paid := false
	for _, effect := range instance.Contract.Effects {
		paid = paid || effect.Paid
	}
	if estimator, ok := c.engine.executor.(PortBudgetEstimator); ok {
		quoted, err := estimator.EstimatePortExecution(ctx, c.engine.workflow.Nodes[instance.ID], inputs)
		if err != nil {
			return reservation, false, err
		}
		reservation.Tokens, reservation.CostUSD = quoted.Tokens, quoted.CostUSD
	} else if paid && (budget.MaxTokens > 0 || budget.MaxCostUSD > 0) {
		return reservation, false, fmt.Errorf("runtime: paid node %s needs an executor budget reservation before finite token/cost admission", instance.ID)
	}
	if reservation.Tokens < 0 || reservation.CostUSD < 0 || math.IsNaN(reservation.CostUSD) || math.IsInf(reservation.CostUSD, 0) {
		return reservation, false, fmt.Errorf("runtime: invalid executor reservation for %s", instance.ID)
	}
	used := c.state.Budget.Consumed
	for _, active := range c.state.Budget.Reservations {
		used.Tokens += active.Tokens
		used.CostUSD += active.CostUSD
		used.Iterations += active.Iterations
	}
	if budget.MaxIterations > 0 && used.Iterations+1 > int64(budget.MaxIterations) ||
		budget.MaxTokens > 0 && used.Tokens+reservation.Tokens > int64(budget.MaxTokens) ||
		budget.MaxCostUSD > 0 && used.CostUSD+reservation.CostUSD > budget.MaxCostUSD {
		return reservation, false, nil
	}
	if remaining, bounded := c.rs.budget.RemainingDuration(); bounded && remaining <= 0 {
		return reservation, false, nil
	}
	if budget.MaxCostUSD > 0 && c.state.Budget.UnpricedNodes > 0 {
		return reservation, false, fmt.Errorf("%w: native root has unpriced paid usage", ErrBudgetExceeded)
	}
	return reservation, true, nil
}

func (e *Engine) executePortInvocation(ctx context.Context, rs *runState, instance *ir.PortInstance, invocation *store.PortInvocation, inputs map[string]any) (output map[string]any, resultErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resultErr = fmt.Errorf("runtime: native invocation %s panicked: %v", invocation.ID, recovered)
		}
	}()
	node, err := ir.ClonePortNode(e.workflow.Nodes[instance.ID], invocation.ID)
	if err != nil {
		return nil, err
	}
	if err := e.emit(ctx, rs.runID, store.EventNodeStarted, instance.ID, map[string]any{"invocation_id": invocation.ID, "attempt": invocation.Attempt, "map_index": invocation.MapIndex}); err != nil {
		return nil, err
	}
	execCtx := e.execContext(ctx, rs, invocation.ID)
	if remaining, bounded := rs.budget.RemainingDuration(); bounded {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, remaining)
		defer cancel()
	}
	switch n := node.(type) {
	case *ir.ComputeNode:
		output, err = e.computeOutputWithInput(rs, invocation.ID, n, rs.scope(), inputs)
	default:
		output, err = e.executor.Execute(execCtx, node, inputs)
	}
	if err != nil {
		return output, err
	}
	return e.correctOutputWithValidation(execCtx, rs, invocation.ID, node, output, func(candidate map[string]any) error {
		if err := ir.ValidatePublicValues(instance.Contract.Outputs, candidate); err != nil {
			return fmt.Errorf("runtime: output of %s: %w", instance.ID, err)
		}
		return instance.Contract.ValidateCriteria("output", candidate)
	})
}

func (c *portCoordinator) complete(ctx context.Context, result portCompletion) error {
	next, err := c.state.Clone()
	if err != nil {
		return err
	}
	invocation := next.Invocations[result.id]
	instance := c.engine.workflow.Ports.Nodes[invocation.Node]
	tokens, cost := extractUsage(result.output)
	next.Budget.Consumed.Tokens += int64(tokens)
	next.Budget.Consumed.CostUSD += cost
	next.Budget.Consumed.Iterations++
	if tokens > 0 && cost == 0 {
		next.Budget.UnpricedTokens += int64(tokens)
		next.Budget.UnpricedNodes++
	}
	delete(next.Budget.Reservations, result.id)
	invocation.Resources = nil
	// Capture every declared file into immutable storage before committing any
	// publication. A capture failure is an invocation failure, not a half-
	// published output or an indefinitely running checkpoint.
	captured := map[string][]store.PortFileRef{}
	if result.err == nil {
		for _, port := range instance.Contract.Outputs {
			value, present := result.output[port.Name]
			if !present || value == nil || port.Type.Name != "file" {
				continue
			}
			result.output[port.Name], captured[port.Name], err = c.engine.capturePortFiles(ctx, c.rs.runID, invocation, port, value, result.area, c.state)
			if err != nil {
				result.err = err
				break
			}
		}
	}
	if result.err != nil {
		invocation.Status, invocation.Failure = store.PortFailed, result.err.Error()
		if errors.Is(result.err, context.Canceled) {
			invocation.Status = store.PortCanceled
		}
		if invocation.EffectDispatched {
			invocation.Status = store.PortUncertain
			idempotent := len(instance.Policy.Effects) > 0
			for _, effect := range instance.Policy.Effects {
				idempotent = idempotent && effect.Recovery == "idempotent"
			}
			if idempotent {
				invocation.Status = store.PortFailed
				invocation.RecoveryDecision = "captured technical policy declares idempotent recovery"
				invocation.RecoveryAttempt = invocation.Attempt
			}
		}
	} else {
		invocation.Status = store.PortSucceeded
		invocation.Outputs = map[string]string{}
		for _, port := range instance.Contract.Outputs {
			value, present := result.output[port.Name]
			if !present {
				continue
			}
			revision := portOutputRevision(invocation, port.Name)
			if err := addPortValue(next, revision, invocation.ID, port.Name, invocation.Attempt, value); err != nil {
				return err
			}
			next.Publications[revision].Files = captured[port.Name]
			invocation.Outputs[port.Name] = revision
		}
	}
	next.Budget.ElapsedNS = max(next.Budget.ElapsedNS, time.Since(c.rs.startedAt).Nanoseconds())
	if err := c.commit(ctx, next); err != nil {
		return err
	}
	c.rs.budget.Restore(int(next.Budget.Consumed.Tokens), next.Budget.Consumed.CostUSD, int(next.Budget.Consumed.Iterations), time.Duration(next.Budget.ElapsedNS), int(next.Budget.UnpricedTokens), int(next.Budget.UnpricedNodes))
	if err := c.engine.emit(ctx, c.rs.runID, store.EventNodeFinished, invocation.Node, map[string]any{"invocation_id": invocation.ID, "attempt": invocation.Attempt, "map_index": invocation.MapIndex, "status": invocation.Status, "error": invocation.Failure}); err != nil {
		return errors.Join(result.err, err)
	}
	return result.err
}
