package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type preparedGrantRaceStore struct {
	*store.FilesystemRunStore
	cancel    context.CancelFunc
	successor store.RunExecutionAuthority
	cas       int
}

func (s *preparedGrantRaceStore) AcquireRunExecutionIf(ctx context.Context, id string, x store.RunExecutionExpectation, intent store.RunExecutionIntent) (store.RunExecutionAuthority, bool, error) {
	original, ok, err := s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, x, intent)
	if err != nil || !ok {
		return original, ok, err
	}
	fresh, err := s.LoadRun(ctx, id)
	if err != nil {
		return original, false, err
	}
	s.successor, ok, err = s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, store.RunExecutionExpectation{Version: fresh.CASVersion, Epoch: fresh.ExecutionEpoch, Status: fresh.Status}, store.ExecutionStart)
	if err != nil || !ok {
		return original, false, err
	}
	s.cancel()
	return original, true, nil
}
func (s *preparedGrantRaceStore) UpdateRunOutcomeIf(ctx context.Context, id string, x store.RunOutcomeExpectation, status store.RunStatus, msg string, m store.RunOutcomeMeta, from []store.RunStatus) (bool, error) {
	s.cas++
	return s.FilesystemRunStore.UpdateRunOutcomeIf(ctx, id, x, status, msg, m, from)
}
func TestPreparedEntryKeepsAcquiredGrant(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.PrepareRunInvocation(t.Context(), fs, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(p.Context)
	defer cancel()
	spy := &preparedGrantRaceStore{FilesystemRunStore: fs, cancel: cancel}
	calls := 0
	e := admissionSnapshotEngine(t, spy, nil, &calls)
	e.executor = preparedAccountingExecutor{e.executor}
	err = e.RunPrepared(ctx, "run", nil, p)
	r, readErr := fs.LoadRun(t.Context(), "run")
	if readErr != nil {
		t.Fatal(readErr)
	}
	t.Logf("err=%v calls=%d CAS=%d status=%s original-acquisition-lost=%v", err, calls, spy.cas, r.Status, spy.successor.Matches(r))
	if !errors.Is(err, ErrAdmissionStopped) || calls != 0 || spy.cas != 0 || r.Status != store.RunStatusRunning || !spy.successor.Matches(r) {
		t.Fatal("acquired grant adopted successor", err, calls, spy.cas, r.Status)
	}
}
