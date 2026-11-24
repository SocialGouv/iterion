package boardops

import "github.com/SocialGouv/iterion/pkg/dispatcher/native"

// write_outcome.go — what a board write's failure says about its effect.
//
// Neither store finishes its write in one step. The fs store writes the
// issue file, then appends the post-commit event; the Mongo store inserts
// the issue, then allocates the event's sequence number and inserts the
// event. A failure in the second half comes back as an error while the card
// already exists, and a lost acknowledgement on the first half does the
// same. So an error from a store WRITE does not say whether the write
// landed — the operations below cannot tell, and neither can a caller.
//
// What they can do is say so, typed: a store write's error leaves here as an
// UndecidedWriteError, which implements runtime.AmbiguousEffect
// STRUCTURALLY (this package must not import the runtime, iterion#2857), so
// the engine's recovery dispatcher refuses to re-run a node whose card may
// already be filed — the guarantee the HTTP connector path gives a mutation
// through exec.Error. Errors raised BEFORE any store write — an argument an
// operation refuses, a capability the grant lacks, an id Resolve cannot
// find — are decided (nothing was written) and stay untyped, so they keep
// their ordinary recovery.
//
// The classification over-approximates on one side, deliberately: a store
// that refuses BEFORE it writes (an unknown state name, a duplicate id)
// reports through the same error return, and the store does not tell the
// two apart. Parking a deterministic refusal costs a resume; retrying a
// committed write files a duplicate card.

// boardOps is the part of native.BoardStore the operations call. Naming it
// keeps the classification total: undecidedWrites implements every method
// EXPLICITLY — no embedding — so an operation that starts calling another
// store method does not compile until that method is classified below as a
// read or a write.
type boardOps interface {
	// Reads: a failure leaves the board as it was.
	Board() (*native.Board, error)
	Resolve(prefix string) (string, error)
	Get(id string) (*native.Issue, error)
	List(filter native.ListFilter) ([]*native.Issue, error)
	AggregateLabels() ([]native.LabelUsage, error)
	// Writes: a failure may or may not have committed.
	Create(in native.Issue) (*native.Issue, error)
	Update(id string, p native.Patch) (*native.Issue, error)
	SetState(id, newState string) (*native.Issue, error)
	AdjustLabels(id string, add, remove []string) (*native.Issue, bool, error)
	AddComment(id, author, body string) (*native.Issue, *native.Comment, error)
	SetGaveUp(id string, g *native.GiveUp) error
}

// UndecidedWriteError is a store write's failure whose effect is unknown:
// the write may have committed before the error surfaced.
type UndecidedWriteError struct {
	// Op is the store method that failed ("Create", "SetState", …).
	Op string
	// Err is the store's own error.
	Err error
}

// Error is the store's own text, unchanged: the stdio and HTTP transports
// relay it to an agent exactly as before.
func (e *UndecidedWriteError) Error() string { return e.Err.Error() }

// Unwrap keeps errors.Is / errors.As reaching the store's error.
func (e *UndecidedWriteError) Unwrap() error { return e.Err }

// AmbiguousEffect marks the failure for runtime.AmbiguousEffect: the write
// may have taken effect, so it must not be repeated blind.
func (e *UndecidedWriteError) AmbiguousEffect() bool { return true }

// undecided types a store write's error; nil stays nil.
func undecided(op string, err error) error {
	if err == nil {
		return nil
	}
	return &UndecidedWriteError{Op: op, Err: err}
}

// undecidedWrites is the store every operation runs against (CallWithEnv
// builds it): reads pass through, writes type their error undecided.
type undecidedWrites struct {
	store native.BoardStore
}

func (w undecidedWrites) Board() (*native.Board, error)         { return w.store.Board() }
func (w undecidedWrites) Resolve(prefix string) (string, error) { return w.store.Resolve(prefix) }
func (w undecidedWrites) Get(id string) (*native.Issue, error)  { return w.store.Get(id) }
func (w undecidedWrites) List(filter native.ListFilter) ([]*native.Issue, error) {
	return w.store.List(filter)
}
func (w undecidedWrites) AggregateLabels() ([]native.LabelUsage, error) {
	return w.store.AggregateLabels()
}

func (w undecidedWrites) Create(in native.Issue) (*native.Issue, error) {
	iss, err := w.store.Create(in)
	return iss, undecided("Create", err)
}

func (w undecidedWrites) Update(id string, p native.Patch) (*native.Issue, error) {
	iss, err := w.store.Update(id, p)
	return iss, undecided("Update", err)
}

func (w undecidedWrites) SetState(id, newState string) (*native.Issue, error) {
	iss, err := w.store.SetState(id, newState)
	return iss, undecided("SetState", err)
}

func (w undecidedWrites) AdjustLabels(id string, add, remove []string) (*native.Issue, bool, error) {
	iss, changed, err := w.store.AdjustLabels(id, add, remove)
	return iss, changed, undecided("AdjustLabels", err)
}

func (w undecidedWrites) AddComment(id, author, body string) (*native.Issue, *native.Comment, error) {
	iss, c, err := w.store.AddComment(id, author, body)
	return iss, c, undecided("AddComment", err)
}

func (w undecidedWrites) SetGaveUp(id string, g *native.GiveUp) error {
	return undecided("SetGaveUp", w.store.SetGaveUp(id, g))
}
