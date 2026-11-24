package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

func TestPublicSetupFailureKeepsLaterFormatRefusal(t *testing.T) {
	t.Parallel()
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary error", true: "format replaced during setup"}[swap], func(t *testing.T) {
			t.Parallel()
			st := tmpStore(t)
			r, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: 1})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			original := errors.New("attachment preparation I/O failed")
			calls := 0
			e := admissionSnapshotEngine(t, st, nil, &calls)
			e.attachmentPromote = func(context.Context, string) error {
				if swap {
					if err := st.DeleteRun(ctx, "run"); err != nil {
						return err
					}
					if _, err := st.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
						return err
					}
					if _, err := store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: 2}); err != nil {
						return err
					}
				}
				return original
			}
			got := e.Run(ctx, "run", nil)
			want := original
			status := store.RunStatusFailed
			if swap {
				want = store.ErrValueFormatChanged
				status = store.RunStatusRunning
			}
			if !errors.Is(got, want) || calls != 0 {
				t.Fatalf("error=%v want %v calls=%d", got, want, calls)
			}
			fresh, err := st.LoadRun(t.Context(), "run")
			if err != nil || fresh.Status != status {
				t.Fatalf("status=%v error=%v want=%s", fresh, err, status)
			}
		})
	}
}
