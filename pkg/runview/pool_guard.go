package runview

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
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
