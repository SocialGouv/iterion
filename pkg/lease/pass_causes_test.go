package lease

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
)

// unreachableFarSide is the per-node failure class a handover exists for: a
// resolver that answers "no such host" for a name every peer resolves.
func unreachableFarSide() error {
	return &url.Error{
		Op:  "Get",
		URL: "https://forge.example/api/v4",
		Err: &net.DNSError{Err: "no such host", Name: "forge.example", IsNotFound: true},
	}
}

// refusedByFarSide is the opposite class: an answer that came BACK, so the
// node demonstrably reached what it had to reach.
func refusedByFarSide() error {
	return errors.New("forge: HTTP 422: the install was approved with fewer permissions")
}

// declaringError is a foreign error type that owns the answer for all its
// values — the shape forge.StatusError takes for an upstream 5xx — rather
// than being marked one call at a time by Unreachable.
type declaringError struct {
	cause    error
	declares bool
}

func (e *declaringError) Error() string          { return e.cause.Error() }
func (e *declaringError) Unwrap() error          { return e.cause }
func (e *declaringError) LeaseUnreachable() bool { return e.declares }

// A sweep that must not abort on its first failure hands its consumer every
// cause it met, and the order its query listed the items in is not a fact:
// a page sorted on an expiry, items running in parallel, two tenants. So the
// class of a multi-cause failure is decided by agreement over ALL the causes
// — a cause that names a reach failure decides — and the same set of causes
// reads the same in EVERY order.
//
// Before that rule the consumer read the FIRST cause: a sweep whose refusal
// happened to be listed before its unreachable item answered "the far side
// answered", which is the verdict that CLEARS the run of unreachable passes
// a handover counts (#2685).
func TestTransportFailure_everyCauseOfAMultiCauseFailureDecidesTogether(t *testing.T) {
	cancelled := fmt.Errorf("list connections: %w", context.Canceled)
	for _, tc := range []struct {
		name   string
		causes []error
		want   bool
	}{
		{"one refusal, one unreachable item", []error{refusedByFarSide(), unreachableFarSide()}, true},
		{"the same two, listed the other way", []error{unreachableFarSide(), refusedByFarSide()}, true},
		{"refusals only: the node reached every far side", []error{refusedByFarSide(), refusedByFarSide()}, false},
		{"unreachable items only", []error{unreachableFarSide(), unreachableFarSide()}, true},
		// The term ending reaches an item in flight as the cancellation of
		// its call. That cause is evidence about nothing, and it must not
		// speak for the item that DID meet the egress failing: inherited
		// errors.Is traversal answered "not transport" for the whole
		// aggregate as soon as one cause was a cancellation.
		{"a cancelled item beside an unreachable one", []error{cancelled, unreachableFarSide()}, true},
		{"the unreachable item before the cancelled one", []error{unreachableFarSide(), cancelled}, true},
		{"cancelled items only", []error{cancelled, cancelled}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := errors.Join(tc.causes...)
			if got := TransportFailure(joined); got != tc.want {
				t.Fatalf("TransportFailure(%v) = %v, want %v", joined, got, tc.want)
			}
			// A sweep's aggregate reaches its consumer wrapped in the
			// sweep's own context as often as bare.
			wrapped := fmt.Errorf("forge: refresh %d connections: %w", len(tc.causes), joined)
			if got := TransportFailure(wrapped); got != tc.want {
				t.Fatalf("TransportFailure(wrapped %v) = %v, want %v", wrapped, got, tc.want)
			}
		})
	}
}

// The consequence of that rule, read where it decides: a holder whose sweep
// met one refusal and one unreachable item keeps ACCUMULATING its run of
// unreachable passes and hands the net over, in either order the causes were
// collected in. Under the first-cause rule the same holder cleared its run
// at every pass and kept a net it could not serve for ever.
func TestHealth_aSweepMixingClassesHandsOverInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		causes []error
	}{
		{"the refusal first", []error{refusedByFarSide(), unreachableFarSide()}},
		{"the unreachable item first", []error{unreachableFarSide(), refusedByFarSide()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Health{Passes: 2}
			for pass := 1; pass <= 2; pass++ {
				var p Pass
				// The shape of a worker that already counts: two items
				// attempted, none renewed, and every cause it met.
				p.Attempted(len(tc.causes), 0, errors.Join(tc.causes...))
				if got := p.Verdict(); got != VerdictUnreachable {
					t.Fatalf("pass %d: verdict = %v, want unreachable", pass, got)
				}
				handOver := h.Observe(p.Verdict())
				if want := pass == 2; handOver != want {
					t.Fatalf("pass %d of 2: handOver = %v, want %v (state %+v)", pass, handOver, want, h.Snapshot())
				}
			}
		})
	}
}

