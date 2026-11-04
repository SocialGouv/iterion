package forge

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/SocialGouv/iterion/pkg/internal/storekit"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// reconcileStoredPins closes the provision's half of the credential pin
// window (#2644): once cfg is stored, it re-reads every record its pins
// name and clears a revoked one from THIS row. A record's delete sweeps the
// pins that name it from the tenant's rows as each row stands when the
// sweep reaches it, and a later full-document write conflicts with the bump
// the sweep made. Two provision writes land pins no such bump guards, so
// each runs this re-check once its row is stored:
//
//   - the migration carry: carryOperatorWebhookSettings copies prev's key
//     and secret pins by value into the row of a NEW hook identity, a write
//     that commits against the candidate row's version, not prev's. A
//     record deleted while the provision ran — its sweep listing the
//     tenant's rows before the new row existed — clears prev and never
//     visits the new row.
//   - the undo's restore (restoreConfig): it writes back a snapshot read
//     before the provision's own write, at the version that write reached.
//     A sweep that reached the row while the provision's write did not name
//     the record — that write never carried the pin, or its own re-check
//     already cleared it — removed nothing and bumped nothing, and the
//     restore lands the dead pin on a row nothing revisits.
//
// The same-identity write needs no re-check: its Update is a CAS on prev
// itself, and a row the sweep's listing saw is the sweep's to clear.
//
// The judgment is secrets.RecheckPinnedRecords, the one the routes'
// reconciliation shares, under the tenant scope the pins validated against.
// A record store that cannot answer is an error the provision reports; the
// retry re-provisions and re-checks, the same mend as a route edit's retry.
// A nil ApiKeys leaves key pins unjudged — the tolerance the routes'
// validators and re-check apply to a store that is not wired; the secret
// pins are re-checked through Secrets, which the provision cannot run
// without (it mints the managed token there).
//
// On every return, error included, cfg.Version is the version the row
// reached under this attempt's own writes — the one the undo restores at
// (restoreConfig) — never one another writer reached. So the clear is ONE
// version-CAS Update of the stored copy minus the dead pins: the store
// either takes it, the single bump it certifies as this attempt's, or
// answers ErrConflict, the fact that another writer moved the row since.
// The value-scoped clear cannot say which bumps were its own: how many it
// makes depends on the store and on the entries it removes, and a writer
// re-pointing a dead entry first leaves the count unchanged. On a conflict
// the row is no longer this attempt's: the dead pins go through that
// value-scoped clear — the sweep's own primitive, safe beside any writer —
// and cfg keeps the version it wrote, which the store has left behind, so
// the undo leaves the row to the writer that moved it. A failed Update
// leaves cfg as written too: the undo re-reads the row and restores only if
// it still stands there.
func (o *Orchestrator) reconcileStoredPins(ctx context.Context, cfg *webhooks.Config) error {
	var dead []string
	for _, rec := range secrets.RecheckPinnedRecords(ctx, o.ApiKeys, o.Secrets, cfg.TenantID, cfg.KeyOverrides, cfg.SecretOverrides) {
		switch {
		case rec.Err != nil:
			return fmt.Errorf("forge: re-check %s %s pinned on webhook %s: %w", rec.Kind, rec.ID, cfg.ID, rec.Err)
		case rec.Revoked:
			dead = append(dead, rec.ID)
		}
	}
	if len(dead) == 0 {
		return nil
	}
	cleared := *cfg
	cleared.KeyOverrides, cleared.SecretOverrides = maps.Clone(cfg.KeyOverrides), maps.Clone(cfg.SecretOverrides)
	for _, id := range dead {
		storekit.DropStringMapValues(&cleared.KeyOverrides, id)
		storekit.DropStringMapValues(&cleared.SecretOverrides, id)
	}
	switch err := o.Webhooks.Update(ctx, cleared); {
	case err == nil:
		cleared.Version++
		*cfg = cleared
	case errors.Is(err, webhooks.ErrConflict):
		for _, id := range dead {
			if _, err := o.Webhooks.ClearCredentialPins(ctx, cfg.ID, id); err != nil {
				return fmt.Errorf("forge: clear the pins naming deleted record %s from webhook %s, moved by another writer: %w", id, cfg.ID, err)
			}
		}
	default:
		return fmt.Errorf("forge: clear the pins naming deleted records %v from webhook %s: %w", dead, cfg.ID, err)
	}
	if o.LogWarn != nil {
		o.LogWarn("forge: webhook %s was written with pins naming records deleted mid-provision; cleared: %v", cfg.ID, dead)
	}
	return nil
}
