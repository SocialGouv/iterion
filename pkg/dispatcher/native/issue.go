package native

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// Issue is the native tracker's source-of-truth issue record. The
// dispatcher consumes a normalized view via tracker.Issue (see
// pkg/dispatcher/tracker/native.go for the conversion).
type Issue struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Body     string   `json:"body,omitempty"`
	State    string   `json:"state"`
	Labels   []string `json:"labels,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Assignee string   `json:"assignee,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
	// ParentID is the planner (or prior planner) ticket that spawned this
	// one. Distinct from Blockers (scheduling deps): ParentID is provenance
	// / campaign ownership so the /pipelines UI can nest children under a
	// plan. Empty for root tickets. Stamped automatically by board.create
	// when the creating run is sourced from a ticket, or set explicitly via
	// create_issue parent_id / bot_args.spawned_from.
	ParentID string         `json:"parent_id,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
	// Bot, when non-empty, overrides the dispatcher's per-assignee /
	// global workflow selection for this ticket. The dispatcher
	// resolves the name to a workflow file via pkg/botregistry.
	Bot string `json:"bot,omitempty"`
	// BotArgs are per-ticket overrides merged on top of the
	// dispatcher config's templated vars at launch time (key-by-key:
	// BotArgs wins for declared keys, config templates fill the rest).
	// Values are stored as strings so the engine's existing var-coercion
	// pipeline applies — same wire format as the studio's Launch form.
	BotArgs map[string]string `json:"bot_args,omitempty"`
	Claim   string            `json:"claim,omitempty"`
	// ClaimEpoch is the per-issue fencing counter: bumped on every FRESH
	// claim acquisition (never on an idempotent same-marker re-claim), it
	// travels in the tracker.ClaimToken and every owner-scoped write is a
	// CAS on (Claim, ClaimEpoch) — a stolen claim's late writes find
	// typed refusals, never the new owner's state.
	ClaimEpoch int64 `json:"claim_epoch,omitempty"`
	// ClaimedAt is when the CURRENT claim was acquired (audit; zero on a
	// legacy claim written before the lease existed — such claims are
	// never expired by time, only by the historical pid-probe sweep).
	ClaimedAt time.Time `json:"claimed_at,omitempty"`
	// ClaimLeaseUntil is the single instant the claim's lease expires —
	// stamped at claim and pushed forward by each RenewClaim heartbeat
	// (one field, not a claimed-at/renewed-at max: it is what the reaper
	// queries and what an index can serve). On Mongo it is written with
	// the server clock so a pod with a fast clock cannot steal a live
	// claim. Zero = legacy claim, see ClaimedAt.
	ClaimLeaseUntil time.Time `json:"claim_lease_until,omitempty"`
	// LastRunID is the most recent dispatcher-spawned run that
	// processed this issue. Stamped by the dispatcher's finishRun
	// regardless of success/failure so the operator can always
	// pivot from the kanban card to the run console / diff inspector.
	LastRunID string `json:"last_run_id,omitempty"`
	// AwaitingInput is a denormalized best-effort HINT that the issue's
	// most recent dispatcher-spawned run parked on a human/operator gate
	// and is waiting for an answer. It lets the studio render a per-card
	// "⏸ Awaiting input" badge on the board grid WITHOUT an N+1 run
	// fetch. The dispatcher sets it true when a run parks on pause and
	// clears it on the paths it controls (clean terminal finish,
	// re-dispatch). It is NOT authoritative — the IssueModal's answer
	// affordance still keys off getRun(last_run_id).status; a stale flag
	// (e.g. after a console-only resume the dispatcher never observed) is
	// corrected at the next card touch.
	AwaitingInput bool `json:"awaiting_input,omitempty"`
	// GaveUp records that the ticket's CURRENT state was written by the
	// dispatcher GIVING UP — its retry budget ran out and it filed the ticket
	// in Agent.FailedState (default "blocked") on its own. That state is
	// usually terminal, and a terminal ticket otherwise reads as "a human
	// already filed this"; without this stamp the two are indistinguishable
	// and the pipeline board buries an automatic give-up in Closed instead of
	// surfacing it in Needs attention (issue #494).
	//
	// The stamp is SELF-INVALIDATING rather than something every writer must
	// remember to clear: it records the run it happened on and the state it
	// wrote, so a later run or any move of the ticket makes it stale by
	// construction (see GiveUp.Current). The one case that needs an explicit
	// clear is an operator filing the ticket into the state it is already in.
	GaveUp *GiveUp `json:"gave_up,omitempty"`
	// LaunchRefusal is the cloud dispatcher's record of a claimed card whose
	// launch the run service REFUSED before any run started (a sealing
	// failure, a queue outage, a bot that does not compile, …): nothing ran,
	// so no verdict belongs on the card and it went back to its column. It
	// is what bounds the retry — the dispatch listing skips the card until
	// NotBefore, and the attempt cap turns a permanent refusal into a
	// `blocked` filing the operator can read (LastReason). Cleared when a
	// launch succeeds (SetLastRun) and when an operator reopens the card.
	LaunchRefusal *LaunchRefusal `json:"launch_refusal,omitempty"`
	// LastWorkdir is the absolute filesystem path the last run
	// executed in — either the per-issue dispatcher workspace or,
	// when `worktree: auto` was used, the run's git worktree path.
	// The studio exposes it as a copy-to-clipboard / vscode://file
	// link so the operator can inspect the diff manually.
	LastWorkdir string `json:"last_workdir,omitempty"`
	// Runs is the append-only history of dispatcher-spawned runs that
	// processed this issue, newest-last. LastRunID/LastWorkdir remain the
	// single overwritten pointer to the most-recent run for back-compat;
	// Runs is the full history the studio renders as a list. Deduped by
	// RunID (see AppendRunRef). Absent on records written before T4a.
	Runs      []RunRef  `json:"runs,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// StateAt is when the card's COLUMN last changed — the transition time,
	// not the record's. UpdatedAt bumps on any edit (a title, a label, a
	// last-run stamp), so anything asking "which side moved more recently?"
	// reads a retitle as a move; the two-way project-board sync's conflict
	// rule is exactly that question (ADR-097).
	//
	// Stamped by the store at every state write, on both twins: the FS one
	// derives it in writeIssueLocked (the state differs from the indexed
	// record), the Mongo one in stateSetAt + the state-naming replace. A
	// caller cannot forget it, and cannot forge it either.
	//
	// Zero on a card written before the field existed — legacy, exactly like
	// ClaimedAt. A reader wanting a transition time for such a card has to
	// say what it falls back to.
	StateAt time.Time `json:"state_at,omitempty"`
	// StateReason is the provenance of the card's LAST column change — the
	// `reason` its state event carried (a tracker.Reason* constant), empty
	// for an unattributed write (an operator surface, a bot's board tool,
	// the project import, a create). It is what lets a reader of the CARD
	// alone — the periodic project-board reflect, which sees no event —
	// tell a column iterion wrote on its own authority (StateByMachine)
	// from one a person, or a run's verdict, put the card in.
	//
	// Stamped by the store at every state write, on both twins, from the
	// same inputs that build the event's `reason`: it cannot disagree with
	// the event, and it is overwritten (or cleared) by the next transition,
	// so it never describes a column the card has left.
	StateReason string `json:"state_reason,omitempty"`
	// External links this card to an issue on an external forge — set when
	// the card is mirrored FROM a forge (one-way forge→board sync) or pushed
	// TO one (push-to-forge). It is metadata: the card's column stays
	// operator-owned. Repo doubles as the board swimlane key (repo-per-lane).
	External *ExternalRef `json:"external,omitempty"`
	// Comments is the append-only discussion thread on the issue. Used
	// by hooks / the dispatcher to leave a dispatch trail, by the studio
	// IssueModal, and — once the comment-trigger wiring lands — to carry
	// operator `/command` requests and the resulting MR/PR back-links.
	Comments []Comment `json:"comments,omitempty"`
}

