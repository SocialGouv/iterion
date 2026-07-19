package sessionboard

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestConfigWithDefaults(t *testing.T) {
	tests := []struct {
		name         string
		in           Config
		wantCooldown time.Duration
		wantMax      int
	}{
		{"zero gets defaults", Config{}, DefaultCooldown, DefaultMaxEvals},
		{"negative gets defaults", Config{Cooldown: -time.Second, MaxEvals: -1}, DefaultCooldown, DefaultMaxEvals},
		{"explicit values kept", Config{Cooldown: time.Minute, MaxEvals: 7}, time.Minute, 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.Cooldown != tc.wantCooldown || got.MaxEvals != tc.wantMax {
				t.Errorf("withDefaults() = {Cooldown:%v MaxEvals:%d}, want {%v %d}",
					got.Cooldown, got.MaxEvals, tc.wantCooldown, tc.wantMax)
			}
		})
	}
}

func TestNewNilGuards(t *testing.T) {
	obs := staticObserver{}
	emit := &fakeEmitter{}
	eval := &fakeEvaluator{}
	if New(nil, emit, "r", Config{}, eval, nil) != nil {
		t.Error("nil observer should yield nil coordinator")
	}
	if New(obs, nil, "r", Config{}, eval, nil) != nil {
		t.Error("nil emitter should yield nil coordinator")
	}
	if New(obs, emit, "", Config{}, eval, nil) != nil {
		t.Error("empty run id should yield nil coordinator")
	}
	c := New(obs, emit, "r", Config{}, eval, nil)
	if c == nil {
		t.Fatal("valid inputs should yield a coordinator")
	}
	// Nil receiver is safe on the public lifecycle methods.
	var nilC *Coordinator
	nilC.Start(context.Background())
	nilC.Close()
}

func TestCoordinatorIngest(t *testing.T) {
	ev := func(seq int64, typ store.EventType, node string) *store.Event {
		return &store.Event{Seq: seq, Type: typ, NodeID: node, Timestamp: time.Now()}
	}
	newC := func() *Coordinator {
		return New(staticObserver{}, &fakeEmitter{}, "r", Config{}, &fakeEvaluator{}, nil)
	}

	t.Run("nil event is a no-op", func(t *testing.T) {
		c := newC()
		c.ingest(nil)
		if c.lastSeq != 0 || len(c.recent) != 0 {
			t.Errorf("nil event mutated state: lastSeq=%d recent=%d", c.lastSeq, len(c.recent))
		}
	})

	t.Run("active node tracking", func(t *testing.T) {
		c := newC()
		c.ingest(ev(1, store.EventNodeStarted, "a"))
		if c.activeNode != "a" {
			t.Fatalf("activeNode = %q, want a", c.activeNode)
		}
		c.ingest(ev(2, store.EventNodeStarted, "b"))
		if c.activeNode != "b" {
			t.Fatalf("activeNode = %q, want b", c.activeNode)
		}
		// Finishing a node that is not the active one leaves it in place.
		c.ingest(ev(3, store.EventNodeFinished, "a"))
		if c.activeNode != "b" {
			t.Errorf("finish of non-active node cleared activeNode: %q", c.activeNode)
		}
		c.ingest(ev(4, store.EventNodeFinished, "b"))
		if c.activeNode != "" {
			t.Errorf("finish of active node should clear it, got %q", c.activeNode)
		}
	})

	t.Run("lastSeq is monotonic max", func(t *testing.T) {
		c := newC()
		c.ingest(ev(5, store.EventNodeStarted, "a"))
		c.ingest(ev(3, store.EventNodeFinished, "a"))
		if c.lastSeq != 5 {
			t.Errorf("lastSeq = %d, want 5 (must not regress)", c.lastSeq)
		}
	})

	t.Run("recent events capped", func(t *testing.T) {
		c := newC()
		for i := 0; i < recentEventsCap+5; i++ {
			c.ingest(ev(int64(i+1), store.EventNodeStarted, "n"))
		}
		if len(c.recent) != recentEventsCap {
			t.Errorf("recent len = %d, want cap %d", len(c.recent), recentEventsCap)
		}
	})
}

// errEvaluator always fails.
type errEvaluator struct{ calls int }

func (e *errEvaluator) Evaluate(context.Context, EvalInput) (*BoardDecision, EvalUsage, error) {
	e.calls++
	return nil, EvalUsage{}, errors.New("eval boom")
}

// failNEmitter fails the first n publishes, then records and succeeds.
type failNEmitter struct {
	n     int
	calls int
	specs []Spec
}

func (e *failNEmitter) Publish(_ context.Context, _ string, spec Spec) error {
	e.calls++
	if e.calls <= e.n {
		return fmt.Errorf("publish boom %d", e.calls)
	}
	e.specs = append(e.specs, spec)
	return nil
}

