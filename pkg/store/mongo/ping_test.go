package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"
)

// scriptedPinger answers the scripted errors in order, then succeeds.
type scriptedPinger struct {
	errs  []error
	calls int
}

func (s *scriptedPinger) Ping(_ context.Context, _ *readpref.ReadPref) error {
	i := s.calls
	s.calls++
	if i < len(s.errs) {
		return s.errs[i]
	}
	return nil
}

// stuckPinger blocks until its attempt context expires: a host whose
// handshake never completes within one attempt.
type stuckPinger struct{ calls int }

func (s *stuckPinger) Ping(ctx context.Context, _ *readpref.ReadPref) error {
	s.calls++
	<-ctx.Done()
	return fmt.Errorf("server selection error: %w", ctx.Err())
}

var fastPolicy = pingPolicy{attempt: 20 * time.Millisecond, pause: time.Millisecond, budget: 300 * time.Millisecond}

// A late server selection is retried until the primary answers — the
// shape that ejected PR #698 from the merge queue: "rs0 primary elected"
// printed, then the 5th fresh client saw ReplicaSetNoPrimary.
func TestPingPrimaryRetriesALateServerSelection(t *testing.T) {
	p := &scriptedPinger{errs: []error{
		fmt.Errorf("server selection error: %w", context.DeadlineExceeded),
		topology.ServerSelectionError{Wrapped: errors.New("current topology: { Type: ReplicaSetNoPrimary }")},
	}}
	if err := pingPrimary(context.Background(), p, fastPolicy); err != nil {
		t.Fatalf("pingPrimary: %v", err)
	}
	if p.calls != 3 {
		t.Fatalf("calls = %d, want 3 (two retried failures, then success)", p.calls)
	}
}

// A failure no retry can cure is returned from the first attempt: an
// operator with wrong credentials must not wait out the budget to learn
// it.
func TestPingPrimaryFailsFastOnANonRetryableError(t *testing.T) {
	auth := errors.New("(AuthenticationFailed) Authentication failed.")
	p := &scriptedPinger{errs: []error{auth, auth, auth}}
	err := pingPrimary(context.Background(), p, fastPolicy)
	if !errors.Is(err, auth) {
		t.Fatalf("err = %v, want the auth failure", err)
	}
	if p.calls != 1 {
		t.Fatalf("calls = %d, want 1", p.calls)
	}
	if strings.Contains(err.Error(), "attempt") {
		t.Fatalf("a first-attempt refusal must not read as an exhausted retry: %v", err)
	}
}

// Without a caller deadline the policy budget bounds the retries, and
// the error names the attempts so an absent primary is not mistaken for
// a hang.
func TestPingPrimaryIsBoundedByTheBudgetWhenTheContextHasNoDeadline(t *testing.T) {
	p := &stuckPinger{}
	start := time.Now()
	err := pingPrimary(context.Background(), p, fastPolicy)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error after the budget")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the last attempt's deadline in the chain", err)
	}
	if !strings.Contains(err.Error(), "no primary after") || !strings.Contains(err.Error(), "attempt(s)") {
		t.Fatalf("err = %v, want the attempt count", err)
	}
	if p.calls < 2 {
		t.Fatalf("calls = %d, want at least 2 attempts within the budget", p.calls)
	}
	if elapsed > 3*fastPolicy.budget {
		t.Fatalf("retries ran %v past a %v budget", elapsed, fastPolicy.budget)
	}
}

// A caller's own deadline wins over the policy budget: the conformance
// factory's 15 s and a boot context both end the retries where they say.
func TestPingPrimaryHonoursTheCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	p := &stuckPinger{}
	pol := fastPolicy
	pol.budget = 10 * time.Second
	start := time.Now()
	err := pingPrimary(ctx, p, pol)
	if err == nil {
		t.Fatal("expected an error at the caller deadline")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("retries ran %v past a 60ms caller deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline ") {
		t.Fatalf("err = %v, want the caller deadline named", err)
	}
}

// A cancelled caller is named as such, not reported as a missing primary.
func TestPingPrimaryNamesACancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &scriptedPinger{errs: []error{fmt.Errorf("server selection error: %w", context.DeadlineExceeded)}}
	cancel()
	err := pingPrimary(ctx, p, fastPolicy)
	if err == nil || !strings.Contains(err.Error(), "context cancelled") {
		t.Fatalf("err = %v, want the cancellation named", err)
	}
}
