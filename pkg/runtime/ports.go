package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// portCoordinator lives inside the existing Engine. It is the sole writer of
// native execution state. Workers execute through NodeExecutor and return
// candidates; only this coordinator can publish them and unblock consumers.
type portCoordinator struct {
	engine *Engine
	rs     *runState
	state  *store.PortExecution
	active map[string]func()
	done   chan portCompletion
	limit  int
}

type portCompletion struct {
	id     string
	output map[string]any
	err    error
	area   *portOutputArea
}

func (e *Engine) checkNativeSemanticIdentity(runID string, run *store.Run) error {
	if e.workflow == nil {
		return fmt.Errorf("runtime: missing workflow")
	}
	semantics := e.workflow.RuntimeSemantics
	if semantics != "" && semantics != ir.RuntimeSemanticsPortsV1 {
		return fmt.Errorf("runtime: unsupported workflow semantics %q: %w", semantics, store.ErrRunSemantics)
	}
	if err := store.ValidateRunID(runID); err != nil {
		return err
	}
	if (semantics == ir.RuntimeSemanticsPortsV1) != store.IsNativeRunID(runID) {
		return fmt.Errorf("runtime: workflow and run %s select different interpreters: %w", runID, store.ErrRunSemantics)
	}
	if run != nil && run.RuntimeSemantics != semantics {
		return fmt.Errorf("runtime: run %s cannot change interpreter, including with --force: %w", runID, store.ErrRunSemantics)
	}
	return nil
}

func (e *Engine) execPortGraph(ctx context.Context, rs *runState) error {
	state, err := e.newPortExecution(ctx, rs.runID, rs.runInputs)
	if err != nil {
		return err
	}
	if err := store.SavePortExecution(ctx, e.store, rs.runID, 0, state); err != nil {
		return err
	}
	limit := 0 // zero inherits the graph's available parallelism
	if e.workflow.Budget != nil {
		limit = e.workflow.Budget.MaxParallelBranches
	}
	coordinator := &portCoordinator{engine: e, rs: rs, state: state, active: map[string]func(){}, done: make(chan portCompletion), limit: limit}
	return coordinator.execute(ctx)
}

func (c *portCoordinator) commit(ctx context.Context, next *store.PortExecution, allowedStatuses ...store.RunStatus) error {
	next.Revision = c.state.Revision + 1
	// Metadata writers (notably correction ledgers) may advance the enclosing
	// run CAS while this coordinator's native revision remains unchanged.
	// Retry only that case; another coordinator's revision is never replayed.
	for range 3 {
		err := store.SavePortExecution(ctx, c.engine.store, c.rs.runID, c.state.Revision, next, allowedStatuses...)
		if err == nil {
			c.state = next
			return nil
		}
		if !errors.Is(err, store.ErrRunConflict) {
			return err
		}
		run, loadErr := c.engine.store.LoadRun(ctx, c.rs.runID)
		if loadErr != nil {
			return loadErr
		}
		if len(allowedStatuses) > 0 && run.Status != store.RunStatusRunning {
			if run.Status == store.RunStatusPausedOperator {
				return ErrRunPausedOperator
			}
			if run.Status == store.RunStatusCancelled {
				return context.Canceled
			}
			return fmt.Errorf("runtime: native root is no longer running: %w", err)
		}
		if run.PortExecution == nil || run.PortExecution.Revision != c.state.Revision {
			return err
		}
	}
	return store.ErrRunConflict
}

