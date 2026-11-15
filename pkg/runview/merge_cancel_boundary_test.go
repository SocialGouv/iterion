package runview

import (
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type observedMergeCancelStore struct {
	cancelSensitiveStore
	cancel                          context.CancelFunc
	loads, claims, writes, acquired int
	writeCanceled                   bool
}

func (s *observedMergeCancelStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.loads++
	return s.RunStore.LoadRun(ctx, id)
}
func (s *observedMergeCancelStore) ClaimMerge(ctx context.Context, id string, before time.Time) (bool, store.MergeStatus, time.Time, error) {
	s.claims++
	ok, prior, token, err := s.RunStore.ClaimMerge(ctx, id, before)
	if ok && err == nil {
		s.acquired++
		if s.cancel != nil {
			s.cancel()
		}
	}
	return ok, prior, token, err
}
func (s *observedMergeCancelStore) UpdateRunMergeIf(ctx context.Context, id string, u store.RunMergeUpdate, from []store.MergeStatus) (bool, error) {
	s.writes++
	s.writeCanceled = ctx.Err() != nil
	return s.cancelSensitiveStore.UpdateRunMergeIf(ctx, id, u, from)
}
func TestPerformMergeCancellationRespectsClaimBoundary(t *testing.T) {
	for _, when := range []string{"before-entry", "after-claim"} {
		t.Run(when, func(t *testing.T) {
			st, _, id := seedMergeableRun(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			spy := &observedMergeCancelStore{cancelSensitiveStore: cancelSensitiveStore{st}}
			svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
			if err != nil {
				t.Fatal(err)
			}
			spy.loads, spy.claims, spy.writes = 0, 0, 0
			if when == "before-entry" {
				cancel()
			} else {
				spy.cancel = cancel
			}
			_, err = svc.PerformMergeCtx(ctx, id, MergeRequest{})
			fresh, readErr := st.LoadRun(t.Context(), id)
			if readErr != nil {
				t.Fatal(readErr)
			}
			t.Logf("phase=%s error=%v loads=%d claims=%d acquired=%d writes=%d writeCanceled=%v status=%s", when, err, spy.loads, spy.claims, spy.acquired, spy.writes, spy.writeCanceled, fresh.MergeStatus)
			if when == "before-entry" {
				if !errors.Is(err, context.Canceled) || spy.acquired != 0 || spy.writes != 0 {
					t.Fatalf("unexpected pre-entry effects")
				}
			} else if err != nil || spy.claims != 1 || spy.writes != 1 || spy.writeCanceled || fresh.MergeStatus != store.MergeStatusMerged {
				t.Fatalf("post-claim outcome lost")
			}
		})
	}
}
