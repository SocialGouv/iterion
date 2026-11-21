package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCancelledBeforeEntryHasNoRunAuthority(t *testing.T) {
	for _, mode := range []string{"absent", "historical-existing", "prepared-existing"} {
		for _, cause := range []string{"cancel", "interrupt", "deadline"} {
			t.Run(mode+"/"+cause, func(t *testing.T) {
				s := tmpStore(t)
				var original *store.Run
				ctx := t.Context()
				var prepared store.PreparedRunInvocation
				if mode != "absent" {
					var err error
					_, err = s.CreateRun(ctx, "entry", "entry", nil)
					if err != nil {
						t.Fatal(err)
					}
					original, err = s.LoadRun(ctx, "entry")
					if err != nil {
						t.Fatal(err)
					}
					if mode == "prepared-existing" {
						prepared, err = store.PrepareRunInvocation(ctx, s, store.ExistingRunInvocation{ID: "entry"})
						if err != nil {
							t.Fatal(err)
						}
						ctx = prepared.Context
					}
				}
				switch cause {
				case "cancel":
					c, f := context.WithCancel(ctx)
					f()
					ctx = c
				case "interrupt":
					c, f := context.WithCancelCause(ctx)
					f(ErrRunInterrupted)
					ctx = c
				case "deadline":
					c, f := context.WithDeadline(ctx, time.Unix(1, 0))
					defer f()
					ctx = c
				}
				wf := &ir.Workflow{Name: "entry", Entry: "step", Nodes: map[string]ir.Node{"step": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step"}}}}
				calls := 0
				x := newStubExecutor()
				x.on("step", func(map[string]any) (map[string]any, error) { calls++; return nil, nil })
				e := New(wf, s, x)
				var err error
				if mode == "prepared-existing" {
					err = e.RunPrepared(ctx, "entry", nil, prepared)
				} else {
					err = e.Run(ctx, "entry", nil)
				}
				if !errors.Is(err, ctx.Err()) || errors.Is(err, ErrRunCancelled) || errors.Is(err, ErrRunInterrupted) || calls != 0 {
					t.Fatalf("PREENTRY_EFFECT: err=%v context=%v calls=%d", err, ctx.Err(), calls)
				}
				got, readErr := s.LoadRun(t.Context(), "entry")
				if mode == "absent" {
					if !errors.Is(readErr, store.ErrRunNotFound) || got != nil {
						t.Fatalf("BIRTH_AFTER_CANCEL: %v %+v", readErr, got)
					}
				} else if readErr != nil || !reflect.DeepEqual(got, original) {
					t.Fatalf("EXISTING_RUN_MUTATED: err=%v before=%+v after=%+v", readErr, original, got)
				}
				events, err := s.LoadEvents(t.Context(), "entry")
				if err != nil || len(events) != 0 {
					t.Fatalf("PREENTRY_EVENT: %v %+v", err, events)
				}
			})
		}
	}
}