func (c *portCoordinator) execute(ctx context.Context) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stop error
	paused := false
	for {
		c.engine.drainOverrides(c.rs)
		select {
		case result := <-c.done:
			if release := c.active[result.id]; release != nil {
				release()
			}
			delete(c.active, result.id)
			if err := c.complete(context.WithoutCancel(ctx), result); err != nil && stop == nil {
				stop = err
				cancel()
			}
		default:
		}
		if ctx.Err() != nil && stop == nil {
			stop = ctx.Err()
			cancel()
		}
		if c.engine.pauseSignal != nil {
			select {
			case <-c.engine.pauseSignal:
				paused = true
			default:
			}
		}
		progress := false
		if stop == nil && !paused {
			var err error
			progress, err = c.expandMaps(workCtx)
			if err == nil {
				var collected bool
				collected, err = c.collect(workCtx)
				progress = progress || collected
			}
			if err == nil {
				var admitted bool
				admitted, err = c.admitReady(workCtx)
				progress = progress || admitted
			}
			if err != nil {
				stop = err
				cancel()
			}
		}
		if len(c.active) == 0 {
			if stop != nil || paused {
				return c.finish(ctx, stop, paused)
			}
			if c.allSucceeded() {
				if err := c.exportResults(ctx); err != nil {
					return c.finish(ctx, err, false)
				}
				return c.finish(ctx, nil, false)
			}
			if !progress {
				return c.finish(ctx, fmt.Errorf("runtime: native graph has unresolved inputs or exhausted admission limits"), false)
			}
			continue
		}
		if progress && stop == nil && !paused {
			continue // newly committed collections may make more nodes ready
		}
		pauseSignal := c.engine.pauseSignal
		if paused {
			pauseSignal = nil
		}
		select {
		case result := <-c.done:
			release := c.active[result.id]
			delete(c.active, result.id)
			if release != nil {
				release()
			}
			// Drain all admitted workers after cancellation. They never outlive
			// the root and mutate a finalized run or a reclaimed workspace.
			writeCtx := context.WithoutCancel(ctx)
			if err := c.complete(writeCtx, result); err != nil && stop == nil {
				stop = err
				cancel()
			}
		case <-workCtx.Done():
			if stop == nil {
				stop = workCtx.Err()
			}
			// A canceled context is always ready. Drain one completion rather
			// than spinning on it while a cooperative executor unwinds.
			result := <-c.done
			if release := c.active[result.id]; release != nil {
				release()
			}
			delete(c.active, result.id)
			if err := c.complete(context.WithoutCancel(ctx), result); err != nil && stop == nil {
				stop = err
			}
		case <-pauseSignal:
			paused = true
		}
	}
}

