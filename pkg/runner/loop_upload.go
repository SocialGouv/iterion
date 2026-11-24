package runner

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// uploadRunFilesBudget bounds the artifact upload end to end. It runs after
// the engine returned, with the run's lease still held: an object store that
// never answers must not hold that lease. What it has not uploaded by then
// is logged as failed, like any other upload failure.
const uploadRunFilesBudget = 5 * time.Minute

// uploadRunFiles copies the run's tool-produced artifact files from the
// runner-local scratch dir to the durable read backend (the Mongo store's
// S3 bridge). The server pod, which never saw this runner's disk, then
// serves them from the artifact-files panel. Best-effort: a store without
// either upload seam (filesystem dev store) no-ops cleanly, and an
// upload failure is logged, never fatal — the run's outcome is already
// decided. Retains the original data binding while detaching cancellation so
// a cancelled/timed-out run still flushes what it produced (mirrors
// recordRunGitMeta), within uploadRunFilesBudget.
func (r *Runner) uploadRunFiles(ctx context.Context, msg *queue.RunMessage) {
	r.uploadRunFilesWithGrant(ctx, msg, nil)
}
func (r *Runner) uploadRunFilesWithGrant(ctx context.Context, msg *queue.RunMessage, grant store.RunFilesGrant) {
	up := store.AsRunFilesUploader(r.cfg.Store)
	granted := store.AsRunFilesGrantUploader(r.cfg.Store)
	if up == nil && granted == nil {
		return
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadRunFilesBudget)
	defer cancel()
	idCtx := store.WithIdentity(bounded, msg.TenantID, msg.OwnerID)
	var n int
	var err error
	if grant != nil {
		idCtx, err = grant.Bind(idCtx, msg.RunID)
	}
	if err == nil {
		if binding, ok := store.RunDataBindingFrom(idCtx); ok && binding.Header().Protocol == store.IncarnationRunData {
			if grant == nil || granted == nil {
				err = store.ErrRunDataCapability
			} else {
				n, err = granted.UploadRunFilesFrom(idCtx, msg.RunID, grant)
			}
		} else if up != nil {
			n, err = up.UploadRunFiles(idCtx, msg.RunID)
		}
	}
	if err != nil {
		r.cfg.Logger.Warn("runner: run %s: upload artifact files: %v", msg.RunID, err)
		return
	}
	if n > 0 {
		r.cfg.Logger.Info("runner: run %s: uploaded %d artifact file(s)", msg.RunID, n)
	}
}
