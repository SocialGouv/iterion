package blob

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

// artifactKey builds the canonical S3 key for an artifact body.
// Format: artifacts/<run_id>/<node_id>/<version>.json.
//
// IDs are sanitised against the same rules the filesystem store
// enforces. Any failure here is a programmer bug — IDs reach this
// path already sanitised — so we panic rather than threading an
// error through every blob write site.
func artifactKey(runID, nodeID string, version int) string {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		panic(err)
	}
	if err := store.SanitizePathComponent("node_id", nodeID); err != nil {
		panic(err)
	}
	if version < 0 {
		panic(fmt.Sprintf("blob: negative artifact version %d", version))
	}
	return fmt.Sprintf("artifacts/%s/%s/%d.json", runID, nodeID, version)
}
