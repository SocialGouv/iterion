package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// A credential pin validated at write can outlive the record it names: the
// owner deletes the key or secret and the stored pins survive it. Nothing
// downstream reports that — the resolver drops an unresolvable pin id in
// silence and the walk seals the team's default, the exact replacement the
// write-time validators (validateKeyOverrides / validateSecretOverrides)
// exist to refuse — so a cron subscription would fire under the default
// credential forever. The delete chokepoints (handleDeleteApiKeyIn,
// handleDeleteGenericSecret) sweep the pins instead of refusing: revoking a
// leaked credential must never wait on a config edit, and a swept pin is
// the explicit clear, not a replacement — the launch falls back exactly as
// an operator clearing the pin would have made it.
//
// The pins live in the two machine-lane homes and no third: webhook configs
// and trigger subscriptions. Both write paths validate a pin against a
// same-tenant record, so one tenant's sweep cannot miss a pin in another.

// sweepCredentialPins clears every pin of tenantID that names recordID and
// reports the surfaces it swept ("webhook <id>", "trigger <id>") for the
// delete's audit line. A store error fails the whole sweep — the caller
// answers with an explicit half-done receipt — while a record that vanished
// between the list and the write is nothing to clear, not a failure.
func (s *Server) sweepCredentialPins(ctx context.Context, tenantID, recordID string) ([]string, error) {
	var swept []string
	var errs []error
	if cfgs := s.webhookConfigs; cfgs != nil {
		rows, err := cfgs.ListByTenant(ctx, tenantID)
		if err != nil {
			errs = append(errs, fmt.Errorf("webhook configs of %s: %w", tenantID, err))
		}
		for _, c := range rows {
			keyPins, keyDropped := withoutPin(c.KeyOverrides, recordID)
			secPins, secretDropped := withoutPin(c.SecretOverrides, recordID)
			if !keyDropped && !secretDropped {
				continue
			}
			if err := cfgs.SetCredentialPins(ctx, c.ID, keyPins, secPins); err != nil && !errors.Is(err, webhooks.ErrNotFound) {
				errs = append(errs, fmt.Errorf("webhook %s: %w", c.ID, err))
				continue
			}
			swept = append(swept, "webhook "+c.ID)
		}
	}
	if subs := s.cfg.TriggerStore; subs != nil {
		rows, err := subs.ListByTenant(ctx, tenantID)
		if err != nil {
			errs = append(errs, fmt.Errorf("trigger subscriptions of %s: %w", tenantID, err))
		}
		for _, sub := range rows {
			keyPins, keyDropped := withoutPin(sub.KeyOverrides, recordID)
			secPins, secretDropped := withoutPin(sub.SecretOverrides, recordID)
			if !keyDropped && !secretDropped {
				continue
			}
			if err := subs.SetCredentialPins(ctx, sub.ID, keyPins, secPins); err != nil && !errors.Is(err, trigger.ErrSubscriptionNotFound) {
				errs = append(errs, fmt.Errorf("trigger %s: %w", sub.ID, err))
				continue
			}
			swept = append(swept, "trigger "+sub.ID)
		}
	}
	return swept, errors.Join(errs...)
}

// withoutPin returns the map minus every entry whose value names the
// deleted record, and whether it dropped any. The map key — a provider for
// key pins, a workflow secret name for secret pins — is display, not
// identity: a pin binds the id it carries.
func withoutPin(pins map[string]string, recordID string) (map[string]string, bool) {
	dropped := false
	out := make(map[string]string, len(pins))
	for name, id := range pins {
		if id == recordID {
			dropped = true
			continue
		}
		out[name] = id
	}
	return out, dropped
}
