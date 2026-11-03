package secrets

import (
	"context"
	"errors"
	"slices"

	"github.com/SocialGouv/iterion/pkg/store"
)

// PinRecheck is what re-reading one record a credential pin names found. A
// pin binds the id it carries — the map key, a provider or a workflow
// secret name, is display — so the verdict is the record's, for every pin
// entry naming it.
type PinRecheck struct {
	// ID is the record id the pins carry.
	ID string
	// Kind names the record's store in a message: "api key" or "stored
	// secret".
	Kind string
	// Revoked is the store's definitive not-found under the tenant: the
	// record is gone, and every pin naming it is dead.
	Revoked bool
	// Err is a read the store could not complete: the record is neither
	// vouched for nor known revoked. Nil whenever the store answered.
	Err error
}

// RecheckPinnedRecords re-reads every record the key and secret pins name,
// under tenantID's scope — the tenant the write-time validators judged the
// pins against, so another tenant's record cannot read as revoked — and
// reports what each read found: each record once however many entries name
// it, key pins first and each family in id order, so a receipt built from
// it is stable. A nil store leaves its family unjudged, the tolerance the
// validators apply to a store that is not wired.
//
// It is the one judgment every writer that lands a pin the delete's sweep
// may never visit makes once its row is stored — the routes' landed-pin
// reconciliation (pkg/server reconcileLandedPins) and the forge migration
// carry (reconcileCarriedPins) — and each acts on the verdicts its own way:
// a route sweeps a revoked record's pins tenant-wide, the carry clears them
// from the one row it wrote.
func RecheckPinnedRecords(ctx context.Context, keys ApiKeyStore, gen GenericSecretStore, tenantID string, keyPins, secretPins map[string]string) []PinRecheck {
	tctx := store.WithTenant(ctx, tenantID)
	var out []PinRecheck
	if keys != nil {
		for _, id := range pinnedRecordIDs(keyPins) {
			_, err := keys.Get(tctx, id)
			out = append(out, judgePinnedRecord(id, "api key", err, ErrApiKeyNotFound))
		}
	}
	if gen != nil {
		for _, id := range pinnedRecordIDs(secretPins) {
			_, err := gen.Get(tctx, id)
			out = append(out, judgePinnedRecord(id, "stored secret", err, ErrGenericSecretNotFound))
		}
	}
	return out
}

// judgePinnedRecord reads one store answer: the store's own not-found
// sentinel is the record's revocation, any other failure a read it could
// not complete.
func judgePinnedRecord(id, kind string, err, notFound error) PinRecheck {
	r := PinRecheck{ID: id, Kind: kind}
	switch {
	case errors.Is(err, notFound):
		r.Revoked = true
	case err != nil:
		r.Err = err
	}
	return r
}

// pinnedRecordIDs lists the record ids a pin map names, each once and in
// order: two providers pinning one key cost one read.
func pinnedRecordIDs(pins map[string]string) []string {
	ids := make([]string, 0, len(pins))
	for _, id := range pins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