func TestCoordinatorEvaluate(t *testing.T) {
	newC := func(cfg Config, eval Evaluator, emit Emitter) *Coordinator {
		c := New(staticObserver{}, emit, "run_1", cfg, eval, nil)
		if c == nil {
			t.Fatal("New returned nil")
		}
		c.ctx = context.Background()
		return c
	}

	t.Run("max evals budget", func(t *testing.T) {
		eval := &fakeEvaluator{}
		c := newC(Config{Cooldown: time.Millisecond, MaxEvals: 2}, eval, &fakeEmitter{})
		for i := 0; i < 4; i++ {
			c.evaluate("turn_boundary", true)
		}
		if eval.calls != 2 {
			t.Errorf("evaluator calls = %d, want 2 (budget)", eval.calls)
		}
		// Characterization: the log-once bump pushes evalCount to MaxEvals+1
		// and it stays there on later attempts.
		if c.evalCount != 3 {
			t.Errorf("evalCount = %d, want 3 (2 evals + log-once bump)", c.evalCount)
		}
	})

	t.Run("cooldown floor honoured unless bypassed", func(t *testing.T) {
		eval := &fakeEvaluator{}
		c := newC(Config{Cooldown: time.Hour, MaxEvals: 10}, eval, &fakeEmitter{})
		c.evaluate("turn_boundary", false) // first eval always runs (lastEvalAt zero)
		c.evaluate("turn_boundary", false) // inside cooldown → skipped
		if eval.calls != 1 {
			t.Fatalf("evaluator calls = %d, want 1 (cooldown skip)", eval.calls)
		}
		c.evaluate("run_terminated", true) // bypass ignores cooldown
		if eval.calls != 2 {
			t.Errorf("evaluator calls = %d, want 2 (bypass)", eval.calls)
		}
	})

	t.Run("applied decision stamps UpdatedSeq and publishes", func(t *testing.T) {
		eval := &fakeEvaluator{decs: []BoardDecision{{
			Upsert: []Widget{{ID: "w1", Kind: KindNote, Props: map[string]any{"text": "hi"}}},
		}}}
		emit := &fakeEmitter{}
		c := newC(Config{Cooldown: time.Millisecond, MaxEvals: 5}, eval, emit)
		c.ingest(&store.Event{Seq: 7, Type: store.EventNodeStarted, NodeID: "n", Timestamp: time.Now()})
		c.evaluate("turn_boundary", true)
		spec, ok := emit.last()
		if !ok {
			t.Fatal("no spec published")
		}
		if spec.Version != 1 || spec.UpdatedSeq != 7 || len(spec.Widgets) != 1 {
			t.Errorf("published spec = %+v, want v1 seq7 1 widget", spec)
		}
	})

	t.Run("no-op decision publishes nothing", func(t *testing.T) {
		eval := &fakeEvaluator{} // returns empty decisions
		emit := &fakeEmitter{}
		c := newC(Config{Cooldown: time.Millisecond, MaxEvals: 5}, eval, emit)
		c.evaluate("turn_boundary", true)
		if _, ok := emit.last(); ok {
			t.Error("empty decision should not publish")
		}
	})

	t.Run("evaluator error consumes budget and cooldown", func(t *testing.T) {
		eval := &errEvaluator{}
		emit := &fakeEmitter{}
		c := newC(Config{MaxEvals: 10}, eval, emit) // default 45s cooldown
		c.evaluate("turn_boundary", true)
		if eval.calls != 1 || c.evalCount != 1 || c.lastEvalAt.IsZero() {
			t.Fatalf("error eval bookkeeping: calls=%d count=%d lastEvalAt=%v",
				eval.calls, c.evalCount, c.lastEvalAt)
		}
		c.evaluate("turn_boundary", false) // still cooling down
		if eval.calls != 1 {
			t.Errorf("cooldown after error not honoured: calls=%d", eval.calls)
		}
		if _, ok := emit.last(); ok {
			t.Error("failed evaluation should not publish")
		}
	})

	t.Run("publish failure keeps spec at persisted state and retries next eval", func(t *testing.T) {
		// c.spec commits only after a successful Publish, so a failed publish
		// leaves the in-memory board mirroring the persisted one and the next
		// evaluation re-derives the diff and retries.
		dec := BoardDecision{
			Upsert: []Widget{{ID: "w1", Kind: KindMetric, Props: map[string]any{"value": 1}}},
		}
		eval := &fakeEvaluator{decs: []BoardDecision{dec, dec}}
		emit := &failNEmitter{n: 1}
		c := newC(Config{Cooldown: time.Millisecond, MaxEvals: 5}, eval, emit)

		c.evaluate("turn_boundary", true)
		if emit.calls != 1 {
			t.Fatalf("publish calls = %d, want 1", emit.calls)
		}
		if c.spec.Version != 0 || len(c.spec.Widgets) != 0 {
			t.Fatalf("failed publish advanced the in-memory spec: %+v", c.spec)
		}

		c.evaluate("turn_boundary", true)
		if emit.calls != 2 || len(emit.specs) != 1 {
			t.Fatalf("retry not published: calls=%d published=%d", emit.calls, len(emit.specs))
		}
		if c.spec.Version != 1 || len(c.spec.Widgets) != 1 {
			t.Errorf("spec not committed after successful retry: %+v", c.spec)
		}
		if emit.specs[0].Version != 1 {
			t.Errorf("published version = %d, want 1 (no phantom bump from the failed attempt)", emit.specs[0].Version)
		}
	})
}
