package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
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
//
// The listing below only PICKS the rows to visit. The removal itself is the
// store's value-scoped clear, judged against the row as the store holds it
// at write time — the load-bearing invariant is that after the delete no
// writer that VALIDATES can create a pin naming the dead record (the
// validators Get the record and refuse a dead id). The one resurrection
// left is a write that validated while the record lived and lands after
// the clear — the race the delete's 404-heal pass exists to re-sweep — and
// nothing the sweep saw is ever lost to it: a concurrent writer only ever
// removes.

// pinSweepTimeout bounds a detached sweep. The delete answers on the
// request's context; the sweep no longer does — a client that hangs up
// mid-sweep must not cancel the clear half-way, because the dead pin it
// leaves standing seals the team's default with nobody watching. Revocation
// already happened (the irreversible half), so it is the sweep finishing
// that makes the 204 true.
const pinSweepTimeout = time.Minute

// sweepCredentialPins clears every pin of tenantID that names recordID and
// reports the surfaces it swept ("webhook <id>", "trigger <id>") for the
// delete's audit line. A store error fails the whole sweep — the caller
// answers with an explicit half-done receipt — while a record that vanished
// between the list and the clear is nothing to sweep, not a failure.
func (s *Server) sweepCredentialPins(ctx context.Context, tenantID, recordID string) ([]string, error) {
	var swept []string
	var errs []error
	if cfgs := s.webhookConfigs; cfgs != nil {
		rows, err := cfgs.ListByTenant(ctx, tenantID)
		if err != nil {
			errs = append(errs, fmt.Errorf("webhook configs of %s: %w", tenantID, err))
		}
		for _, c := range rows {
			if !pinsName(c.KeyOverrides, recordID) && !pinsName(c.SecretOverrides, recordID) {
				continue
			}
			cleared, err := cfgs.ClearCredentialPins(ctx, c.ID, recordID)
			if err != nil && !errors.Is(err, webhooks.ErrNotFound) {
				errs = append(errs, fmt.Errorf("webhook %s: %w", c.ID, err))
				continue
			}
			if cleared {
				swept = append(swept, "webhook "+c.ID)
			}
		}
	}
	if subs := s.cfg.TriggerStore; subs != nil {
		rows, err := subs.ListByTenant(ctx, tenantID)
		if err != nil {
			errs = append(errs, fmt.Errorf("trigger subscriptions of %s: %w", tenantID, err))
		}
		for _, sub := range rows {
			if !pinsName(sub.KeyOverrides, recordID) && !pinsName(sub.SecretOverrides, recordID) {
				continue
			}
			cleared, err := subs.ClearCredentialPins(ctx, sub.ID, recordID)
			if err != nil && !errors.Is(err, trigger.ErrSubscriptionNotFound) {
				errs = append(errs, fmt.Errorf("trigger %s: %w", sub.ID, err))
				continue
			}
			if cleared {
				swept = append(swept, "trigger "+sub.ID)
			}
		}
	}
	return swept, errors.Join(errs...)
}

// pinsName reports whether any pin entry's value names the record. The map
// key — a provider for key pins, a workflow secret name for secret pins —
// is display, not identity: a pin binds the id it carries.
func pinsName(pins map[string]string, recordID string) bool {
	for _, id := range pins {
		if id == recordID {
			return true
		}
	}
	return false
}

// retryCredentialPinSweep re-runs the delete sweep for a record the store no
// longer knows. The delete's halves are ordered — revocation first, the pin
// sweep second — so a sweep that failed (a store error, or a client hanging
// up mid-sweep before the detach) leaves behind a retry that reaches the
// route as "not found": without this pass the retry would answer 404 before
// any sweep ran, and the dead pins would stand until a manual hunt through
// every webhook and subscription. The clear matches pins by value, so the
// pass is idempotent and sweeps nothing for a record that never had pins.
//
// The tenant comes from the request scope — the same derivation the route's
// store reads use — and the record id from the path, the two facts a delete
// retry still carries. The pass runs detached (pinSweepTimeout): the caller
// may be gone, the sweep must not. Scoped routes with no tenant in the path
// (the /api/me deletes) sweep nothing: their user-scoped records are never
// pinnable, and a tenant-less sweep would be unscoped.
func (s *Server) retryCredentialPinSweep(r *http.Request, tenantID, recordID string) ([]string, error) {
	id, _ := auth.FromContext(r.Context())
	if tenantID == "" || !s.mayClearTenantPins(r.Context(), id, tenantID) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pinSweepTimeout)
	defer cancel()
	return s.sweepCredentialPins(ctx, tenantID, recordID)
}

// mayClearTenantPins reports whether the identity may sweep a tenant's
// credential pins — the right the delete routes already demand for the
// record whose pins they sweep. The rows a sweep clears belong to the
// tenant (its webhook configs and subscriptions, whose edit path is
// canManageTeam); an org-tier tenant's rows answer to the org's admins;
// super-admins answer to every tenant, as on the record-scoped gate. The
// record is gone — the tenant form is the only gate left, and it asks for
// the same right creating a record there would.
func (s *Server) mayClearTenantPins(ctx context.Context, id auth.Identity, tenantID string) bool {
	if id.IsSuperAdmin {
		return true
	}
	if orgID, ok := secrets.OrgIDFromTierScope(tenantID); ok {
		return s.canManageOrg(ctx, id, orgID)
	}
	return s.canManageTeam(ctx, id, tenantID)
}