// A net whose far side this package cannot type says so itself: the mark
// (Unreachable) for one call, a LeaseUnreachable() method for a type that
// owns the answer for all its values. Both are additive — a declaration is
// only ever read as TRUE, so a type that answers false keeps every typed
// class, and a cause nobody marked is classified exactly as before.
func TestTransportFailure_aNetDeclaresItsOwnFarSide(t *testing.T) {
	refusal := refusedByFarSide()
	cancelledItem := fmt.Errorf("refresh conn-B: %w", context.Canceled)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"an unmarked refusal stays a refusal", refusal, false},
		{"the net marks the call it knows it never completed", Unreachable(refusal), true},
		{"the mark survives the sweep wrapping it", fmt.Errorf("forge: %w", Unreachable(refusal)), true},
		{"a marked item inside an aggregate decides", errors.Join(refusal, Unreachable(refusal)), true},
		{"a type that declares its 5xx", &declaringError{cause: refusal, declares: true}, true},
		{"a type that declares this value is not one", &declaringError{cause: refusal, declares: false}, false},
		// No veto: a type answering false for itself cannot suppress the
		// typed class of the cause it wraps — the shape where each of two
		// guards looks covered and neither is.
		{"a declaration of false is not a veto over the cause it wraps",
			&declaringError{cause: unreachableFarSide(), declares: false}, true},
		// The term ending is not the egress failing, and a mark does not
		// change that: every shutdown under a deadline would read as a
		// handover.
		{"the term's own cancellation stays unmarkable", Unreachable(context.Canceled), false},
		// A sweep hands its caller ONE error for N items, so the mark a net
		// puts on its WHOLE sweep sits above an aggregate. A classifier that
		// answered for the items alone never read that chain: the net said
		// "this node never got there" and the verdict said "the far side
		// answered", which is the answer that CLEARS the run of unreachable
		// passes a handover counts (#2685).
		{"the net marks the whole aggregate its sweep handed back",
			Unreachable(errors.Join(refusal, refusal)), true},
		{"the mark survives the sweep wrapping that aggregate",
			fmt.Errorf("forge: refresh 2 connections: %w", Unreachable(errors.Join(refusal, refusal))), true},
		{"a type that declares a joined cause",
			&declaringError{cause: errors.Join(refusal, refusal), declares: true}, true},
		// Reading the chain above an aggregate stays additive: a declaration
		// of false over one leaves the agreement rule deciding, and cannot
		// hide the unreachable item among the causes.
		{"a declaration of false over an aggregate keeps the agreement rule",
			&declaringError{cause: errors.Join(refusal, refusal), declares: false}, false},
		{"a declaration of false over an aggregate hides no unreachable item",
			&declaringError{cause: errors.Join(refusal, unreachableFarSide()), declares: false}, true},
		// The term's own cancellation answers first, aggregate or not: a
		// pass the term cut short is not a reach failure, whatever the net
		// wrapped around it. EVERY cause, though: one item the term ending
		// cut off is evidence about nothing, exactly as the agreement rule
		// above says, so it cannot answer for the sweep and veto the
		// declaration sitting over it. errors.Is traverses the aggregate as
		// a tree, which is how one cancelled item used to do precisely that
		// — the per-cause mark decided beside a cancelled item while the
		// whole-sweep mark did not.
		{"a mark over an aggregate the term cut short stays unmarkable",
			Unreachable(errors.Join(fmt.Errorf("list connections: %w", context.Canceled), context.Canceled)), false},
		{"one cancelled item does not veto the mark over the whole sweep",
			Unreachable(errors.Join(refusal, cancelledItem)), true},
		{"the same sweep, listed the other way",
			Unreachable(errors.Join(cancelledItem, refusal)), true},
		{"a type declaring an aggregate one item of which was cut off",
			&declaringError{cause: errors.Join(refusal, cancelledItem), declares: true}, true},
		{"the mark over a partly cancelled sweep survives the sweep wrapping it",
			fmt.Errorf("forge: refresh 2 connections: %w", Unreachable(errors.Join(refusal, cancelledItem))), true},
		{"a declaration of false over a partly cancelled sweep keeps the agreement rule",
			&declaringError{cause: errors.Join(refusal, cancelledItem), declares: false}, false},
		{"a declaration of false hides no unreachable item beside a cancelled one",
			&declaringError{cause: errors.Join(unreachableFarSide(), cancelledItem), declares: false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TransportFailure(tc.err); got != tc.want {
				t.Fatalf("TransportFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	if got := Unreachable(nil); got != nil {
		t.Fatalf("Unreachable(nil) = %v, want nil: there is no failure to mark", got)
	}
	// The mark is transparent to the callers that already read the cause:
	// its message is the cause's, and errors.Is finds what it wrapped.
	marked := Unreachable(refusal)
	if marked.Error() != refusal.Error() {
		t.Errorf("marked message = %q, want the cause's %q", marked.Error(), refusal.Error())
	}
	if !errors.Is(marked, refusal) {
		t.Errorf("errors.Is(%v, the cause) = false, want the mark to be transparent", marked)
	}
}

// A net's declaration over its whole sweep reads the same at every DEPTH.
// The exception that lets the term's own cancellation veto it is "every one
// of the causes", and asking errors.Is that question of a cause that is
// itself an aggregate reads it as a tree: one cancelled item would answer for
// the sub-sweep holding it, which is the contamination the per-cause rule
// removes one level up. Wrapping a sweep's items one level deeper must not
// flip whether the net's own declaration is honoured.
func TestTransportFailure_aDeclarationOverASweepReadsTheSameAtEveryDepth(t *testing.T) {
	cancelled := func() error { return fmt.Errorf("list connections: %w", context.Canceled) }
	refusalA, refusalB := refusedByFarSide(), refusedByFarSide()
	for _, tc := range []struct {
		name   string
		flat   []error
		nested []error
		want   bool
	}{
		{
			name:   "refusals and cancelled items under one declaration",
			flat:   []error{cancelled(), refusalA, cancelled(), refusalB},
			nested: []error{errors.Join(cancelled(), refusalA), errors.Join(cancelled(), refusalB)},
			want:   true,
		},
		{
			// Every item the term cut off IS the term ending, however the
			// sweep nested them: no declaration turns that into an egress
			// failure.
			name:   "cancelled items only",
			flat:   []error{cancelled(), cancelled(), cancelled(), cancelled()},
			nested: []error{errors.Join(cancelled(), cancelled()), errors.Join(cancelled(), cancelled())},
			want:   false,
		},
		{
			// A nested unreachable item decides on its own, which the
			// per-cause rule already answers for at any depth.
			name:   "one unreachable item among refusals",
			flat:   []error{cancelled(), refusalA, unreachableFarSide()},
			nested: []error{errors.Join(cancelled(), refusalA), errors.Join(unreachableFarSide())},
			want:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := Unreachable(errors.Join(tc.flat...))
			nested := Unreachable(errors.Join(tc.nested...))
			gotFlat, gotNested := TransportFailure(flat), TransportFailure(nested)
			if gotFlat != tc.want {
				t.Errorf("TransportFailure(flat %v) = %v, want %v", flat, gotFlat, tc.want)
			}
			if gotNested != tc.want {
				t.Errorf("TransportFailure(nested %v) = %v, want %v", nested, gotNested, tc.want)
			}
			if gotFlat != gotNested {
				t.Fatalf("the same facts read %v flat and %v nested: the altitude of the items decided", gotFlat, gotNested)
			}
		})
	}
	// The exception is "every cause", so a sweep of ONE cancelled item is
	// the term ending whichever way it is spelt — the single-cause chain and
	// the join of one answer alike.
	for _, err := range []error{Unreachable(cancelled()), Unreachable(errors.Join(cancelled()))} {
		if TransportFailure(err) {
			t.Errorf("TransportFailure(%v) = true, want false: a sweep whose every item the term cut off is the term ending", err)
		}
	}
}
