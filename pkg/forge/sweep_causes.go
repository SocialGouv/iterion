package forge

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"sync"
)

// sweepCauses collects one failure per ITEM of a sweep that must not abort on
// the first one — a credential refresh, a project-bot rotation — and hands
// them ALL to its caller as one error.
//
// A sweep's caller classifies the cause it is given, and for an elected sweep
// that classification decides whether the replica keeps the term: pkg/lease
// reads the cause's CLASS to tell "this node cannot reach the far side" from
// "the far side answered no". A sweep that kept only the FIRST failure made
// that verdict depend on the order its own query listed the items in — one
// refusal listed before one unreachable item read "the far side answered",
// which CLEARS the run of unreachable passes a handover counts, so the
// sick-but-alive holder kept the net (#2685).
//
// The aggregate is ordered by the ITEM's own key — a connection id, an
// integration id — never by arrival: the same set of failures then reads, and
// logs, identically whatever order the sweep met them in. Two items sharing a
// key would be a caller's bug rather than an order; their causes still sort
// by message, so even then nothing depends on arrival.
//
// Safe for concurrent use, like the lease.Pass it feeds: a sweep whose items
// run in parallel — the order the key discipline above exists for — collects
// from several goroutines.
type sweepCauses struct {
	mu     sync.Mutex
	causes []sweepCause
}

type sweepCause struct {
	key string
	err error
}

// add records one item's failure under the item's own key. A nil err is not a
// failure, so a caller may hand every item's result here unconditionally.
func (c *sweepCauses) add(key string, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.causes = append(c.causes, sweepCause{key: key, err: err})
}

// err answers the sweep's failure: nil when no item failed, that item's error
// verbatim when exactly one did — a wrapper would stand between every reader
// and the error it already reads — and every cause joined in key order when
// several did. errors.Is and errors.As reach each of them, and pkg/lease
// classifies the joined causes by agreement.
func (c *sweepCauses) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch len(c.causes) {
	case 0:
		return nil
	case 1:
		return c.causes[0].err
	}
	ordered := slices.Clone(c.causes)
	slices.SortStableFunc(ordered, func(a, b sweepCause) int {
		return cmp.Or(strings.Compare(a.key, b.key), strings.Compare(a.err.Error(), b.err.Error()))
	})
	errs := make([]error, 0, len(ordered))
	for _, cause := range ordered {
		errs = append(errs, cause.err)
	}
	return errors.Join(errs...)
}
