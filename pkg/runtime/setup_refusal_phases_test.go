package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type setupRefusalStore struct {
	store.RunStore
	writes, events int
}

func (s *setupRefusalStore) UpdateRunStatusCoded(ctx context.Context, id string, status store.RunStatus, text string, code store.FailureCode) error {
	s.writes++
	return s.RunStore.UpdateRunStatusCoded(ctx, id, status, text, code)
}
func (s *setupRefusalStore) UpdateRunStatusIfCoded(ctx context.Context, id string, status store.RunStatus, text string, code store.FailureCode, from []store.RunStatus) (bool, error) {
	s.writes++
	return s.RunStore.UpdateRunStatusIfCoded(ctx, id, status, text, code, from)
}
func (s *setupRefusalStore) AppendEvent(ctx context.Context, id string, event store.Event) (*store.Event, error) {
	s.events++
	return s.RunStore.AppendEvent(ctx, id, event)
}

// All fourteen production phases use the same recorder and conversion. The
// distinct labels must not make cancellation turn a refusal into a verdict.
func TestEverySetupPhasePreservesStoreRefusalWithoutRetry(t *testing.T) {
	t.Parallel()
	phases := []string{"execution context admission", "var validation", "engine requirement", "worktree setup", "child resources", "sandbox start", "emit run_started", "bundle attachment defaults", "attachment promote", "execution context workspace", "save work dir", "bundle skills", "plugin contributions", "library skills"}
	for _, phase := range phases {
		for _, to := range []int{1, 2, 99} {
			for _, cancelled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/to=%d/cancelled=%t", phase, to, cancelled), func(t *testing.T) {
					t.Parallel()
					from := store.ValueFormat(1)
					if to == 1 {
						from = 2
					}
					st := tmpStore(t)
					r, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: from})
					if err != nil {
						t.Fatal(err)
					}
					ctx, err := store.BindRunValueFormat(t.Context(), r)
					if err != nil {
						t.Fatal(err)
					}
					if err = st.DeleteRun(ctx, "run"); err != nil {
						t.Fatal(err)
					}
					if _, err = st.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					birth := store.ValueFormat(to)
					if to == 99 {
						birth = 1
					}
					if _, err = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: birth}); err != nil {
						t.Fatal(err)
					}
					if to == 99 {
						path := filepath.Join(st.Root(), "runs", "run", "run.json")
						raw, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						var doc map[string]json.RawMessage
						if err = json.Unmarshal(raw, &doc); err != nil {
							t.Fatal(err)
						}
						doc["format_version"] = json.RawMessage("99")
						raw, err = json.Marshal(doc)
						if err != nil {
							t.Fatal(err)
						}
						if err = os.WriteFile(path, raw, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if cancelled {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					}
					spy := &setupRefusalStore{RunStore: st}
					calls := 0
					e := admissionSnapshotEngine(t, spy, nil, &calls)
					ordinary := errors.New("initial ordinary setup failure")
					status, code, reported := e.markFailedBestEffort(ctx, "run", phase, ordinary)
					want := store.ErrValueFormatChanged
					if to == 99 {
						want = store.ErrUnsupportedValueFormat
					}
					if !errors.Is(reported, want) || spy.writes != 1 || spy.events != 0 {
						t.Fatalf("reported=%v want=%v writes=%d events=%d", reported, want, spy.writes, spy.events)
					}
					if got := setupErrFrom(status, code, fmt.Errorf("%s: %w", phase, reported)); !errors.Is(got, want) {
						t.Fatalf("conversion lost refusal: %v", got)
					}
				})
			}
		}
	}
}

func TestSetupRefusalAndOwnershipPrecedeAllWritesAndConversions(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{errors.Join(store.ErrValueFormatChanged, context.Canceled), &RuntimeError{Code: ErrCodeExecutionFailed, Cause: errors.Join(store.ErrUnsupportedValueFormat, context.DeadlineExceeded)}, errors.Join(errCorrectionOwnershipLost, context.Canceled)} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			spy := &setupRefusalStore{}
			calls := 0
			e := admissionSnapshotEngine(t, spy, nil, &calls)
			status, code, reported := e.markFailedBestEffort(t.Context(), "run", "sandbox start", cause)
			if reported != cause || spy.writes != 0 || spy.events != 0 {
				t.Fatalf("priority lost: %v writes=%d events=%d", reported, spy.writes, spy.events)
			}
			if got := setupErrFrom(store.RunStatusCancelled, code, reported); got != cause {
				t.Fatalf("conversion replaced refusal: %v status=%s", got, status)
			}
		})
	}
}
