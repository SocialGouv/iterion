package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
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
// validators Get the record and refuse a dead id), and the clear's version
// bump turns a full-document editor holding a pre-sweep snapshot away with
// a conflict instead of letting it write the swept pin back. A row BORN
// mid-delete — a write whose validation beat the revocation and lands
// after this sweep's listing, which therefore never sees it — is the
// write routes' half to close: reconcileLandedPins re-checks every record
// a just-landed write's pins name and sweeps a dead one immediately, so
// the window is covered whatever the interleaving. This file's retry pass
// (retryCredentialPinSweep) remains the reconciliation of an INTERRUPTED
// sweep — a failed or hung-up first pass the delete's retry finishes —
// not the born-mid-delete case.

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
// The tenant is the route's read scope (pinSweepTenant) and the record id
// the path's, the two facts a delete retry still carries. The pass runs
// detached (pinSweepTimeout): the caller may be gone, the sweep must not.
// A read scope with no tenant sweeps nothing — a tenant-less sweep would
// be unscoped — and no cloud route reaches this pass without one: the
// Mongo stores refuse a tenant-less read before any "not found". A caller
// the gate refuses sweeps nothing either and gets the route's bare 404 —
// the same answer a caller with no standing gets for any record. A gate
// the identity store cannot ANSWER (an outage, not a refusal) is an error
// the delete route answers 5xx: a 404 there would be the silent skip the
// heal exists to close, with the pins still standing and nothing logged.
func (s *Server) retryCredentialPinSweep(r *http.Request, tenantID, recordID string) ([]string, error) {
	if tenantID == "" {
		return nil, nil
	}
	id, _ := auth.FromContext(r.Context())
	allowed, err := s.mayClearTenantPinsOrFail(r.Context(), id, tenantID)
	if err != nil {
		return nil, fmt.Errorf("judge the right to sweep the pins of %s: %w", tenantID, err)
	}
	if !allowed {
		return nil, nil
	}
	ctx, cancel := pinGuardCtx(r)
	defer cancel()
	return s.sweepCredentialPins(ctx, tenantID, recordID)
}

// pinGuardCtx detaches a pin guard from the request's lifetime, bounded by
// pinSweepTimeout: the caller may be gone, the guard must not — a client
// that hangs up mid-clear must not cancel it half-way, because the dead
// pin that leaves standing seals the team's default with nobody watching.
func pinGuardCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), pinSweepTimeout)
}

// pinSweepTenant names the tenant a delete sweeps pins under: the tenant
// the route's store reads ran under — the team in the path, the org
// credential tier's reserved scope, the platform scope the admin routes
// stamp, or the caller's active team on the /api/me routes (the auth
// middleware's stamp, under which a team-wide key or secret is as
// deletable as under its team path). That read scope is the record's own
// tenant — the stores filter the read on it — and the pins' — the
// write-time validators admit a same-tenant record only — so the first
// pass and the retry derive the one tenant from the one source, with or
// without the row, and sweep the same rows. A read scope with no tenant
// (a tenant-less fixture) names nothing to sweep under.
func pinSweepTenant(ctx context.Context) string {
	tenantID, _ := store.TenantFromContext(ctx)
	return tenantID
}

// landedPins is what a pin-landing write learns from reconcileLandedPins
// once its row is stored. Cleared lists the surfaces the re-check swept
// ("webhook <id>", "trigger <id>"), the receipt the audit line carries;
// Swept the record ids found revoked whose sweep completed, the pins the
// answer strips so it does not resell a pin the store no longer holds;
// Unreconciled the record ids the re-check could not vouch for — a record
// the store failed to read, or a dead one whose sweep failed — behind Err,
// the joined causes for the server log.
type landedPins struct {
	Cleared      []string
	Swept        []string
	Unreconciled []string
	Err          error
}

// strip drops from pins every entry naming a swept record — in place, the
// row is the route's own copy — so the answer shows the row as the store
// holds it. The two pin-bearing rows hide their version from JSON, and the
// clear touches nothing else, so the stripped copy is byte-for-byte the
// stored row's answer without a read-back that could fail after the write.
func (p landedPins) strip(pins map[string]string) {
	for _, dead := range p.Swept {
		storekit.DropStringMapValues(&pins, dead)
	}
}

