package runview

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrPoolContentRefused is the typed refusal (sovereign pools, D12): a
// server-side auxiliary LLM surface — the merge-conflict resolver, declared
// supervisors, the session board — declines to send content derived from a
// run stamped to a sovereign runner pool through a vendor-default model.
// Detect with errors.Is. The remedy is the operator's: pin the surface to
// the pool's openai_compatible gateway spec, or provide that gateway
// credential to the process making the call.
var ErrPoolContentRefused = errors.New("sovereign pool: refusing to send this run's content through a server-side LLM")

// poolContentRefusal reports the refusal for an auxiliary server-side LLM
// call whose content derives from a run stamped to pool. A nil return means
// the call may proceed: either the run carries no pool stamp, or the model
// spec is itself gateway-routed (openai_compatible/…) — the pool team's own
// credential, provided to this process by the operator. The stamp consulted
// is the run document's FROZEN stamp: unmapping a team never declassifies
// its past runs' content.
func poolContentRefusal(pool, spec string) error {
	if pool == "" {
		return nil
	}
	if modelroute.Parse(spec).Gateway() {
		return nil
	}
	return fmt.Errorf("%w: the run is stamped to pool %q; pin the model to the pool's openai_compatible gateway, or run the surface on the pool", ErrPoolContentRefused, pool)
}

// frozenPoolStamp reads the run document's pool stamp. The document is the
// stamp's authority: written at launch, never re-derived from the team's
// current mapping.
func (s *Service) frozenPoolStamp(ctx context.Context, runID string) (string, error) {
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return "", err
	}
	return r.RunnerPool, nil
}

// preStampPoolRefusal covers the cohort the frozen stamp cannot see: runs
// launched before the stamp existed carry RunnerPool=="", and the current
// mapping is the only signal left (a team mapped today was sovereign in
// intent for its older conflicted content). Nil lookup (local mode, no
// identity seam) and a nil stamp both pass. An UNREADABLE mapping is an
// infrastructure error surfaced as such (HTTP 500 — the remedy is not a
// model pin); only a DEFINITIVE mapped answer yields the policy refusal.
func (s *Service) preStampPoolRefusal(ctx context.Context, r *store.Run) error {
	if r.RunnerPool != "" || s.currentPoolForTenant == nil {
		return nil
	}
	cur, err := s.currentPoolForTenant(ctx, r.TenantID)
	if err != nil {
		return fmt.Errorf("pool guard: the current pool mapping for tenant %q is unreadable (%v)", r.TenantID, err)
	}
	if cur == "" {
		return nil
	}
	return fmt.Errorf("%w: the run predates the pool stamp and its tenant is now mapped to pool %q — resolve manually or from the pool", ErrPoolContentRefused, cur)
}

// WithCurrentPoolForTenant wires the fresh tenant→pool mapping lookup the
// pre-stamp cohort check reads. The server passes its identity store's
// GetTeam (ErrNotFound reads as definitively unmapped, mirroring the
// publisher's resolver).
func WithCurrentPoolForTenant(f func(ctx context.Context, tenantID string) (string, error)) ServiceOption {
	return func(s *Service) { s.currentPoolForTenant = f }
}
