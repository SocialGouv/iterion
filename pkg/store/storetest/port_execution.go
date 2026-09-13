package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func portStateFixture(t *testing.T, s store.RunStore, id string) (context.Context, *store.PortExecution) {
	t.Helper()
	ctx := store.WithRuntimeSemantics(testCtx(), store.RuntimeSemanticsPortsV1)
	if _, err := s.CreateRun(ctx, id, "ports", nil); err != nil {
		t.Fatal(err)
	}
	identity := store.PortExecutionIdentity{Source: "source", Graph: "graph", Contract: "contract", Policy: "policy", Inputs: "inputs", Dependencies: map[string]string{"child.bot": "captured-child"}}
	state := &store.PortExecution{
		Version: store.PortExecutionVersion, Revision: 1, Generation: 1, RootRunID: id, Identity: identity,
		Invocations:  map[string]*store.PortInvocation{"producer": {ID: "producer", Node: "producer", Attempt: 1, Status: store.PortPending, Identity: identity, Inputs: map[string]string{"value": "input.value"}}},
		Publications: map[string]*store.PortValue{"input.value": portTestValue(t, "input.value", "input", "value", 0, `{"integer":9007199254740993,"fraction":1.234567890123456789,"null":null,"empty":[],"markup":"<report>&"}`)},
	}
	if err := store.SavePortExecution(ctx, s, id, 0, state); err != nil {
		t.Fatal(err)
	}
	return ctx, state
}

