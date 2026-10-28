package model

import (
	"context"
	"fmt"
	"time"
)

// ErrNodeDeadline is a node ended by the bound its OWN `timeout:` declared,
// named by the one site that knows which deadline fired: the executor that
// froze it into the node's context (see [ClawExecutor.executeBackend]).
//
// It carries the cut in its CHAIN and keeps it out of its TEXT, and both
// halves are load-bearing:
//
//   - In the chain, because what a cut node REPORTS is whatever its backend
//     saw: a sandboxed crossing reports a killed runner or a closed pipe and
//     never a context error, and the in-node retry loop hands up a bare
//     context.DeadlineExceeded when the bound cuts its backoff. Through an
//     Unwrap() []error, so the observed failure stays reachable BESIDE the
//     cut — the engine already walks multi-error chains
//     ([runtime.IsAmbiguousEffect]), so an undecided remote effect and a
//     provider's typed refusal both keep their own classification across
//     this wrap.
//   - Out of the text, because recovery dispatch's last tier matches
//     substrings and "context deadline exceeded" is one of its
//     network-transient needles. Joined into the message, a node that ran
//     out its declared bound is handed to the ~10 min exponential backoff
//     and re-run six times, which silently multiplies the one bound its
//     author did declare. recovery.Classify reads this type BY TYPE, before
//     every needle, exactly as it reads a rejected credential.
//
// A per-request timeout INSIDE the node is deliberately NOT this: it is the
// upstream client's own bound, it says nothing about the node's, and it stays
// the transient fault the dispatcher retries.
type ErrNodeDeadline struct {
	// NodeID is the node whose bound expired.
	NodeID string
	// Timeout is the bound, as the node declared it.
	Timeout time.Duration
	// Observed is what came back when the bound cut the node — the far
	// side's words, the chain's last error, the bare context error of an
	// in-process call. The DETAIL of the failure, never its classification.
	Observed error
}

func (e *ErrNodeDeadline) Error() string {
	msg := fmt.Sprintf("model: node %q ran out the timeout it declared (%s)", e.NodeID, e.Timeout)
	if e.Observed != nil {
		return msg + ": " + e.Observed.Error()
	}
	return msg
}

// Unwrap exposes the observed failure AND the cut, in that order: a reader
// asking what actually happened finds the backend's own typed error first, a
// reader classifying the cut finds context.DeadlineExceeded. Never a nil
// element — the errors package's contract for this shape.
func (e *ErrNodeDeadline) Unwrap() []error {
	if e.Observed == nil {
		return []error{context.DeadlineExceeded}
	}
	return []error{e.Observed, context.DeadlineExceeded}
}
