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
	BotArgs   map[string]string `json:"bot_args,omitempty"`
	Claim     string            `json:"claim,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}