// reconcileLandedPins closes the write-side twin of the sweep's window. A
// pin is judged against its record at VALIDATION time, so a write whose
// validation beat the record's revocation can land after the delete's
// sweep already listed — the sweep never sees the row, no client can
// re-delete a gone record, and a pin naming the dead record would stand
// forever, silently sealing the team's default on a cron subscription.
// After the store write lands, the route re-checks every record its pins
// name under the same detached context as the delete's sweep: one already
// revoked sweeps immediately. The pair closes the window whatever the
// interleaving — a write that lands before the sweep's listing is the
// sweep's to clear; one that lands after names a record revoked before the
// listing began, which is exactly what the re-check reads. The re-check is
// secrets.RecheckPinnedRecords — the judgment the forge migration carry
// shares — under the tenant scope the write-time validators used, so a
// record they admitted cannot read as missing.
//
// What a route answers when Err is set depends on what a retry can mend.
// An EDIT answers 5xx: the row holds the edit, the client's retry re-reads
// and re-validates the very pins, and a dead record is refused by name. A
// CREATE answers its row with Unreconciled beside it (`pins_unreconciled`)
// and the cause in the log under the row id (logUnreconciledPins): the row
// is already stored under a fresh id, so a retry of a 5xx mints a second
// webhook or a second cron subscription, the 5xx body is opaque by
// convention so the id could not even travel in it, and a webhook's
// one-time token exists only in the 201 — a 5xx would reconcile nothing and
// lose the only copy of the secret.
func (s *Server) reconcileLandedPins(ctx context.Context, tenantID string, keyPins, secretPins map[string]string) landedPins {
	var out landedPins
	var errs []error
	for _, rec := range secrets.RecheckPinnedRecords(ctx, s.apiKeys, s.genericSecrets, tenantID, keyPins, secretPins) {
		switch {
		case rec.Revoked:
			cleared, err := s.sweepCredentialPins(ctx, tenantID, rec.ID)
			out.Cleared = append(out.Cleared, cleared...)
			if err != nil {
				out.Unreconciled = append(out.Unreconciled, rec.ID)
				errs = append(errs, fmt.Errorf("%s %s is revoked, sweeping its pins: %w", rec.Kind, rec.ID, err))
				continue
			}
			out.Swept = append(out.Swept, rec.ID)
		case rec.Err != nil:
			out.Unreconciled = append(out.Unreconciled, rec.ID)
			errs = append(errs, fmt.Errorf("%s %s: %w", rec.Kind, rec.ID, rec.Err))
		}
	}
	out.Err = errors.Join(errs...)
	return out
}

// logUnreconciledPins is the create routes' half of a failed re-check: the
// cause, under the row id, where the operator looks — the 201 beside it
// carries the record ids only, never the store's error text.
func (s *Server) logUnreconciledPins(surface, rowID string, pins landedPins) {
	if s.logger == nil {
		return
	}
	s.logger.Error("credential pins: %s %s is stored, but re-checking the records its pins name failed (unreconciled: %v): %v — a pin may name a record deleted in flight and stand until it is cleared", surface, rowID, pins.Unreconciled, pins.Err)
}

// mayClearTenantPinsOrFail reports whether the identity may sweep a tenant's
// credential pins — the right the delete routes already demand for the
// record whose pins they sweep. The rows a sweep clears belong to the
// tenant (its webhook configs and subscriptions, whose pins only
// canManageTeam writes); an org-tier tenant's rows answer to the org's admins;
// super-admins answer to every tenant, as on the record-scoped gate. The
// record is gone — the tenant form is the only gate left, and it asks for
// the same right creating a record there would. Only a refusal carries an
// error, and only when a read the store could not complete is what stopped
// it (the OrFail forms): the caller tells an outage from a "no" instead of
// skipping the heal over a blip.
func (s *Server) mayClearTenantPinsOrFail(ctx context.Context, id auth.Identity, tenantID string) (bool, error) {
	if id.IsSuperAdmin {
		return true, nil
	}
	if orgID, ok := secrets.OrgIDFromTierScope(tenantID); ok {
		return s.canManageOrgOrFail(ctx, id, orgID)
	}
	return s.canManageTeamOrFail(ctx, id, tenantID)
}
