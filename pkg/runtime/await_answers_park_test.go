package runtime

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestAwaitAnswersPersistsOnlyWhileParked(t *testing.T) {
	for _, exit := range []string{"cancel", "answer", "timeout"} {
		t.Run(exit, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				s := tmpStore(t)
				const id = "await-park"
				if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
					t.Fatal(err)
				}
				if err := s.WriteInteraction(ctx, &store.Interaction{ID: "question", RunID: id, NodeID: "asker", Kind: store.InteractionKindAsync, RequestedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
				e := New(&ir.Workflow{Name: "wf"}, s, newStubExecutor())
				rs := e.newRunState(id, nil)
				rs.ctx = ctx
				done := make(chan error, 1)
				go func() {
					_, err := e.awaitAsyncAnswers(ctx, rs, "sync", &ir.AwaitAnswersNode{Timeout: 10 * time.Second})
					done <- err
				}()
				joined := false
				defer func() {
					cancel()
					synctest.Wait()
					if !joined {
						<-done
					}
				}()
				synctest.Wait()
				run, err := s.LoadRun(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if len(run.AwaitAnswersWaits) != 1 {
					t.Fatalf("persisted waits=%v, want exactly the parked sync point", run.AwaitAnswersWaits)
				}
				for _, wait := range run.AwaitAnswersWaits {
					if wait.NodeID != "sync" || !wait.Until.Equal(time.Now().Add(10*time.Second)) {
						t.Fatalf("park proof=%+v", wait)
					}
				}
				switch exit {
				case "cancel":
					cancel()
				case "answer":
					if _, err := store.AnswerInteraction(ctx, s, id, "question", map[string]any{"answer": "blue"}); err != nil {
						t.Fatal(err)
					}
					e.NotifyInteractionAnswered()
				case "timeout":
					time.Sleep(10 * time.Second)
				}
				synctest.Wait()
				result := <-done
				joined = true
				if exit == "answer" && result != nil {
					t.Fatalf("answered wait: %v", result)
				}
				if exit == "cancel" && !errors.Is(result, context.Canceled) {
					t.Fatalf("cancelled wait: %v", result)
				}
				if exit == "timeout" {
					var re *RuntimeError
					if !errors.As(result, &re) || re.Code != ErrCodeTimeout {
						t.Fatalf("timed out wait: %v", result)
					}
				}
				run, err = s.LoadRun(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				if len(run.AwaitAnswersWaits) != 0 {
					t.Fatalf("cancelled sync point still exempts the run: %+v", run.AwaitAnswersWaits)
				}
			})
		})
	}
}

type failingAwaitWaitStore struct {
	store.RunStore
	failClear bool
	err       error
}

func (s failingAwaitWaitStore) SetAwaitAnswersWait(ctx context.Context, id, token string, wait *store.AwaitAnswersWait) error {
	if (wait == nil) == s.failClear {
		return s.err
	}
	return s.RunStore.SetAwaitAnswersWait(ctx, id, token, wait)
}

func TestAwaitAnswersReportsPersistenceFailure(t *testing.T) {
	for _, failClear := range []bool{false, true} {
		name := "enter"
		if failClear {
			name = "leave"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				s := tmpStore(t)
				if _, err := s.CreateRun(ctx, "r", "wf", nil); err != nil {
					t.Fatal(err)
				}
				if err := s.WriteInteraction(ctx, &store.Interaction{ID: "q", RunID: "r", NodeID: "ask", Kind: store.InteractionKindAsync, RequestedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("wait write unavailable")
				e := New(&ir.Workflow{Name: "wf"}, failingAwaitWaitStore{RunStore: s, failClear: failClear, err: failure}, newStubExecutor())
				rs := e.newRunState("r", nil)
				rs.ctx = ctx
				done := make(chan error, 1)
				go func() {
					_, err := e.awaitAsyncAnswers(ctx, rs, "sync", &ir.AwaitAnswersNode{Timeout: time.Minute})
					done <- err
				}()
				synctest.Wait()
				cancel()
				result := <-done
				if !errors.Is(result, failure) {
					t.Fatalf("lost storage error: %v", result)
				}
				if failClear && !errors.Is(result, context.Canceled) {
					t.Fatalf("cleanup erased cancellation: %v", result)
				}
				if !failClear && store.HasBlockingHumanWait(context.Background(), s, "r", time.Now()) {
					t.Fatal("failed write left a false wait exemption")
				}
				if store.HasBlockingHumanWait(context.Background(), s, "r", time.Now().Add(time.Minute)) {
					t.Fatal("failed cleanup left an unbounded exemption")
				}
			})
		})
	}
}
