package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// ManifestOriginPrefix marks the rows a reconciliation created from a bot's
// manifest ("manifest:<bot>").
const ManifestOriginPrefix = "manifest:"

// ManifestBot is what a reconciliation reads of one discovered bot.
type ManifestBot struct {
	Name        string
	Invocations []bundle.Invocation
}

// ReconcileReport counts what a reconciliation wrote.
type ReconcileReport struct {
	Created, Refreshed, Removed int
}

// ReconcileManifests makes the derived rows of a tenant match the bots'
// manifests:
//   - each board, schedule and keepalive invocation that derives a
//     subscription has its row under its derived id (EnsureDerived), unless
//     an operator deleted it (a tombstone);
//   - an existing derived row takes the manifest's fields again (match,
//     mode, cron, cadence, vars) while keeping the operator's: enabled, pins,
//     guard and concurrency policy;
//   - a derived row of a discovered bot whose invocation is gone is deleted,
//     with no tombstone, so the invocation comes back with its manifest. The
//     rows of a bot discovery did not return stay: a manifest that fails to
//     load is not a bot that left, and deleting would cost the operator's
//     choices (a disabled row would come back enabled).
//
// A connector subscription is never derived, refreshed or removed here: it
// listens to a connection an operator chose (ADR-168 §6). Every failure is
// joined into the error and the rest still reconciles.
func ReconcileManifests(ctx context.Context, st SubscriptionStore, tenantID string, bots []ManifestBot, now time.Time) (ReconcileReport, error) {
	var (
		rep  ReconcileReport
		errs []error
	)
	wanted := map[string]Subscription{}
	discovered := map[string]bool{}
	for _, b := range bots {
		discovered[b.Name] = true
		origin := ManifestOriginPrefix + b.Name
		for i, inv := range b.Invocations {
			id := DerivedSubscriptionID(tenantID, b.Name, inv.Kind, DerivedInvocationKey(b.Invocations, i), "")
			if sub, ok := DeriveFromInvocation(id, tenantID, b.Name, origin, inv, now); ok {
				wanted[id] = sub
			}
		}
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		sub := wanted[id]
		created, err := st.EnsureDerived(ctx, sub)
		if err != nil {
			errs = append(errs, fmt.Errorf("subscription %s (%s): %w", id, sub.BotID, err))
			continue
		}
		if created {
			rep.Created++
			continue
		}
		cur, err := st.Get(ctx, id)
		if errors.Is(err, ErrSubscriptionNotFound) {
			continue // tombstoned: an operator deleted it
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("subscription %s (%s): %w", id, sub.BotID, err))
			continue
		}
		if sameManifestFields(cur, sub) {
			continue
		}
		next := withManifestFields(cur, sub)
		next.UpdatedAt = now
		if err := st.Update(ctx, next); err != nil {
			errs = append(errs, fmt.Errorf("refresh subscription %s (%s): %w", id, sub.BotID, err))
			continue
		}
		rep.Refreshed++
	}
	rows, err := st.ListByTenant(ctx, tenantID)
	if err != nil {
		errs = append(errs, fmt.Errorf("list subscriptions: %w", err))
		return rep, errors.Join(errs...)
	}
	for _, row := range rows {
		if !IsDerivedID(row.ID) || row.Invocation == bundle.InvocationKindConnector || !discovered[row.BotID] {
			continue
		}
		if _, ok := wanted[row.ID]; ok {
			continue
		}
		if err := st.Delete(ctx, row.ID); err != nil && !errors.Is(err, ErrSubscriptionNotFound) {
			errs = append(errs, fmt.Errorf("remove subscription %s (%s): %w", row.ID, row.BotID, err))
			continue
		}
		rep.Removed++
	}
	return rep, errors.Join(errs...)
}

// DeriveFromInvocation derives the subscription a board, schedule or
// keepalive invocation describes; ok is false for every other kind, and for
// one the builders refuse (no block, no cron, a mode no effect honours).
func DeriveFromInvocation(id, tenantID, botID, origin string, inv bundle.Invocation, now time.Time) (Subscription, bool) {
	switch inv.Kind {
	case bundle.InvocationKindBoard:
		return FromBoardInvocation(id, tenantID, "", botID, origin, inv, now)
	case bundle.InvocationKindSchedule:
		return FromScheduleInvocation(id, tenantID, "", botID, origin, inv, now)
	case bundle.InvocationKindKeepalive:
		return FromKeepaliveInvocation(id, tenantID, "", botID, origin, inv, now)
	}
	return Subscription{}, false
}

// withManifestFields lays the fields a manifest owns — the ones the
// builders set — over the stored row, keeping everything else the row holds.
func withManifestFields(cur, derived Subscription) Subscription {
	next := cur
	next.BotID = derived.BotID
	next.Invocation = derived.Invocation
	next.Mode = derived.Mode
	next.ConsumeLabels = derived.ConsumeLabels
	next.Match = derived.Match
	next.ArgsVar = derived.ArgsVar
	next.Vars = derived.Vars
	next.Cron = derived.Cron
	next.IntervalSeconds = derived.IntervalSeconds
	next.Overlap = derived.Overlap
	next.StaleAfter = derived.StaleAfter
	return next
}

// manifestFields is the part of a row a manifest owns, in a form where an
// absent and an empty value read alike — a journal or a document round-trip
// turns one into the other, and must not read as a manifest change.
type manifestFields struct {
	BotID           string                `json:"bot_id,omitempty"`
	Invocation      bundle.InvocationKind `json:"invocation,omitempty"`
	Mode            bundle.ExecutionMode  `json:"mode,omitempty"`
	ConsumeLabels   bool                  `json:"consume_labels,omitempty"`
	Match           Matcher               `json:"match"`
	ArgsVar         string                `json:"args_var,omitempty"`
	Vars            map[string]string     `json:"vars,omitempty"`
	Cron            string                `json:"cron,omitempty"`
	IntervalSeconds int                   `json:"interval_seconds,omitempty"`
	Overlap         string                `json:"overlap,omitempty"`
	StaleAfter      string                `json:"stale_after,omitempty"`
}

func sameManifestFields(a, b Subscription) bool {
	view := func(s Subscription) []byte {
		raw, _ := json.Marshal(manifestFields{
			BotID: s.BotID, Invocation: s.Invocation, Mode: s.Mode, ConsumeLabels: s.ConsumeLabels,
			Match: s.Match, ArgsVar: s.ArgsVar, Vars: s.Vars, Cron: s.Cron,
			IntervalSeconds: s.IntervalSeconds, Overlap: s.Overlap, StaleAfter: s.StaleAfter,
		})
		return raw
	}
	return bytes.Equal(view(a), view(b))
}
