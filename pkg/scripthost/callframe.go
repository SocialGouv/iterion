package scripthost

import (
	"context"
	"encoding/json"
)

// The call frame rides the context of a NESTED tool call, beside the result
// budget ([WithScriptResultBudget]) and for the same reason: the parent's pump
// is the one party that knows where a call sits in its program, and the
// server that dispatches it receives a context, not a frame.
//
// What it carries is program order, which nothing downstream of the pump can
// reconstruct: a batch's workers finish in whatever order the scheduler picks,
// so a server that numbered calls as they arrived would number them in
// goroutine order. The frame ID is the child's own — minted strictly
// increasing from 1 in the order the program issued its requests, state
// requests included — so a reader that sorts by it reads the program.
//
// Armed per call, on the worker context [dispatchBatch] derives for that call:
// two calls of one batch each carry their own frame, and no worker shares a
// mutable.
type ctxKeyCallFrame struct{}

// CallFrame is where one nested call sits in its program.
type CallFrame struct {
	// ID is the frame ID the child minted for the call.
	ID int64
	// Batch is the call's slot in the `call.parallel` batch it came in; nil
	// for a single `call()`.
	Batch *BatchSlot
}

// BatchSlot is one entry's place in a `call.parallel` batch. The JSON names
// are the ones a run's events carry.
type BatchSlot struct {
	// First is the frame ID of the batch's first entry: the batch's identity
	// within its program, shared by every entry of it.
	First int64 `json:"first"`
	// Size is how many entries the program submitted, the entries the parent
	// refused on the call budget included.
	Size int `json:"size"`
	// Index is the entry's 0-based position in the batch as submitted.
	Index int `json:"index"`
}

// WithCallFrame pins a nested call's frame on the context its server receives.
func WithCallFrame(ctx context.Context, frame CallFrame) context.Context {
	return context.WithValue(ctx, ctxKeyCallFrame{}, frame)
}

// CallFrameFrom reads the pinned frame. The second return is false when the
// context did not come from the pump — a server called outside a program.
func CallFrameFrom(ctx context.Context) (CallFrame, bool) {
	frame, ok := ctx.Value(ctxKeyCallFrame{}).(CallFrame)
	return frame, ok
}

// callGroup is one request read off the tool channel: a single call, or a
// batch. It turns an entry's position into the frame its server receives.
type callGroup struct {
	batch bool
	first int64
	size  int
}

func (g callGroup) frame(i int, id int64) CallFrame {
	f := CallFrame{ID: id}
	if g.batch {
		f.Batch = &BatchSlot{First: g.first, Size: g.size, Index: i}
	}
	return f
}

// BudgetRefusalObserver is the optional half of a [ToolServer] that is told
// about each call the PARENT refused on the call budget, with the frame the
// call would have been served under. Such a call is never dispatched, so
// without this its program's call is visible to nobody but the program.
//
// Calls the child refuses itself, once it has latched on a spent budget,
// never reach the parent and are not reported: the child is what is being
// bounded, and its latch is a round-trip saving, not a verdict.
type BudgetRefusalObserver interface {
	RefusedOnBudget(frame CallFrame, name string, input json.RawMessage, text string)
}

// CallStats is the parent's account of one program's tool channel.
type CallStats struct {
	// Served is how many requests the parent admitted against the call
	// budget: tool calls and run-state requests alike, since both spend it.
	Served int64
	// BudgetRefused is how many requests the parent refused because the
	// budget was spent: batch entries past the admissible prefix, and
	// run-state requests past the bound.
	BudgetRefused int64
	// StateCalls is how many of the served requests were `call.store` or
	// `call.load`.
	StateCalls int64
}