func (c *portCoordinator) expandMaps(ctx context.Context) (bool, error) {
	changed := false
	for _, id := range c.engine.workflow.Ports.Order {
		instance := c.engine.workflow.Ports.Nodes[id]
		if instance.MapInput == "" || c.state.Collections[id] != nil {
			continue
		}
		bindings, ready := resolvePortBindings(c.state, instance)
		if !ready {
			continue
		}
		axis := c.state.Publications[bindings[instance.MapInput]]
		if axis == nil {
			return changed, fmt.Errorf("runtime: mapping input %s.%s is absent", id, instance.MapInput)
		}
		value, err := ir.DecodePortValue(axis.Data)
		if err != nil {
			return changed, err
		}
		items, ok := value.([]any)
		if !ok {
			return changed, fmt.Errorf("runtime: mapping input %s.%s is not an array", id, instance.MapInput)
		}
		limit := instance.Policy.MaxMapItems
		if limit == 0 {
			limit = 1024
			if raw := os.Getenv("ITERION_PORTS_MAX_MAP_ITEMS"); raw != "" {
				limit, err = strconv.Atoi(raw)
				if err != nil || limit < 0 {
					return changed, fmt.Errorf("runtime: ITERION_PORTS_MAX_MAP_ITEMS must be a non-negative integer")
				}
			}
		}
		if limit > 0 && len(items) > limit {
			return changed, fmt.Errorf("runtime: map %s has %d items, exceeding max_map_items %d", id, len(items), limit)
		}
		// Cardinality is checked before allocating any item record.
		next, err := c.state.Clone()
		if err != nil {
			return changed, err
		}
		collection := &store.PortCollection{ID: id, Node: id, Items: make([]string, len(items))}
		for index := range items {
			itemID := "@" + id
			if c.state.Generation > 1 {
				itemID += "#" + strconv.FormatUint(c.state.Generation, 10)
			}
			itemID += "[" + strconv.Itoa(index) + "]"
			invocation, err := c.engine.newPortInvocation(instance, itemID, &index)
			if err != nil {
				return changed, err
			}
			invocation.Inputs = bindings
			collection.Items[index] = itemID
			next.Invocations[itemID] = invocation
		}
		next.Collections[id] = collection
		if err := c.commit(ctx, next); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func (c *portCoordinator) collect(ctx context.Context) (bool, error) {
	changed := false
	for _, id := range c.engine.workflow.Ports.Order {
		collection := c.state.Collections[id]
		if collection == nil || collection.Complete {
			continue
		}
		ready := true
		for _, item := range collection.Items {
			ready = ready && c.state.Invocations[item].Status == store.PortSucceeded
		}
		if !ready {
			continue
		}
		next, err := c.state.Clone()
		if err != nil {
			return changed, err
		}
		next.Collections[id].Complete = true
		next.Collections[id].Outputs = map[string]string{}
		for _, port := range c.engine.workflow.Ports.Nodes[id].Contract.Outputs {
			values := make([]any, len(collection.Items))
			var files []store.PortFileRef
			for index, item := range collection.Items {
				invocation := c.state.Invocations[item]
				value := c.state.Publications[invocation.Outputs[port.Name]]
				if value == nil {
					return changed, fmt.Errorf("runtime: mapped output %s.%s is absent at item %d", id, port.Name, index)
				}
				values[index], err = ir.DecodePortValue(value.Data)
				if err != nil {
					return changed, err
				}
				files = append(files, value.Files...)
			}
			lifted := port
			lifted.Type = port.Type.Array()
			lifted.MinItems, lifted.MaxItems = nil, nil // per-item bounds were already checked
			lifted.Nullable = false
			if err := lifted.ValidateValue(values); err != nil {
				return changed, err
			}
			revision := "collection:" + id + ":" + port.Name
			if err := addPortValue(next, revision, id, port.Name, 0, values); err != nil {
				return changed, err
			}
			next.Publications[revision].Files = files
			next.Collections[id].Outputs[port.Name] = revision
		}
		if err := c.commit(ctx, next); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func (c *portCoordinator) allSucceeded() bool {
	for _, id := range c.engine.workflow.Ports.Order {
		if c.engine.workflow.Ports.Nodes[id].MapInput != "" {
			if c.state.Collections[id] == nil || !c.state.Collections[id].Complete {
				return false
			}
		} else if c.state.Invocations[id].Status != store.PortSucceeded {
			return false
		}
	}
	return true
}

func (c *portCoordinator) exportResults(ctx context.Context) error {
	next, err := c.state.Clone()
	if err != nil {
		return err
	}
	values := map[string]any{}
	for name, source := range c.engine.workflow.Ports.Exports {
		revision, ready := portSource(c.state, source)
		if !ready {
			return fmt.Errorf("runtime: workflow output %s is not committed", name)
		}
		if revision != "" {
			next.Exports[name] = revision
			values[name], err = ir.DecodePortValue(c.state.Publications[revision].Data)
			if err != nil {
				return err
			}
		}
	}
	if err := ir.ValidatePublicValues(c.engine.workflow.PublicContract.Outputs, values); err != nil {
		return err
	}
	if err := c.engine.workflow.PublicContract.ValidateCriteria("output", values); err != nil {
		return err
	}
	return c.commit(ctx, next)
}

func (c *portCoordinator) finish(ctx context.Context, cause error, paused bool) error {
	writeCtx := context.WithoutCancel(ctx)
	status, event := store.RunStatusFinished, store.EventRunFinished
	message := ""
	if cause != nil {
		status, event, message = store.RunStatusFailedResumable, store.EventRunFailed, cause.Error()
		if errors.Is(cause, context.Canceled) && !errors.Is(context.Cause(ctx), ErrRunInterrupted) {
			status, event = store.RunStatusCancelled, store.EventRunCancelled
		}
	} else if paused {
		status, event = store.RunStatusPausedOperator, store.EventRunPaused
	}
	changed, err := c.engine.store.UpdateRunStatusIf(writeCtx, c.rs.runID, status, message, []store.RunStatus{store.RunStatusRunning})
	if err != nil {
		return errors.Join(cause, err)
	}
	if !changed {
		run, err := c.engine.store.LoadRun(writeCtx, c.rs.runID)
		if err != nil {
			return errors.Join(cause, err)
		}
		switch run.Status {
		case store.RunStatusCancelled:
			return context.Canceled
		case store.RunStatusPausedOperator:
			return ErrRunPausedOperator
		default:
			return errors.Join(cause, fmt.Errorf("runtime: root finalization lost its status claim: %w", store.ErrRunConflict))
		}
	}
	if err := c.engine.emit(writeCtx, c.rs.runID, event, "", map[string]any{"runtime_semantics": ir.RuntimeSemanticsPortsV1, "error": message}); err != nil {
		return errors.Join(cause, err)
	}
	if cause == nil && paused {
		return ErrRunPausedOperator
	}
	return cause
}

func (c *portCoordinator) acquireResources(needs []string) (func(), map[string]string, bool, error) {
	names := append([]string(nil), needs...)
	sort.Strings(names)
	held := map[string]string{}
	release := func() {
		for name, token := range held {
			c.rs.resourceSemaphores[name] <- token
		}
	}
	for _, name := range names {
		if _, acquired := held[name]; acquired {
			continue
		}
		resource := c.rs.resourceSemaphores[name]
		if resource == nil {
			release()
			return nil, nil, false, fmt.Errorf("runtime: undeclared native resource %s", name)
		}
		select {
		case token := <-resource:
			held[name] = token
		default:
			release()
			return nil, nil, false, nil
		}
	}
	return release, held, true, nil
}
