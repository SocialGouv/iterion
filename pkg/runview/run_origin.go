package runview

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/store"
)

// runDataHeader is the identity a loaded run's readers bind to.
func runDataHeader(run *store.Run) store.RunDataHeader {
	return store.RunDataHeader{RunID: run.ID, Format: store.ValueFormat(run.FormatVersion), Protocol: run.DataProtocol, BirthID: run.BirthID}
}

// runObservation is the observation a loaded run's readers bind to: its
// header and the tenant its store decoded. The decoded tenant differs from
// the canonical one only on a record whose tenant_id is duplicated, spelled
// as a case alias or not a string; the canonical parser of every native bound
// read refuses such a record (ErrInvalidRunDataIdentity), so it is never read
// under this observation.
func runObservation(run *store.Run) (store.RunObservation, error) {
	return store.CaptureRunObservation(runDataHeader(run), run.TenantID, true)
}

// CaptureRunOrigin loads runID once from the primary store and returns the
// observation a long-lived reader binds to with store.BindRunObservation.
// See CaptureRunOriginIn.
func (s *Service) CaptureRunOrigin(ctx context.Context, runID string) (store.RunObservation, error) {
	return CaptureRunOriginIn(ctx, s.store, runID)
}

// CaptureRunOriginIn loads runID once from st and returns its observation: the
// loaded header and the run's STORED tenant (runObservation). The requester's
// identity on ctx selects what the store lets it see; it is never recorded as
// the run's tenant. The observation is provenance, not authorization, and no
// later read repairs it.
func CaptureRunOriginIn(ctx context.Context, st store.RunStore, runID string) (store.RunObservation, error) {
	_, run, err := store.LoadRunWithValueFormat(ctx, st, runID)
	if err != nil {
		return store.RunObservation{}, err
	}
	return runObservation(run)
}
