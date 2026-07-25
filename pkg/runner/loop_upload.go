package runner

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// uploadRunFiles copies the run's tool-produced artifact files from the
// runner-local scratch dir to the durable read backend (the Mongo store's
// S3 bridge). The server pod, which never saw this runner's disk, then
// serves them from the artifact-files panel. Best-effort: a store without
// the RunFilesUploader seam (filesystem dev store) no-ops cleanly, and an
// upload failure is logged, never fatal — the run's outcome is already
// decided. Runs on a background ctx carrying the run's tenant identity so
// a cancelled/timed-out run still flushes what it produced (mirrors
// recordRunGitMeta).
func (r *Runner) uploadRunFiles(_ context.Context, msg *queue.RunMessage) {
	up := store.AsRunFilesUploader(r.cfg.Store)
	if up == nil {
		return
	}
	idCtx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
	n, err := up.UploadRunFiles(idCtx, msg.RunID)
	if err != nil {
		r.cfg.Logger.Warn("runner: run %s: upload artifact files: %v", msg.RunID, err)
		return
	}
	if n > 0 {
		r.cfg.Logger.Info("runner: run %s: uploaded %d artifact file(s)", msg.RunID, n)
	}
}
