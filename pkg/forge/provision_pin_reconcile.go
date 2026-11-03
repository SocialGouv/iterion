package forge

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// reconcileCarriedPins closes the migration carry's half of the credential
// pin window (#2644). carryOperatorWebhookSettings copies prev's key and
// secret pins by value into the row of a NEW hook identity, and the write
// that lands them commits against the candidate row's version, not prev's.
// A record deleted while the provision ran — its delete's sweep listing the
// tenant's rows before the new row existed — clears prev and never visits
// the new row: the dead pin lands on a row nothing revisits, and the lane
// seals the team default in silence. The same-identity path needs no
// re-check: its Update is a CAS on prev itself, and a row the sweep's
// listing saw is the sweep's to clear.
//
// So the migration write does what every pin-landing route does once its
// row is stored: it re-reads every record the carried pins name, under the
// tenant scope the pins validated against (secrets.RecheckPinnedRecords,
// the judgment the routes' reconciliation shares), and clears a revoked one
// from THIS row with the store's value-scoped clear — the sweep's own
// primitive, version bump included — then refreshes the written copy so the
// undo's version check and the rest of the provision see the row as the
// store holds it. A record store that cannot answer is an error the
// provision reports; the retry re-provisions and re-checks, the same mend
// as a route edit's retry. A nil ApiKeys leaves key pins unjudged — the
// tolerance the routes' validators and re-check apply to a store that is
// not wired; the secret pins are re-checked through Secrets, which the
// provision cannot run without (it mints the managed token there).
func (o *Orchestrator) reconcileCarriedPins(ctx context.Context, cfg *webhooks.Config) error {
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
	for _, id := range dead {
		if _, err := o.Webhooks.ClearCredentialPins(ctx, cfg.ID, id); err != nil {
			return fmt.Errorf("forge: clear the pins naming deleted record %s from webhook %s: %w", id, cfg.ID, err)
		}
	}
	fresh, err := o.Webhooks.Get(ctx, cfg.ID)
	if err != nil {
		return fmt.Errorf("forge: re-read webhook %s after clearing its dead pins: %w", cfg.ID, err)
	}
	cfg.KeyOverrides, cfg.SecretOverrides, cfg.Version = fresh.KeyOverrides, fresh.SecretOverrides, fresh.Version
	if o.LogWarn != nil {
		o.LogWarn("forge: webhook %s carried pins naming records deleted mid-provision; cleared: %v", cfg.ID, dead)
	}
	return nil
}