func portTestValue(t *testing.T, revision, producer, port string, attempt int, data string) *store.PortValue {
	t.Helper()
	value, err := store.NewPortValue(revision, producer, port, attempt, json.RawMessage(data))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func clonePortState(t *testing.T, state *store.PortExecution) *store.PortExecution {
	t.Helper()
	copy, err := state.Clone()
	if err != nil {
		t.Fatal(err)
	}
	return copy
}

func commitPortState(t *testing.T, ctx context.Context, s store.RunStore, state *store.PortExecution) {
	t.Helper()
	previous := state.Revision
	state.Revision++
	if err := store.SavePortExecution(ctx, s, state.RootRunID, previous, state); err != nil {
		t.Fatal(err)
	}
}

var errPortInjectedCrash = errors.New("injected crash at checkpoint commit")

type crashPortStore struct {
	store.RunStore
	after bool
}

func (s crashPortStore) SaveRun(ctx context.Context, run *store.Run) error {
	if s.after {
		if err := s.RunStore.SaveRun(ctx, run); err != nil {
			return err
		}
	}
	return errPortInjectedCrash
}

// RunPortExecutionState checks the native commit boundary through real
// RunStore implementations. The wrapper injects process-loss windows before
// the atomic store call and after its durable acknowledgment, not fake state.
func RunPortExecutionState(t *testing.T, factory Factory) {
	t.Run("AtomicPublicationAndLostAcknowledgment", func(t *testing.T) {
		s := factory(t)
		ctx, state := portStateFixture(t, s, "pc1_atomic")
		state.Invocations["producer"].Status = store.PortAdmitted
		state.Budget.Reservations = map[string]store.PortBudgetAmount{"producer": {Tokens: 100, Iterations: 1}}
		commitPortState(t, ctx, s, state)
		state.Invocations["producer"].Status = store.PortRunning
		commitPortState(t, ctx, s, state)
		// A crash after artifact staging leaves actual bytes on disk/S3 but
		// no successful invocation or publication in the authoritative state.
		if err := s.WriteArtifact(ctx, &store.Artifact{RunID: state.RootRunID, NodeID: "producer", Version: 1, Data: map[string]any{"value": "staged"}}); err != nil {
			t.Fatal(err)
		}
		next := clonePortState(t, state)
		next.Revision++
		next.Invocations["producer"].Status = store.PortSucceeded
		next.Invocations["producer"].Outputs = map[string]string{"value": "producer.value.1"}
		next.Publications["producer.value.1"] = portTestValue(t, "producer.value.1", "producer", "value", 1, `{"integer":9007199254740993,"null":null,"empty":[]}`)
		next.Exports = map[string]string{"report": "producer.value.1"}
		next.Products = []string{"report"}
		next.Budget.Reservations = map[string]store.PortBudgetAmount{}
		next.Budget.Consumed = store.PortBudgetAmount{Tokens: 17, Iterations: 1}
		if err := store.SavePortExecution(ctx, crashPortStore{RunStore: s}, state.RootRunID, state.Revision, next); !errors.Is(err, errPortInjectedCrash) {
			t.Fatal(err)
		}
		loaded, err := s.LoadRun(ctx, state.RootRunID)
		if err != nil || !reflect.DeepEqual(loaded.PortExecution, state) {
			t.Fatalf("uncommitted publication became visible: %+v, %v", loaded, err)
		}
		if err := store.SavePortExecution(ctx, crashPortStore{RunStore: s, after: true}, state.RootRunID, state.Revision, next); !errors.Is(err, errPortInjectedCrash) {
			t.Fatal(err)
		}
		// A lost acknowledgment must not execute the producer or charge the
		// budget again. Retrying the exact committed snapshot is idempotent.
		if err := store.SavePortExecution(ctx, s, state.RootRunID, state.Revision, next); err != nil {
			t.Fatal(err)
		}
		loaded, err = s.LoadRun(ctx, state.RootRunID)
		if err != nil || !reflect.DeepEqual(loaded.PortExecution, next) {
			t.Fatalf("publication, status and reservation did not commit together: %+v, %v", loaded, err)
		}
		loaded.Name = "renamed by operator"
		if err := s.SaveRun(ctx, loaded); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveCheckpoint(ctx, loaded.ID, &store.Checkpoint{NodeID: "unrelated legacy metadata"}); err != nil {
			t.Fatal(err)
		}
		loaded, err = s.LoadRun(ctx, state.RootRunID)
		if err != nil || !reflect.DeepEqual(loaded.PortExecution, next) {
			t.Fatalf("metadata write dropped or coerced native state: %+v, %v", loaded, err)
		}
	})

	t.Run("ConcurrentCoordinatorsCannotOverwrite", func(t *testing.T) {
		s := factory(t)
		ctx, state := portStateFixture(t, s, "pc1_cas")
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			next := clonePortState(t, state)
			next.Revision++
			next.Invocations["producer"].Status = store.PortAdmitted
			next.Budget.Reservations = map[string]store.PortBudgetAmount{"producer": {Tokens: int64(10 + i), Iterations: 1}}
			wg.Go(func() {
				<-start
				results <- store.SavePortExecution(ctx, s, state.RootRunID, state.Revision, next)
			})
		}
		close(start)
		wg.Wait()
		close(results)
		wins, conflicts := 0, 0
		for err := range results {
			if err == nil {
				wins++
			} else if errors.Is(err, store.ErrRunConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
		}
	})

	t.Run("RejectPartialPublicationAndSemanticLoss", func(t *testing.T) {
		s := factory(t)
		ctx, state := portStateFixture(t, s, "pc1_invalid")
		for name, mutate := range map[string]func(*store.PortExecution){
			"future format": func(s *store.PortExecution) { s.Version++ },
			"unfinished producer": func(s *store.PortExecution) {
				s.Publications["early"] = portTestValue(t, "early", "producer", "value", 1, `"early"`)
			},
			"invented success":       func(s *store.PortExecution) { s.Invocations["producer"].Status = store.PortSucceeded },
			"unpublished export":     func(s *store.PortExecution) { s.Exports = map[string]string{"product": "missing"} },
			"tampered input":         func(s *store.PortExecution) { s.Publications["input.value"].Data = json.RawMessage(`"different"`) },
			"unknown effect outcome": func(s *store.PortExecution) { s.Invocations["producer"].Status = "unknown_new_state" },
			"identity replacement":   func(s *store.PortExecution) { s.Identity.Dependencies["child.bot"] = "changed" },
			"reservation without admission": func(s *store.PortExecution) {
				s.Budget.Reservations = map[string]store.PortBudgetAmount{"producer": {Tokens: 10}}
			},
			"partial collection": func(s *store.PortExecution) {
				s.Collections = map[string]*store.PortCollection{"collection": {ID: "collection", Node: "producer", Complete: true, Items: []string{"producer"}}}
			},
		} {
			t.Run(name, func(t *testing.T) {
				next := clonePortState(t, state)
				next.Revision++
				mutate(next)
				if err := store.SavePortExecution(ctx, s, state.RootRunID, state.Revision, next); !errors.Is(err, store.ErrRunSemantics) {
					t.Fatalf("invalid state accepted: %v", err)
				}
			})
		}
		loaded, err := s.LoadRun(ctx, state.RootRunID)
		if err != nil || !reflect.DeepEqual(loaded.PortExecution, state) {
			t.Fatalf("rejected writes changed durable state: %+v, %v", loaded, err)
		}
		loaded.PortExecution = nil
		if err := s.SaveRun(ctx, loaded); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("full save silently discarded checkpoint: %v", err)
		}
	})

	t.Run("UncertainEffectsRequireRecoveryDecision", func(t *testing.T) {
		s := factory(t)
		ctx, state := portStateFixture(t, s, "pc1_uncertain")
		state.Invocations["producer"].Status = store.PortAdmitted
		commitPortState(t, ctx, s, state)
		state.Invocations["producer"].Status = store.PortRunning
		state.Invocations["producer"].EffectDispatched = true
		commitPortState(t, ctx, s, state)
		state.Invocations["producer"].Status = store.PortUncertain
		commitPortState(t, ctx, s, state)
		next := clonePortState(t, state)
		next.Revision++
		next.Invocations["producer"].Status = store.PortPending
		next.Invocations["producer"].Attempt++
		next.Invocations["producer"].EffectDispatched = false
		if err := store.SavePortExecution(ctx, s, state.RootRunID, state.Revision, next); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("effect silently retried: %v", err)
		}
		next.Invocations["producer"].RecoveryDecision = "operator confirmed effect did not occur"
		next.Invocations["producer"].RecoveryAttempt = 1
		if err := store.SavePortExecution(ctx, s, state.RootRunID, state.Revision, next); err != nil {
			t.Fatal(err)
		}
		loaded, err := s.LoadRun(ctx, state.RootRunID)
		if err != nil || loaded.PortExecution.Invocations["producer"].Attempt != 2 || loaded.PortExecution.Invocations["producer"].RecoveryDecision == "" {
			t.Fatalf("recovery evidence missing: %+v, %v", loaded, err)
		}
		state = loaded.PortExecution
		state.Invocations["producer"].Status = store.PortAdmitted
		commitPortState(t, ctx, s, state)
		state.Invocations["producer"].Status = store.PortRunning
		state.Invocations["producer"].EffectDispatched = true
		commitPortState(t, ctx, s, state)
		state.Invocations["producer"].Status = store.PortUncertain
		commitPortState(t, ctx, s, state)
		next = clonePortState(t, state)
		next.Revision++
		next.Invocations["producer"].Status = store.PortPending
		next.Invocations["producer"].Attempt++
		next.Invocations["producer"].EffectDispatched = false
		if err := store.SavePortExecution(ctx, s, state.RootRunID, state.Revision, next); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("decision from attempt one authorized retry of attempt two: %v", err)
		}
	})
}
