package native

import "time"

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
	// At is when the give-up was stamped (UTC).
	At time.Time `json:"at"`
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
