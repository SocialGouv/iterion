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
