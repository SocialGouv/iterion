package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/SocialGouv/iterion/pkg/store"
)

// LegacyBareDigestMatches reports whether run r recorded the BARE digest of
// the workflow file at mainBot — the digest of the source bytes alone,
// which every surface that compiled a bundle's main.bot without promoting
// it recorded (the studio's file picker, the dispatcher's service path,
// the trigger launcher, rewind, the export) before the promotion reached
// them. Since then the digest folds the bundle's prompts/*.md and
// presets/*.md in, so such a run, resumed, compares a different digest
// against the one it recorded and would be refused as a source change
// that never happened. The class is narrow — a bundle that ships prompts/
// or presets/ — and its members are the long-lived runs: whats-next parks
// on human gates for hours.
//
// The run's record is NOT rewritten: a whole-document save outside the
// engine's claim would race every other writer of the run (an answer
// upload, an automatic resume, the orphan sweeper). The bare digest is
// accepted, and logged, on every resume of such a run instead.
func LegacyBareDigestMatches(r *store.Run, mainBot string) bool {
	if r == nil || r.WorkflowHash == "" || mainBot == "" {
		return false
	}
	src, err := os.ReadFile(mainBot)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(src)
	return hex.EncodeToString(sum[:]) == r.WorkflowHash
}