// StateByMachine reports whether the card's current column was written by
// iterion acting on its own authority — a watchdog repair, a schema
// migration, the dispatcher returning a card it could not launch — rather
// than by a person or by a run's verdict. The predicate is the enumerated
// tracker.IsMachineReason over the persisted provenance, the same contract
// the trigger spine applies to the event: a descriptive reason (unblocked,
// run_finished) reads as a gesture here too.
func (i *Issue) StateByMachine() bool {
	return i != nil && tracker.IsMachineReason(i.StateReason)
}

// RunRef is one entry in an issue's run history (Issue.Runs). RunID is
// the dispatcher-spawned run id; Workdir is the absolute path it executed
// in (per-issue workspace or a `worktree: auto` git worktree); At is when
// the run was stamped onto the card.
type RunRef struct {
	RunID   string    `json:"run_id"`
	Workdir string    `json:"workdir,omitempty"`
	At      time.Time `json:"at"`
}

// AppendRunRef dedup-appends a run onto an issue's history keyed on RunID:
// if runID is already present its Workdir/At are updated in place; otherwise
// a new RunRef is appended (newest-last). Shared by both the native and
// boardmongo SetLastRun implementations so the append semantics stay
// identical across stores. Growth is uncapped by design.
func AppendRunRef(runs []RunRef, runID, workdir string, at time.Time) []RunRef {
	// SetLastRun(id, "", "") is the documented way to CLEAR the pointer, and
	// a cleared pointer is not a run that happened — appending it would put a
	// blank entry in the card's run history.
	if runID == "" {
		return runs
	}
	for i := range runs {
		if runs[i].RunID == runID {
			runs[i].Workdir = workdir
			runs[i].At = at
			return runs
		}
	}
	return append(runs, RunRef{RunID: runID, Workdir: workdir, At: at})
}

