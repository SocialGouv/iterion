package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestFanoutTopologyFailureClass(t *testing.T) {
	for _, topology := range []string{"all", "each", "multi"} {
		t.Run(topology, func(t *testing.T) {
			for _, kind := range []string{"healthy", "cancel", "interrupt", "deadline"} {
				t.Run(kind, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						wf := fanOutWorkflow(ir.AwaitWaitAll)
						if topology == "each" {
							wf = fanOutEachWorkflow(false, ir.AwaitWaitAll, 2)
						}
						if topology == "multi" {
							router := wf.Nodes["router"].(*ir.RouterNode)
							router.RouterMode = ir.RouterLLM
							router.RouterMulti = true
							router.Model = "fixture"
						}
						base := tmpStore(t)
						s := &fanoutObservedStore{RunStore: base}
						var ctx context.Context
						var cancel func()
						if kind == "deadline" {
							ctx, cancel = context.WithTimeout(t.Context(), time.Hour)
						} else {
							c, stop := context.WithCancelCause(t.Context())
							ctx = c
							cancel = func() {
								if kind == "interrupt" {
									stop(ErrRunInterrupted)
								} else {
									stop(context.Canceled)
								}
							}
						}
						defer cancel()
						var entered sync.WaitGroup
						entered.Add(2)
						var completed sync.WaitGroup
						completed.Add(1)
						go func() {
							defer completed.Done()
							entered.Wait()
							if kind == "deadline" {
								time.Sleep(time.Hour)
							} else if kind != "healthy" {
								cancel()
							}
						}()
						x := newStubExecutor()
						x.on("router", func(map[string]any) (map[string]any, error) {
							return map[string]any{"selected_routes": []any{"agent_a", "agent_b"}}, nil
						})
						x.on("entry", func(map[string]any) (map[string]any, error) {
							return map[string]any{"summary": "go", "items": []any{item("A"), item("B")}}, nil
						})
						for _, name := range []string{"agent_a", "agent_b", "handle"} {
							x.on(name, func(map[string]any) (map[string]any, error) {
								entered.Done()
								if kind != "healthy" {
									<-ctx.Done()
									return nil, ctx.Err()
								}
								return map[string]any{}, nil
							})
						}
						err := New(wf, s, x).Run(ctx, "fanout-observe", nil)
						completed.Wait()
						r, re := base.LoadRun(t.Context(), "fanout-observe")
						if re != nil {
							t.Fatal(re)
						}
						s.mu.Lock()
						rows := append([]fanoutWriteObservation(nil), s.writes...)
						s.mu.Unlock()
						t.Logf("FANOUT_OBSERVED kind=%s status=%s code=%s err=%v cancelled=%v interrupted=%v ctxcause=%v writes=%+v", kind, r.Status, r.FailureCode, err, errors.Is(err, ErrRunCancelled), errors.Is(err, ErrRunInterrupted), context.Cause(ctx), rows)
						if kind == "healthy" {
							if err != nil || r.Status != store.RunStatusFinished || r.FailureCode != "" || context.Cause(ctx) != nil || len(rows) != 0 {
								t.Fatalf("HEALTHY_CHANGED err=%v status=%s code=%s cause=%v writes=%v", err, r.Status, r.FailureCode, context.Cause(ctx), rows)
							}
							return
						}
						wantCode := ErrCodeExecutionFailed
						wantCause := context.Canceled
						if kind == "interrupt" {
							wantCause = ErrRunInterrupted
						}
						if kind == "deadline" {
							wantCode = ErrCodeTimeout
							wantCause = context.DeadlineExceeded
						}
						var rt *RuntimeError
						if r.Status != store.RunStatusFailedResumable || r.FailureCode != wantCode || !errors.As(err, &rt) || rt.Code != wantCode || context.Cause(ctx) != wantCause {
							t.Fatalf("FANOUT_TERMINAL_VERDICT_CHANGED status=%s code=%s wantCode=%s err=%v originalCause=%v wantCause=%v", r.Status, r.FailureCode, wantCode, err, context.Cause(ctx), wantCause)
						}
						// The historical fanout wrapper flattens cancellation into its
						// message; the checkpoint door returns a classified verdict.
						// It must not claim a separately persisted cancellation/interruption.
						if errors.Is(err, ErrRunCancelled) || errors.Is(err, ErrRunInterrupted) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("FANOUT_CAUSE_POLICY_CHANGED %v", err)
						}
						if r.Checkpoint == nil || r.Checkpoint.Parallel == nil || len(rows) != 2 || rows[0].Method != "resumable" || rows[0].Code != wantCode || rows[0].Context != nil || rows[0].Result != nil || rows[1].Method != "terminal-event" || rows[1].Context != nil || rows[1].Result != nil {
							t.Fatalf("FANOUT_TERMINAL_PUBLICATION_CHANGED checkpoint=%+v writes=%v", r.Checkpoint, rows)
						}
					})
				})
			}
		})
	}
}
