package runview

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/runtime"
)

// PromoteForHandoff runs a launch's attachment promotion on a path that
// hands the run to ANOTHER process — the cloud publisher's queue message,
// a detached runner's subprocess. The in-process engine promotes for
// itself (runtime.WithAttachmentPromote); a process that only publishes
// or spawns has to do it here, once the run row exists and before the
// hand-off, or the run starts without the files its launch named and
// nothing says so. A nil promote is a launch that names no upload.
func PromoteForHandoff(ctx context.Context, runID string, promote runtime.AttachmentPromoteFunc) error {
	if promote == nil {
		return nil
	}
	if err := promote(ctx, runID); err != nil {
		return fmt.Errorf("promote the launch's attachments for run %s: %w", runID, err)
	}
	return nil
}
