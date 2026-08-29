package sessionboard

import (
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Enabled reports whether the LLM curation layer should run, read from
// ITERION_SESSION_BOARD (on|true|1 => enabled; anything else / unset =>
// off). The deterministic task-list board (Phase 1) is always on in the
// studio and needs no gating; only the token-spending curation coordinator
// is opt-in. A per-bot DSL `session_board:` field is a planned follow-on.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_SESSION_BOARD"))) {
	case "on", "true", "1", "yes":
		return true
	default:
		return false
	}
}

// ModelFromEnv returns the curation model pin from
// ITERION_DEFAULT_SESSIONBOARD_MODEL, or "" to let the evaluator
// auto-detect a reachable provider.
func ModelFromEnv() string {
	return strings.TrimSpace(ir.LookupEnv("ITERION_DEFAULT_SESSIONBOARD_MODEL"))
}