// GiveUp is the give-up stamp on an Issue: the dispatcher exhausted
// Agent.MaxAttempts on RunID and moved the ticket to State by itself. It is
// the "who filed this ticket" answer a bare terminal state cannot give.
type GiveUp struct {
	// RunID is the run whose failure exhausted the budget.
	RunID string `json:"run_id,omitempty"`
	// State is the state the give-up wrote. Kept so the stamp expires on its
	// own: once the ticket sits anywhere else, someone decided something
	// after the give-up and the stamp no longer describes the card.
	State string `json:"state,omitempty"`
	// Attempts is how many attempts were made before giving up.
	Attempts int `json:"attempts,omitempty"`
	// Reason, when set, says WHY the dispatcher gave up when it was not a
	// retry budget: the claim watchdog filing a card whose recorded run
	// is gone. Operator-facing (rendered on the pipeline board); a
	// retry-budget give-up leaves it empty and reads by Attempts.
	Reason string `json:"reason,omitempty"`
	// At is when the give-up was stamped (UTC).
	At time.Time `json:"at"`
}

// LaunchRefusal is the retry ledger of a card whose launch keeps being
// refused before a run exists (see Issue.LaunchRefusal). Attempts counts
// the consecutive refusals; NotBefore is the earliest instant the dispatch
// tick may claim the card again; LastAt / LastReason say when and why, for
// the operator who finds the card filed blocked once the attempts run out.
type LaunchRefusal struct {
	Attempts   int       `json:"attempts"`
	LastAt     time.Time `json:"last_at"`
	NotBefore  time.Time `json:"not_before,omitempty"`
	LastReason string    `json:"last_reason,omitempty"`
}

