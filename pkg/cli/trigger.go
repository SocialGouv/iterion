package cli

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// openLocalTriggerStore opens the local subscription journal under storeDir
// and reconciles it with the bots discovered under botsPaths
// (trigger.ReconcileManifests): each kind=board invocation carrying a board:
// block, each schedule and each keepalive invocation keeps one row under its
// derived id, refreshed from its manifest, unless an operator deleted it.
// Operator subscriptions, edits, pins and tombstones persist across restarts,
// and a process sharing the store directory (`iterion dispatch` beside the
// studio) reads the same journal.
//
// The store is returned whatever the manifests declare — an operator's own
// subscriptions need the spine as much as a manifest's do — and a failed
// discovery leaves the stored rows as they stand rather than reconciling
// them against nothing.
func openLocalTriggerStore(storeDir string, botsPaths []string, logger *iterlog.Logger) trigger.SubscriptionStore {
	st := trigger.NewFileSubscriptionStore(trigger.SubscriptionJournalPath(storeDir))
	entries, err := botregistry.List(botregistry.ListOptions{Paths: botsPaths})
	if err != nil {
		if logger != nil {
			logger.Warn("trigger: bot discovery for triggers failed, the stored subscriptions stand unreconciled: %v", err)
		}
		return st
	}
	bots := make([]trigger.ManifestBot, 0, len(entries))
	for _, e := range entries {
		bots = append(bots, trigger.ManifestBot{Name: e.Name, Invocations: e.Invocations})
	}
	rep, err := trigger.ReconcileManifests(context.Background(), st, "", bots, time.Now().UTC())
	if err != nil && logger != nil {
		logger.Warn("trigger: reconciling subscriptions with the bot manifests: %v", err)
	}
	if logger != nil && rep != (trigger.ReconcileReport{}) {
		logger.Info("trigger: subscriptions reconciled with the bot manifests: %d created, %d refreshed, %d removed", rep.Created, rep.Refreshed, rep.Removed)
	}
	return st
}
