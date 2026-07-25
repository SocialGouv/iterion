package native

// BoardStore is the storage contract the board operations (boardops), the
// dispatcher tracker adapter, and the REST handlers operate against. The
// filesystem-backed *Store satisfies it; a cloud build can supply a
// Mongo-backed implementation of the SAME contract so the shared boardops and
// dispatcher run unchanged against either backend.
//
// The board domain types (Issue, Patch, ListFilter, Board, Event, LabelUsage)
// live in this package; a non-filesystem implementation imports them from
// here. They are plain JSON/BSON-friendly structs with no filesystem
// coupling, so this is types-only reuse, not behaviour.
type BoardStore interface {
	Board() *Board
	SetBoard(b *Board) error

	Create(in Issue) (*Issue, error)
	Get(id string) (*Issue, error)
	List(filter ListFilter) ([]*Issue, error)
	Update(id string, p Patch) (*Issue, error)
	SetState(id, newState string) (*Issue, error)
	Delete(id string) error

	// Claim/Release are the dispatcher's per-issue lease (marker = the
	// dispatcher instance id). SetLastRun records the run a dispatch spawned
	// so a cross-restart resume can find it.
	Claim(id, marker string) error
	Release(id, marker string) error
	SetLastRun(id, runID, workdir string) error
	// SetAwaitingInput denormalizes onto the issue whether its most recent
	// run parked awaiting human/operator input, so the board grid can badge
	// the card without a per-run fetch. A best-effort HINT (see Issue.AwaitingInput).
	SetAwaitingInput(id string, v bool) error

	// AddComment appends a note to the issue's discussion thread and
	// returns the updated issue plus the created comment.
	AddComment(id, author, body string) (*Issue, *Comment, error)

	Resolve(prefix string) (string, error)
	ScanEvents(visit func(*Event) bool) error
	AggregateLabels() []LabelUsage
}

// UniqueTitleCreator is the optional interface for board backends that can
// assign a distinct card title atomically with the create write (so two
// concurrent creates of the same title cannot both land it — PR #193 M4).
// The filesystem store implements it; a backend that does not cleanly
// degrades to the caller's best-effort list-then-check.
type UniqueTitleCreator interface {
	// normalize, when non-nil, is applied to every candidate title inside the
	// critical section — including the "#N - " prefixed variants. It lets a
	// caller keep the result within a display budget: prefixing "#N - " onto an
	// already-max-length title would otherwise overflow it, so the caller passes
	// its own rune-aware truncator (the prefix survives because truncation keeps
	// the head). nil = titles are used verbatim.
	CreateUniqueTitle(in Issue, normalize func(string) string) (*Issue, error)
}

// AsUniqueTitleCreator returns s as UniqueTitleCreator when the backend
// supports the atomic-unique-title create, or nil otherwise.
func AsUniqueTitleCreator(s BoardStore) UniqueTitleCreator {
	if s == nil {
		return nil
	}
	u, _ := s.(UniqueTitleCreator)
	return u
}

// LaunchClaimer is the optional interface for board backends that can
// atomically claim a Ready ticket for launch — a CAS StateReady →
// StateInProgress that reports whether THIS caller won (PR #193 M2). It
// closes the check-then-act window where a live dispatcher and the studio
// admission loop both pick the same Ready ticket. The filesystem store
// implements it; a backend that does not cleanly degrades to the caller's
// best-effort SetState (the documented V1 window).
type LaunchClaimer interface {
	ClaimForLaunch(id string) (*Issue, bool, error)
}

// AsLaunchClaimer returns s as LaunchClaimer when the backend supports the
// atomic launch claim, or nil otherwise.
func AsLaunchClaimer(s BoardStore) LaunchClaimer {
	if s == nil {
		return nil
	}
	c, _ := s.(LaunchClaimer)
	return c
}

// Compile-time assertion that the filesystem store satisfies the contract.
var _ BoardStore = (*Store)(nil)
var _ UniqueTitleCreator = (*Store)(nil)
var _ LaunchClaimer = (*Store)(nil)

// Compile-time guarantees: the filesystem store is a full board backend.
var (
	_ BoardStore = (*Store)(nil)
	_ BoardAdmin = (*Store)(nil)
)
