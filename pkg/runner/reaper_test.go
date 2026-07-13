package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeLeaseChecker is an in-memory runLeaseChecker.
type fakeLeaseChecker struct {
	locked map[string]bool
	err    error
}

func (f fakeLeaseChecker) IsRunLocked(_ context.Context, runID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.locked[runID], nil
}

// fakeRunLoader is an in-memory runLoader. When loadErr is set it is
// returned for every lookup (used to exercise the transient-error path);
// otherwise a missing run yields the store.ErrRunNotFound sentinel so the
// reap predicate can distinguish genuine absence from a store blip.
type fakeRunLoader struct {
	runs    map[string]*store.Run
	loadErr error
}

func (f fakeRunLoader) LoadRun(_ context.Context, runID string) (*store.Run, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	r, ok := f.runs[runID]
	if !ok {
		return nil, fmt.Errorf("store: run %s: %w", runID, store.ErrRunNotFound)
	}
	return r, nil
}

func TestSandboxResourceReapable(t *testing.T) {
	ctx := context.Background()
	const rid = "run-1"

	run := func(s store.RunStatus) map[string]*store.Run {
		return map[string]*store.Run{rid: {ID: rid, Status: s}}
	}

	tests := []struct {
		name   string
		runID  string
		leases fakeLeaseChecker
		loader fakeRunLoader
		want   bool
	}{
		{
			name:  "empty run id is an orphan",
			runID: "",
			want:  true,
		},
		{
			name:   "lease held → live → keep",
			runID:  rid,
			leases: fakeLeaseChecker{locked: map[string]bool{rid: true}},
			loader: fakeRunLoader{runs: run(store.RunStatusFinished)},
			want:   false,
		},
		{
			name:   "lease-check error → fail safe → keep",
			runID:  rid,
			leases: fakeLeaseChecker{err: errors.New("kv down")},
			loader: fakeRunLoader{runs: run(store.RunStatusFinished)},
			want:   false,
		},
		{
			name:   "no lease + provably absent from store → reap",
			runID:  rid,
			leases: fakeLeaseChecker{},
			loader: fakeRunLoader{runs: map[string]*store.Run{}}, // yields ErrRunNotFound
			want:   true,
		},
		{
			name:   "no lease + transient store error → fail safe → keep",
			runID:  rid,
			leases: fakeLeaseChecker{},
			loader: fakeRunLoader{loadErr: errors.New("mongo: connection reset")},
			want:   false,
		},
		{
			name:   "no lease + terminal (finished) → reap",
			runID:  rid,
			leases: fakeLeaseChecker{},
			loader: fakeRunLoader{runs: run(store.RunStatusFinished)},
			want:   true,
		},
		{
			name:   "no lease + failed_resumable (terminal) → reap",
			runID:  rid,
			leases: fakeLeaseChecker{},
			loader: fakeRunLoader{runs: run(store.RunStatusFailedResumable)},
			want:   true,
		},
		{
			name:   "no lease but store says running → backstop keeps it",
			runID:  rid,
			leases: fakeLeaseChecker{},
			loader: fakeRunLoader{runs: run(store.RunStatusRunning)},
			want:   false,
		},
		{
			name:   "no lease but store says paused → backstop keeps it",
			runID:  rid,
			leases: fakeLeaseChecker{},
			loader: fakeRunLoader{runs: run(store.RunStatusPausedWaitingHuman)},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sandboxResourceReapable(ctx, tt.leases, tt.loader, tt.runID)
			if got != tt.want {
				t.Fatalf("sandboxResourceReapable(%q) = %v, want %v", tt.runID, got, tt.want)
			}
		})
	}
}

func TestSandboxReapInterval_EnvOverride(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_REAP_INTERVAL", "5s")
	if got := sandboxReapInterval(); got.String() != "5s" {
		t.Fatalf("sandboxReapInterval() = %v, want 5s", got)
	}

	t.Setenv("ITERION_SANDBOX_REAP_INTERVAL", "not-a-duration")
	if got := sandboxReapInterval(); got != defaultSandboxReapInterval {
		t.Fatalf("sandboxReapInterval() with bad value = %v, want default %v", got, defaultSandboxReapInterval)
	}

	t.Setenv("ITERION_SANDBOX_REAP_INTERVAL", "0")
	if got := sandboxReapInterval(); got != 0 {
		t.Fatalf("sandboxReapInterval() = %v, want 0 (disables ticker)", got)
	}
}
