package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A lost grant is a stopped attempt, never permission to adopt another owner's
// epoch. The caller must unwind its resources without running another round.
var errCorrectionOwnershipLost = fmt.Errorf("%w: output correction execution ownership lost", ErrAdmissionStopped)

// correctionExecution is invocation authority, obtained from the successful
// atomic claim response. It is immutable, and shared by a run's branches.
// This carrier is deliberately not installed by Engine yet: the owned claim
// and checkpoint callsites must be composed before activation.
type correctionExecution struct {
	runs      store.RunStore
	ledger    store.OutputCorrectionCASStore
	authority store.RunExecutionAuthority
}

func acquireCorrectionExecution(ctx context.Context, runs store.RunStore, observed *store.Run, intent store.RunExecutionIntent, publishedAt time.Time) (*correctionExecution, error) {
	ledger := store.AsObservedExecutionStore(runs)
	if ledger == nil {
		return nil, nil
	}
	if observed == nil {
		return nil, errors.New("output correction: claim without run snapshot")
	}
	expected := store.RunExecutionExpectation{Version: observed.CASVersion, Epoch: observed.ExecutionEpoch, Status: observed.Status, QueuedAt: observed.QueuedAt, PublishedAt: publishedAt}
	authority, acquired, err := ledger.AcquireRunExecutionIf(ctx, observed.ID, expected, intent)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errCorrectionOwnershipLost
	}
	if authority.RunID != observed.ID || authority.Epoch == "" {
		return nil, errors.New("output correction: invalid acquired authority")
	}
	return &correctionExecution{runs: runs, ledger: ledger, authority: authority}, nil
}

func (x *correctionExecution) context(ctx context.Context) context.Context {
	if x == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, correctionExecutionKey{}, x)
	return model.WithOutputCorrectionOwnership(ctx, model.OutputCorrectionOwnership{Protocol: model.OutputCorrectionProtocol, Authority: x.authority})
}

func (x *correctionExecution) load(ctx context.Context, key string) (store.OutputCorrectionEpisode, bool, error) {
	r, err := x.runs.LoadRun(ctx, x.authority.RunID)
	if err != nil {
		return store.OutputCorrectionEpisode{}, false, err
	}
	if !x.authority.Matches(r) {
		return store.OutputCorrectionEpisode{}, false, errCorrectionOwnershipLost
	}
	ep, exists := r.OutputCorrections[key]
	return ep, exists, nil
}

func (x *correctionExecution) decision(ctx context.Context, key string, old store.OutputCorrectionEpisode, exists bool, next store.OutputCorrectionEpisode) (store.OutputCorrectionEpisode, error) {
	expected := store.ExpectOutputCorrection(old)
	if !exists {
		expected = store.OutputCorrectionExpected{Absent: true}
	}
	next.Revision = old.Revision + 1
	next.UpdatedAt = time.Now().UTC()
	applied, err := x.ledger.SetRunOutputCorrectionIf(ctx, x.authority.RunID, x.authority, key, expected, next)
	if err != nil {
		return old, err
	}
	if !applied {
		return old, fmt.Errorf("%w: episode decision conflicted", errCorrectionOwnershipLost)
	}
	return next, nil
}
