package model

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"math"
	"sync"
	"testing"
)

func TestExecutionUsageFacts(t *testing.T) {
	t.Parallel()
	c := &ExecutionUsageCollector{}
	task := delegate.Task{SessionID: "s", SessionFingerprint: "fp"}
	c.observe("backend", task, delegate.Result{Tokens: 10, CostUSD: 10, CostIsSessionTotal: true})
	before, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.observe("backend", task, delegate.Result{Tokens: 7, CostUSD: 8, CostIsSessionTotal: true})
	c.observe("backend", task, delegate.Result{Tokens: 3, CostUSD: 2})
	c.observe("backend", task, delegate.Result{Tokens: 4})
	c.observe("backend", task, delegate.Result{UnreportedCalls: 2})
	got, err := c.Since(before)
	if err != nil || got.Tokens != 14 || got.UnpricedTokens != 4 || got.UnreportedCalls != 2 || got.OtherCostUSD != 2 || len(got.Sessions) != 1 || got.Sessions[0].CostUSD != 8 || got.Sessions[0].Fingerprint != "fp" {
		t.Fatalf("facts=%+v err=%v", got, err)
	}
	got.Sessions[0].CostUSD = 999
	snapshot, _ := c.Snapshot()
	if snapshot.Sessions[0].CostUSD != 10 {
		t.Fatal("snapshot aliases collector")
	}
	// Neither a sibling backend nor a credential rotation shares its maximum.
	c.observe("other", task, delegate.Result{Tokens: 1, CostUSD: 5, CostIsSessionTotal: true})
	c.observe("backend", task, delegate.Result{Tokens: 1, CostUSD: 6, CostIsSessionTotal: true, SessionFingerprint: "new"})
	snapshot, _ = c.Snapshot()
	if len(snapshot.Sessions) != 3 {
		t.Fatalf("identity collapsed %+v", snapshot)
	}
	child := WithExecutionUsageCollector(WithExecutionUsageCollector(context.Background(), c), nil)
	observeExecutionUsage(child, task, delegate.Result{Tokens: 99})
	after, _ := c.Snapshot()
	if after.Tokens != snapshot.Tokens {
		t.Fatal("child inherited collector")
	}
}
func TestExecutionUsageUnknownAndUnreported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                         string
		r                            delegate.Result
		tokens, unpriced, unreported int
	}{
		{"empty", delegate.Result{}, 0, 0, 0},
		{"unknown tokens", delegate.Result{Tokens: 4}, 4, 4, 0},
		{"unreported only", delegate.Result{UnreportedCalls: 1}, 0, 0, 1},
		{"known positive", delegate.Result{Tokens: 4, CostUSD: 1}, 4, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ExecutionUsageCollector{}
			c.observe("backend", delegate.Task{}, tc.r)
			s, err := c.Snapshot()
			if err != nil || s.Tokens != tc.tokens || s.UnpricedTokens != tc.unpriced || s.UnreportedCalls != tc.unreported {
				t.Fatalf("%+v %v", s, err)
			}
		})
	}
	c := &ExecutionUsageCollector{}
	c.observe("backend", delegate.Task{}, delegate.Result{CostUSD: math.NaN()})
	c.observe("backend", delegate.Task{}, delegate.Result{Tokens: 3, CostUSD: 1})
	if _, err := c.Snapshot(); err == nil {
		t.Fatal("invalid observation silently disappeared")
	}
}
func TestExecutionUsageConcurrent(t *testing.T) {
	t.Parallel()
	c := &ExecutionUsageCollector{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 50 {
				c.observe("backend", delegate.Task{}, delegate.Result{Tokens: 1, CostUSD: 1})
				_, _ = c.Snapshot()
			}
		})
	}
	wg.Wait()
	s, err := c.Snapshot()
	if err != nil || s.Tokens != 1000 || s.OtherCostUSD != 1000 {
		t.Fatalf("%+v %v", s, err)
	}
}
