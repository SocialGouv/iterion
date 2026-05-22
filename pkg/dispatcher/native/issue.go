package native

import "time"

// Issue is the native tracker's source-of-truth issue record. The
// dispatcher consumes a normalized view via tracker.Issue (see
// pkg/dispatcher/tracker/native.go for the conversion).
type Issue struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Body     string         `json:"body,omitempty"`
	State    string         `json:"state"`
	Labels   []string       `json:"labels,omitempty"`
	Priority int            `json:"priority,omitempty"`
	Assignee string         `json:"assignee,omitempty"`
	Blockers []string       `json:"blockers,omitempty"`
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
	// LastWorkdir is the absolute filesystem path the last run
	// executed in — either the per-issue dispatcher workspace or,
	// when `worktree: auto` was used, the run's git worktree path.
	// The studio exposes it as a copy-to-clipboard / vscode://file
	// link so the operator can inspect the diff manually.
	LastWorkdir string    `json:"last_workdir,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
