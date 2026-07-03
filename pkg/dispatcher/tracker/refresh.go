package tracker

import (
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// refreshStateByID drives the RefreshStates pattern shared by the gitlab
// and forgejo adapters: parse each id, fetch its current WorkflowState,
// and log + skip (rather than fail the whole sweep) on a per-issue
// error — a transient blip on one issue must not make the dispatcher
// treat the rest as "disappeared from tracker" (which would cancel
// their in-flight runs).
func refreshStateByID(ids []string, parseID func(id string) (int, bool), fetchState func(num int) (string, error), logger *iterlog.Logger, logPrefix string) map[string]string {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		num, ok := parseID(id)
		if !ok {
			continue
		}
		state, err := fetchState(num)
		if err != nil {
			if logger != nil {
				logger.Warn("%s: issue %d: %v", logPrefix, num, err)
			}
			continue
		}
		if state != "" {
			out[id] = state
		}
	}
	return out
}
