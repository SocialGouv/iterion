package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCheckpointContextDoesNotRecreateAbsentRun(t *testing.T) {
	for _, p := range []RunDataProtocol{0, 1} {
		for _, f := range []ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d-f%d", p, f), func(t *testing.T) {
				s, c := checkpointBindingFixture(t, f, p)
				if e := s.DeleteRun(c, "r"); e != nil {
					t.Fatal(e)
				}
				if _, e := s.PruneDeletionMarkers(context.Background(), time.Now().Add(time.Hour)); e != nil {
					t.Fatal(e)
				}
				ops := []func(context.Context) error{func(x context.Context) error { return s.SaveCheckpoint(x, "r", nil) }, func(x context.Context) error { return s.PauseRun(x, "r", nil) }, func(x context.Context) error { return s.FailRunResumable(x, "r", nil, "late", "") }, func(x context.Context) error { return s.FailRunTerminal(x, "r", nil, "late", "") }}
				want := ErrRunNotFound
				if p == HistoricalRunData {
					want = ErrRunDataIdentityChanged
				}
				for _, op := range ops {
					if e := op(c); !errors.Is(e, want) {
						t.Fatal("missing run was adopted/recreated or falsely classified", e)
					}
					cc, cancel := context.WithCancel(c)
					cancel()
					if e := op(cc); !errors.Is(e, context.Canceled) {
						t.Fatal("cancellation lost precedence", e)
					}
				}
				if _, e := s.LoadRun(c, "r"); !errors.Is(e, ErrRunNotFound) {
					t.Fatal("refusal recreated metadata", e)
				}
			})
		}
	}

}
