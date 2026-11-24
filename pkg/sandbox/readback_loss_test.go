package sandbox

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// The reason a drain failure carries is read off the error, never off its
// text: the outermost typed loss of one dir's chain, the agreement across
// a join of dirs, and an honest "unknown" where nothing was typed.
func TestReadBackLossReasonOf(t *testing.T) {
	typed := func(r ReadBackLossReason, msg string) error {
		return &ReadBackLoss{Reason: r, Err: errors.New(msg)}
	}
	for _, tc := range []struct {
		name string
		err  error
		want ReadBackLossReason
	}{
		{"nil carries no reason", nil, ""},
		{"an untyped failure is unknown", errors.New("kubectl exec failed"), ReadBackUnknown},
		{"a typed loss is its reason", typed(ReadBackOverCap, "over"), ReadBackOverCap},
		{"a wrap above the loss keeps it readable", fmt.Errorf("kubernetes: drain /x: %w", typed(ReadBackRefused, "pre-drain")), ReadBackRefused},
		{"the outermost typed loss of a chain is the reason", typed(ReadBackUnsettled, "kept changing: "+typed(ReadBackArchiver, "tar exit 1").Error()), ReadBackUnsettled},
		{"a chain typed twice reads the outer one, not the inner", &ReadBackLoss{Reason: ReadBackUnsettled, Err: fmt.Errorf("after 3 attempts: %w", typed(ReadBackArchiver, "tar exit 1"))}, ReadBackUnsettled},
		{"a join of dirs lost alike agrees", errors.Join(typed(ReadBackOverCap, "a"), typed(ReadBackOverCap, "b")), ReadBackOverCap},
		{"a join of dirs lost for different reasons is mixed", errors.Join(typed(ReadBackOverCap, "a"), typed(ReadBackRefused, "b")), ReadBackMixed},
		{"a join with an untyped branch is mixed, not guessed", errors.Join(typed(ReadBackArchiver, "a"), errors.New("b")), ReadBackMixed},
		{"a join of one is that one", errors.Join(typed(ReadBackArchiver, "a")), ReadBackArchiver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadBackLossReasonOf(tc.err); got != tc.want {
				t.Fatalf("ReadBackLossReasonOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// The typed loss stays an ordinary error: its text is the cause's and
// errors.Is reaches through it.
func TestReadBackLossIsATransparentWrap(t *testing.T) {
	cause := errors.New("the host dir could not be written")
	loss := &ReadBackLoss{Reason: ReadBackRefused, Err: cause}
	if loss.Error() != cause.Error() {
		t.Errorf("Error() = %q, want the cause's own text %q", loss.Error(), cause.Error())
	}
	if !errors.Is(fmt.Errorf("drain: %w", loss), cause) {
		t.Error("errors.Is does not reach the cause through the typed loss")
	}
}

// The declined members travel on the loss of the dir that declined them and
// are read back across every branch, in one order whatever order the dirs
// drained or the archives carried them in — the order the event lists, and
// the order its bound keeps the first of.
func TestReadBackDeclinedOf(t *testing.T) {
	a := ReadBackDeclinedMember{Dir: "/a", Name: "z.html", Why: DeclinedHardlinkTarget}
	b := ReadBackDeclinedMember{Dir: "/a", Name: "b.fifo", Why: DeclinedKind}
	c := ReadBackDeclinedMember{Dir: "/b", Name: "a.lnk", Why: DeclinedLinkLeavesTree}
	for _, tc := range []struct {
		name string
		err  error
		want []ReadBackDeclinedMember
	}{
		{"nil declines nothing", nil, nil},
		{"an untyped failure declines nothing", errors.New("kubectl exec failed"), nil},
		{"one dir's members, ordered by name", &ReadBackLoss{Reason: ReadBackDeclined, Err: errors.New("x"), Declined: []ReadBackDeclinedMember{a, b}}, []ReadBackDeclinedMember{b, a}},
		{"a wrap above the loss keeps them readable", fmt.Errorf("drain: %w", &ReadBackLoss{Reason: ReadBackOverCap, Err: errors.New("x"), Declined: []ReadBackDeclinedMember{a}}), []ReadBackDeclinedMember{a}},
		{"every branch of a join, ordered by dir then name", errors.Join(
			&ReadBackLoss{Reason: ReadBackDeclined, Err: errors.New("b"), Declined: []ReadBackDeclinedMember{c}},
			&ReadBackLoss{Reason: ReadBackArchiver, Err: errors.New("a"), Declined: []ReadBackDeclinedMember{a, b}},
		), []ReadBackDeclinedMember{b, a, c}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadBackDeclinedOf(tc.err); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReadBackDeclinedOf(%v) = %+v, want %+v", tc.err, got, tc.want)
			}
		})
	}
}

// TestReadBackDeclinedSetIsBounded: an archive is the pod's to fill, so the
// set a drain keeps of what it declined is bounded however many arrive —
// the count is every one, the list the first ReadBackDeclinedListed in dir,
// name and why order, the same whichever order they arrived in, and what
// the set holds never grows past twice the bound.
func TestReadBackDeclinedSetIsBounded(t *testing.T) {
	const n = 10 * ReadBackDeclinedListed
	member := func(i int) ReadBackDeclinedMember {
		return ReadBackDeclinedMember{Dir: "/d", Name: fmt.Sprintf("fifo-%05d", i), Why: DeclinedKind}
	}
	want := make([]ReadBackDeclinedMember, 0, ReadBackDeclinedListed)
	for i := 1; i <= ReadBackDeclinedListed; i++ {
		want = append(want, member(i))
	}
	for _, order := range []struct {
		name string
		at   func(k int) int
	}{
		{"ascending", func(k int) int { return k + 1 }},
		{"descending", func(k int) int { return n - k }},
		{"interleaved", func(k int) int { return (k*7919)%n + 1 }},
	} {
		t.Run(order.name, func(t *testing.T) {
			var s ReadBackDeclinedSet
			for k := 0; k < n; k++ {
				s.Add(member(order.at(k)))
				if len(s.kept) > 2*ReadBackDeclinedListed {
					t.Fatalf("after %d members the set holds %d, want at most %d — the bound is the memory's too", k+1, len(s.kept), 2*ReadBackDeclinedListed)
				}
			}
			if s.Count() != n {
				t.Errorf("Count = %d, want every member counted (%d)", s.Count(), n)
			}
			if got := s.List(); !reflect.DeepEqual(got, want) {
				t.Errorf("List = %d members from %v, want the first %d in name order", len(got), got[:min(3, len(got))], ReadBackDeclinedListed)
			}
		})
	}
	t.Run("a set under the bound lists every member, in order", func(t *testing.T) {
		var s ReadBackDeclinedSet
		s.Add(member(2))
		s.Add(member(1))
		if got, want := s.List(), []ReadBackDeclinedMember{member(1), member(2)}; !reflect.DeepEqual(got, want) || s.Count() != 2 {
			t.Errorf("List = %+v (count %d), want %+v", got, s.Count(), want)
		}
	})
}

// TestReadBackDeclinedCountOf: the count is the loss's — every member the
// host declined, named or not — summed across the branches of a join; a
// loss built without a count counts what it names.
func TestReadBackDeclinedCountOf(t *testing.T) {
	named := []ReadBackDeclinedMember{{Dir: "/a", Name: "x", Why: DeclinedKind}, {Dir: "/a", Name: "y", Why: DeclinedKind}}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"nil counts nothing", nil, 0},
		{"an untyped failure counts nothing", errors.New("kubectl exec failed"), 0},
		{"the loss's count, past what it names", &ReadBackLoss{Reason: ReadBackDeclined, Err: errors.New("x"), Declined: named, DeclinedCount: 5000}, 5000},
		{"a loss without a count counts what it names", &ReadBackLoss{Reason: ReadBackDeclined, Err: errors.New("x"), Declined: named}, 2},
		{"every branch of a join", errors.Join(
			&ReadBackLoss{Reason: ReadBackDeclined, Err: errors.New("a"), Declined: named, DeclinedCount: 300},
			fmt.Errorf("drain: %w", &ReadBackLoss{Reason: ReadBackArchiver, Err: errors.New("b"), Declined: named}),
		), 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadBackDeclinedCountOf(tc.err); got != tc.want {
				t.Fatalf("ReadBackDeclinedCountOf(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// The stale mark is read off the loss too: one stale branch makes the run's
// set one that may carry members its pod no longer has.
func TestReadBackStaleOf(t *testing.T) {
	stale := &ReadBackLoss{Reason: ReadBackOverCap, Err: errors.New("over"), Stale: true}
	clean := &ReadBackLoss{Reason: ReadBackOverCap, Err: errors.New("over")}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not stale", nil, false},
		{"an untyped failure is not marked", errors.New("kubectl exec failed"), false},
		{"a loss without the mark", clean, false},
		{"a marked loss", stale, true},
		{"a wrap above the mark keeps it readable", fmt.Errorf("drain: %w", stale), true},
		{"one stale branch of a join, last", errors.Join(clean, stale), true},
		{"one stale branch of a join, first", errors.Join(stale, clean), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadBackStaleOf(tc.err); got != tc.want {
				t.Fatalf("ReadBackStaleOf(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
