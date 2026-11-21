package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicRuntimeValueFormatRefusalHasNoExecutionOrSuccessorWrites(t *testing.T) {
	t.Parallel()
	for _, resume := range []bool{false, true} {
		for _, from := range []store.ValueFormat{1, 2} {
			name := "run"
			if resume {
				name = "resume"
			}
			t.Run(name+string(rune('0'+from)), func(t *testing.T) {
				t.Parallel()
				s := tmpStore(t)
				to := store.ValueFormat(3) - from
				r, err := store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "successor", Format: to})
				if err != nil {
					t.Fatal(err)
				}
				if resume {
					r.Status = store.RunStatusFailedResumable
					if err = s.SaveRun(t.Context(), r); err != nil {
						t.Fatal(err)
					}
				}
				path := filepath.Join(s.Root(), "runs", r.ID, "run.json")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				ctx, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: r.ID, Format: from})
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				e := admissionSnapshotEngine(t, s, nil, &calls)
				if resume {
					err = e.Resume(ctx, r.ID, nil)
				} else {
					err = e.Run(ctx, r.ID, nil)
				}
				if !errors.Is(err, store.ErrValueFormatChanged) || calls != 0 {
					t.Fatalf("wrong format executed: %v calls=%d", err, calls)
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || string(before) != string(after) {
					t.Fatalf("successor mutated: %v", readErr)
				}
				if _, err = os.Stat(filepath.Join(s.Root(), "runs", r.ID, "events.jsonl")); !os.IsNotExist(err) {
					t.Fatalf("refusal emitted events: %v", err)
				}
			})
		}
	}
}

type invocationReadFailureStore struct {
	store.RunStore
	cause   error
	creates int
}

func (s *invocationReadFailureStore) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, s.cause
}
func (s *invocationReadFailureStore) CreateRun(context.Context, string, string, map[string]any) (*store.Run, error) {
	s.creates++
	return nil, errors.New("unexpected create")
}
func TestPublicRunDoesNotCreateAfterLoadFailureOrBoundAbsence(t *testing.T) {
	t.Parallel()
	infra := errors.New("read unavailable")
	for _, tc := range []struct {
		name  string
		bound bool
		cause error
	}{{"infra", false, infra}, {"existing vanished", true, store.ErrRunNotFound}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := tmpStore(t)
			s := &invocationReadFailureStore{RunStore: base, cause: tc.cause}
			ctx := t.Context()
			if tc.bound {
				ctx, _ = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "run", Format: 1})
			}
			calls := 0
			e := admissionSnapshotEngine(t, s, nil, &calls)
			err := e.Run(ctx, "run", nil)
			if !errors.Is(err, tc.cause) || s.creates != 0 || calls != 0 {
				t.Fatalf("cause lost or effect: err=%v creates=%d exec=%d", err, s.creates, calls)
			}
			if _, err = base.LoadRun(t.Context(), "run"); !errors.Is(err, store.ErrRunNotFound) {
				t.Fatalf("unexpected row: %v", err)
			}
		})
	}
}
func TestPublicRuntimeKeepsSameFormatInvocation(t *testing.T) {
	t.Parallel()
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(string(rune('0'+format)), func(t *testing.T) {
			t.Parallel()
			s := tmpStore(t)
			r, err := store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "run", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			r.Name = "metadata changed"
			if err = s.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			calls := 0
			e := admissionSnapshotEngine(t, s, nil, &calls)
			if err = e.Run(ctx, "run", nil); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("exec calls=%d", calls)
			}
			got, err := s.LoadRun(t.Context(), "run")
			if err != nil || got.FormatVersion != int(format) {
				t.Fatalf("format changed: %+v %v", got, err)
			}
		})
	}
}

func TestAttachmentPromotionReloadKeepsOriginalFormat(t *testing.T) {
	t.Parallel()
	for _, from := range []store.ValueFormat{1, 2} {
		t.Run(string(rune('0'+from)), func(t *testing.T) {
			t.Parallel()
			st := tmpStore(t)
			original, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: from})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), original)
			if err != nil {
				t.Fatal(err)
			}
			if err = st.DeleteRun(ctx, "run"); err != nil {
				t.Fatal(err)
			}
			if _, err = st.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: 3 - from}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			e := admissionSnapshotEngine(t, st, nil, &calls)
			got, err := e.runPromoteAttachments(ctx, "run", original)
			if !errors.Is(err, store.ErrValueFormatChanged) || got != nil {
				t.Fatalf("promotion adopted replacement: got=%+v error=%v", got, err)
			}
		})
	}
}

func TestPublicRunCancelledBeforeBirthAcquiresNothing(t *testing.T) {
	t.Parallel()
	st := tmpStore(t)
	calls := 0
	e := admissionSnapshotEngine(t, st, nil, &calls)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := e.Run(ctx, "not-born", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls=%d", calls)
	}
	if _, err := st.LoadRun(t.Context(), "not-born"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("cancelled birth wrote run: %v", err)
	}
}
