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
// the RunFilesUploader seam (filesystem dev store) no-ops cleanly, and an
// upload failure is logged, never fatal — the run's outcome is already
// decided. Runs on a background ctx carrying the run's tenant identity so
// a cancelled/timed-out run still flushes what it produced (mirrors
// recordRunGitMeta), within uploadRunFilesBudget.
func (r *Runner) uploadRunFiles(_ context.Context, msg *queue.RunMessage) {
	up := store.AsRunFilesUploader(r.cfg.Store)
	if up == nil {
		return
	}
	bounded, cancel := context.WithTimeout(context.Background(), uploadRunFilesBudget)
	defer cancel()
	idCtx := store.WithIdentity(bounded, msg.TenantID, msg.OwnerID)
	n, err := up.UploadRunFiles(idCtx, msg.RunID)
	if err != nil {
		r.cfg.Logger.Warn("runner: run %s: upload artifact files: %v", msg.RunID, err)
		return
	}
	if n > 0 {
		r.cfg.Logger.Info("runner: run %s: uploaded %d artifact file(s)", msg.RunID, n)
	}
}
