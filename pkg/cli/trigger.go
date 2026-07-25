package cli

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// buildLocalTriggerStore discovers the bots under botsPaths and derives a
// board trigger.Subscription from each kind=board invocation that carries a
// board: block (see trigger.FromBoardInvocation — a plain kind=board target
// with no block stays poll-only). It returns nil when no such invocation
// exists, so the server leaves the trigger spine off and the dispatcher poll
// remains the only board path. This is the discovery-driven local wiring: a
// bot opts into event-driven promotion purely by adding a board: block to its
// manifest — no engine or CLI edit.
//
// The store is in-memory (the local single-host scope, tenant/repo ""), seeded
// fresh each process start from the manifests; operator-authored subscriptions
// added via the studio /triggers CRUD live here too but are not persisted
// across restarts yet (a file-backed store is the follow-on).
func buildLocalTriggerStore(botsPaths []string, logger *iterlog.Logger) trigger.SubscriptionStore {
	entries, err := botregistry.List(botregistry.ListOptions{Paths: botsPaths})
	if err != nil {
		if logger != nil {
			logger.Warn("trigger: bot discovery for triggers failed: %v", err)
		}
		return nil
	}
	st := trigger.NewMemorySubscriptionStore()
	now := time.Now().UTC()
	n := 0
	for _, e := range entries {
		for _, inv := range e.Invocations {
			origin := "manifest:" + e.Name
			if sub, ok := trigger.FromBoardInvocation(uuid.NewString(), "", "", e.Name, origin, inv, now); ok {
				if err := st.Create(context.Background(), sub); err == nil {
					n++
				}
				continue
			}
			if sub, ok := trigger.FromScheduleInvocation(uuid.NewString(), "", "", e.Name, origin, inv, now); ok {
				if err := st.Create(context.Background(), sub); err == nil {
					n++
				}
				continue
			}
			if sub, ok := trigger.FromKeepaliveInvocation(uuid.NewString(), "", "", e.Name, origin, inv, now); ok {
				if err := st.Create(context.Background(), sub); err == nil {
					n++
				}
			}
		}
	}
	if n == 0 {
		return nil
	}
	if logger != nil {
		logger.Info("trigger: %d subscription(s) loaded from bot manifests", n)
	}
	return st
}
