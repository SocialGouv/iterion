package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

func TestUsageOriginBeforeExecute(t *testing.T) {
	for _, mode := range []string{"queue", "local", "foreign-child"} {
		t.Run(mode, func(t *testing.T) {
			s := tmpStore(t)
			wf := correctionEngineWorkflow()
			calls := 0
			ctx := store.WithTenant(context.Background(), "t")
			origin := store.UsageOrigin{Version: 1, RunID: "origin-runtime", Publication: "2026-11-04T00:00:00Z", TenantID: "t", OrgID: "org"}
			if mode == "foreign-child" {
				origin.RunID = "parent"
			}
			if mode != "local" {
				var err error
				ctx, err = WithUsageOrigin(ctx, origin)
				if err != nil {
					t.Fatal(err)
				}
			}
			origin.OrgID = "caller-mutated"
			seeded, seedErr := s.CreateRun(ctx, "origin-runtime", "main", nil)
			if seedErr != nil {
				t.Fatal(seedErr)
			}
			seeded.TenantID = "t"
			if seedErr = s.SaveRun(ctx, seeded); seedErr != nil {
				t.Fatal(seedErr)
			}
			b := observedBackend(func(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
				calls++
				r, err := s.LoadRun(ctx, "origin-runtime")
				if err != nil {
					t.Fatal(err)
				}
				if len(r.InitialUsageReservations) != 1 {
					t.Fatal("origin was not reserved before effect")
				}
				for _, res := range r.InitialUsageReservations {
					if mode == "local" {
						if res.Origin != nil {
							t.Fatal("local invocation acquired queue origin")
						}
					} else if res.Origin == nil || res.Origin.OrgID != "org" || res.Origin.RunID != r.ID {
						t.Fatalf("origin not immutable %+v", res.Origin)
					}
				}
				return delegate.Result{Output: map[string]any{"reply": "ok", "ok": true}, Tokens: 3, CostUSD: 1}, nil
			})
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg))
			e := New(wf, s, ex)
			err := e.Run(ctx, "origin-runtime", nil)
			if mode == "foreign-child" {
				if err == nil || calls != 0 {
					t.Fatalf("child inherited provenance calls=%d err=%v", calls, err)
				}
				return
			}
			if err != nil || calls != 1 {
				t.Fatal(calls, err)
			}
		})
	}
}
