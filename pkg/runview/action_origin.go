package runview

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ActionOrigin is the origin of the run a checked action addresses: what a
// surface that compared the caller's expected birth admitted (the HTTP
// run-origin gate). The zero value is unchecked: every caller that compared
// nothing — the CLI, MCP, bots, the retry sweeper, an older studio — passes
// it, and the service acts exactly as without it.
//
// A service that continues an execution or reads other runs carries it
// instead of a bound context: it verifies each run it loads by ID and acts
// on against the origin (Verify), and binds only the named store-only writes
// of that run to it (Bind). The execution it starts never inherits the
// binding.
type ActionOrigin struct {
	origin  store.RunObservation
	checked bool
}

// ExpectRunOrigin returns the checked ActionOrigin of an admitted
// observation (store.RunObservation, as CaptureRunOrigin returns it).
func ExpectRunOrigin(o store.RunObservation) ActionOrigin {
	return ActionOrigin{origin: o, checked: true}
}

// Checked reports whether the action was admitted on an expected birth.
func (a ActionOrigin) Checked() bool { return a.checked }

// Verify compares a run the service loaded by ID with the admitted origin:
// its header (birth, format, protocol) and its stored tenant, as a bound read
// compares them. A successor recreated under the ID answers
// store.ErrRunDataIdentityChanged. An unchecked origin admits every run.
func (a ActionOrigin) Verify(r *store.Run) error {
	if !a.checked {
		return nil
	}
	if r == nil {
		return fmt.Errorf("runview: the run this action was admitted on was not loaded: %w", store.ErrRunNotFound)
	}
	got, err := runObservation(r)
	if err != nil {
		return err
	}
	if got.Header() != a.origin.Header() || got.TenantID() != a.origin.TenantID() {
		return fmt.Errorf("runview: run %s holds birth %q, not the birth %q this action was admitted on: %w",
			r.ID, r.BirthID, a.origin.Header().BirthID, store.ErrRunDataIdentityChanged)
	}
	return nil
}

// originRefused reports whether err is a bound access refusing the run behind
// the ID as no longer the admitted origin: a successor's header, a deletion,
// an absence.
func originRefused(err error) bool {
	return errors.Is(err, store.ErrRunDataIdentityChanged) || errors.Is(err, store.ErrRunDeleted) || errors.Is(err, store.ErrRunNotFound)
}

// verifyOrigin loads runID and verifies it against a checked origin. An
// unchecked origin reads nothing.
func (s *Service) verifyOrigin(ctx context.Context, runID string, origin ActionOrigin) error {
	if !origin.Checked() {
		return nil
	}
	_, r, err := store.LoadRunWithValueFormat(ctx, s.store, runID)
	if err != nil {
		return err
	}
	return origin.Verify(r)
}

// Bind returns ctx bound to the admitted origin (store.BindRunObservation),
// for a store-only write of the addressed run through st: the write then
// refuses a successor recreated under the ID. st must endorse a bound
// observation (store.RequireBoundObservationStore): a store that may drop the
// context could write whatever run holds the ID now. An unchecked origin
// returns ctx unchanged.
func (a ActionOrigin) Bind(ctx context.Context, st store.RunStore) (context.Context, error) {
	if !a.checked {
		return ctx, nil
	}
	if err := store.RequireBoundObservationStore(st); err != nil {
		return nil, err
	}
	return store.BindRunObservation(ctx, a.origin)
}
