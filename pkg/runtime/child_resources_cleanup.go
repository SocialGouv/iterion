package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

const resourceDrainTimeout = 5 * time.Second

func (e *Engine) finishRunResources(ctx context.Context, runID string, scope *runResourceScope, backup string, restore func() error, release func()) error {
	workspaceResourceGates.Lock()
	delete(workspaceResourceGates.active, resourceOwnerKey{scope.path, runID})
	workspaceResourceGates.Unlock()
	scope.setupRelease()
	drainCtx, cancel := context.WithTimeout(context.Background(), resourceDrainTimeout)
	err := scope.gate.sem.Acquire(drainCtx, resourceWriterWeight)
	cancel()
	if err != nil {
		cause := fmt.Errorf("resource drain for run %s exceeded %s; snapshot retained at %s until outstanding executions exit: %w", runID, resourceDrainTimeout, backup, err)
		recorded := e.recordResourceRestoreFailure(ctx, runID, cause)
		// Return promptly without restoring underneath an abandoned executor. Keep
		// the outer workspace lease and backup alive. The last reader's eventual
		// exit allows restoration and releases the quarantine; never publish success
		// from this background cleanup after reporting a failure to the caller.
		go func() {
			_ = scope.gate.sem.Acquire(context.Background(), resourceWriterWeight)
			defer scope.gate.sem.Release(resourceWriterWeight)
			defer release()
			if err := restore(); err != nil {
				_ = e.recordResourceRestoreFailure(context.Background(), runID, err)
			}
		}()
		return recorded
	}
	defer scope.gate.sem.Release(resourceWriterWeight)
	defer release()
	if err := restore(); err != nil {
		return e.recordResourceRestoreFailure(ctx, runID, err)
	}
	if scope.reachedDone {
		return e.publishRunFinished(context.WithoutCancel(ctx), runID)
	}
	return nil
}

func (e *Engine) recordResourceRestoreFailure(ctx context.Context, runID string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), childResourceIOTimeout)
	defer cancel()
	err := e.store.UpdateRunStatusCoded(ctx, runID, store.RunStatusFailedResumable, cause.Error(), store.FailureResourceRestore)
	if err != nil && e.logger != nil {
		e.logger.Error("runtime: could not record resource restoration failure for %s: %v (cause: %v)", runID, err, cause)
	}
	e.emitSetupFailure(ctx, runID, "resource restoration", store.RunStatusFailedResumable, cause.Error(), store.FailureResourceRestore)
	return &RuntimeError{Code: ErrCodeResourceRestore, Message: "resource restoration failed", Cause: errors.Join(cause, err)}
}