// Clone returns a copy, nil-safe.
func (r *LaunchRefusal) Clone() *LaunchRefusal {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// Current reports whether the stamp still describes the issue as it stands:
// the ticket has not moved since the give-up, and the run being examined is
// the one that was given up on. Anything else — a retry, an operator move, a
// newer run — makes the stamp history rather than a live signal, with no
// writer having to remember to clear it.
//
// A stamp with no run id never matches: it cannot be attributed to the card
// being rendered, and guessing is how the lane got confusing in the first place.
func (g *GiveUp) Current(issueState, runID string) bool {
	if g == nil || g.RunID == "" {
		return false
	}
	return g.State == issueState && g.RunID == runID
}

// ExternalRef links a board card to an issue on an external forge. Set by
// the forge→board sync worker and the push-to-forge action; read by the card
// PR/CI panel and push handler. Provider is "github"|"gitlab"|"forgejo".
type ExternalRef struct {
	Provider     string `json:"provider"`
	ConnectionID string `json:"connection_id"`
	Repo         string `json:"repo"`
	Number       int    `json:"number"`
	URL          string `json:"url,omitempty"`
	State        string `json:"state,omitempty"`
	// Author is the forge login that opened the external issue — the identity
	// the author-trust gate classified at ingest, kept so operators can see
	// WHO requested a parked card before approving its triage.
	Author string `json:"author,omitempty"`
	// Project is the card's sync state with the forge's PROJECT board (GitHub
	// Projects v2), when the team is bound to one. Nil until a project import
	// has seen this card on the board.
	Project *ExternalProject `json:"project,omitempty"`
}

// Clone returns a deep copy. ExternalRef is passed by value at several store
// boundaries; since it now carries a pointer, a plain `*ref` copy would alias
// the project sync state between the caller's value and the stored record.
func (e *ExternalRef) Clone() *ExternalRef {
	if e == nil {
		return nil
	}
	out := *e
	if e.Project != nil {
		p := *e.Project
		out.Project = &p
	}
	return &out
}

// ExternalProject is a card's sync state with ONE forge project board. It is
// the per-card half of the two-way status sync (ADR-097); the board itself is
// identified by the team's binding.
//
// The two timestamps are what make "both sides moved" decidable. They record
// STATE CHANGES, not record touches: Issue.UpdatedAt bumps on any edit, so
// comparing it against the board would let an unrelated title edit win a
// status conflict.
type ExternalProject struct {
	// Owner + Number identify the board (its "owner/number" URL form).
	Owner  string `json:"owner,omitempty"`
	Number int    `json:"number,omitempty"`
	// ItemID is the provider's project-item handle, so a status write skips a
	// lookup. Re-resolved when the board no longer knows it.
	ItemID string `json:"item_id,omitempty"`
	// Status is the board status option NAME last synchronized, in the
	// board's own vocabulary ("In progress"). Comparing against it is the
	// ECHO SUPPRESSOR: a status iterion itself wrote reads back as equal and
	// changes nothing.
	Status string `json:"status,omitempty"`
	// StatusAt is the provider's own timestamp for that status value.
	StatusAt time.Time `json:"status_at,omitempty"`
	// StateAt is when iterion last WROTE the native state for this board —
	// the moment of a sync write, not of the card's transition. The conflict
	// rule reads the card's own Issue.StateAt; this stays as the fallback for
	// a card whose last transition predates that stamp.
	StateAt time.Time `json:"state_at,omitempty"`
}

// Equal reports whether two sync states carry the same information.
//
// It lives here, next to the struct, because it is what a periodic writer
// checks before deciding to write at all: a card rewritten when nothing
// changed bumps UpdatedAt and emits an EvtIssueUpdated the trigger spine
// consumes as `card.updated`, which relaunches label-matching board
// subscriptions. Keeping the comparison beside the fields is what stops it
// silently going stale the day a field is added.
//
// The timestamps compare with Equal, never ==: a time.Time carries a monotonic
// reading and a location, so two values denoting the same instant are routinely
// unequal under ==.
func (p *ExternalProject) Equal(o ExternalProject) bool {
	if p == nil {
		return false
	}
	return p.Owner == o.Owner &&
		p.Number == o.Number &&
		p.ItemID == o.ItemID &&
		p.Status == o.Status &&
		p.StatusAt.Equal(o.StatusAt) &&
		p.StateAt.Equal(o.StateAt)
}

// Comment is a single append-only note on a native issue. Author is a
// free-form display name ("operator", a bot persona, "system"); an empty
// Author renders as "anonymous" downstream.
type Comment struct {
	ID        string    `json:"id"`
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}
