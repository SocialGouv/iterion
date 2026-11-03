package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// pinSweepingSecrets is the generic secret store the server hands the forge
// orchestrator: the delete chokepoint of handleDeleteGenericSecret in store
// form. The orchestrator deletes the managed secrets it minted straight
// through the store — a project bot's token on deprovision, a connection's
// managed token on disconnect, the compensation of a failed mint — and
// never through the route. Those secrets are team-scoped with no user
// scope, so validateSecretOverrides admits any of them as a pin on another
// webhook or subscription of the tenant (a bot posting under a different
// forge identity, the use carryOperatorWebhookSettings keeps), and a
// deprovision would leave that pin dead and the workflow secret unfilled in
// silence — the class the route sweeps close. The one sweep the routes run
// (sweepCredentialPins) runs here too, in the routes' order: revocation
// first, the sweep second, under the tenant the caller's read scope carries
// (pinSweepTenant — the forge routes stamp it, and the Mongo store refuses
// a tenant-less delete before anything could be swept).
//
// A record the store no longer knows still sweeps: that is the shape of a
// delete whose sweep half failed, and the orchestrator's retry (Deprovision
// reconciles until RetireCleanupComplete, DeprovisionConnection until the
// connection is gone) re-enters here with ErrGenericSecretNotFound from the
// inner delete — the retry heal the routes run before their 404.
type pinSweepingSecrets struct {
	secrets.GenericSecretStore
	srv *Server
}

// pinSweepingSecrets wraps the server's generic secret store for the
// orchestrator; nil when no store is wired, so the orchestrator's own
// dependency check keeps refusing to build.
func (s *Server) pinSweepingSecrets() secrets.GenericSecretStore {
	if s.genericSecrets == nil {
		return nil
	}
	return pinSweepingSecrets{GenericSecretStore: s.genericSecrets, srv: s}
}

// Delete revokes, then sweeps the pins naming the record. A sweep the
// stores refuse is an error even though the record is gone: a clean return
// would be the dead pin standing behind a clean deprovision, and the
// caller's retry is what finishes the sweep. The inner delete's verdict is
// what the caller gets back once the sweep succeeded — nil, or the
// ErrGenericSecretNotFound every orchestrator deleter tolerates.
func (d pinSweepingSecrets) Delete(ctx context.Context, id string) error {
	err := d.GenericSecretStore.Delete(ctx, id)
	if err != nil && !errors.Is(err, secrets.ErrGenericSecretNotFound) {
		return err
	}
	// Detached from the caller's lifetime (pinSweepTimeout): a cancelled
	// deprovision must not cancel the clear half-way, exactly as a client
	// hanging up on the route must not.
	sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pinSweepTimeout)
	defer cancel()
	swept, sweepErr := d.srv.sweepCredentialPins(sweepCtx, pinSweepTenant(ctx), id)
	if sweepErr != nil {
		return fmt.Errorf("secret %s is deleted, but clearing the credential pins that name it failed: %w — a surviving pin keeps its workflow secret unfilled until the delete is retried", id, sweepErr)
	}
	if len(swept) > 0 && d.srv.logger != nil {
		d.srv.logger.Info("credential pins: deleting secret %s swept the pins naming it: %v", id, swept)
	}
	return err
}
