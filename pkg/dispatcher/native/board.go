package native

import (
	"errors"
	"fmt"
	"time"
)

// Default state names emitted by [DefaultBoard]. Callers that
// customise the board can ignore these; tests and skills referring to
// the shipped defaults should use the constants so renames stay
// compile-checked.
const (
	StateInbox   = "inbox"
	StateBacklog = "backlog"
	StateReady   = "ready"
	// StateWaitingDeps holds a ticket whose hard blockers are not yet
	// StateDone. Non-eligible and non-terminal: the launch loop and the
	// dispatcher skip it, and it does not satisfy anyone else's blockers
	// (unlike StateBlocked, which is terminal "won't do").
	StateWaitingDeps = "waiting_deps"
	StateInProgress  = "in_progress"
	// StateAwaitingInput holds a dispatched card whose run paused waiting for
	// a human answer (paused_waiting_human). Non-eligible (the dispatcher
	// never re-picks it) and non-terminal (the run resumes on answer). The
	// column-level expression of the per-card AwaitingInput badge.
	StateAwaitingInput = "awaiting_input"
	StateReview        = "review"
	StateDone          = "done"
	// StateBlocked is terminal "won't do" / abandoned — not a temporary
	// hold for open deps (use StateWaitingDeps). A ticket in blocked does
	// NOT satisfy hard blockers of dependents (see BlockerSatisfied).
	StateBlocked = "blocked"
)

// FieldType enumerates the supported custom-field value kinds.
type FieldType string

const (
	FieldText   FieldType = "text"
	FieldNumber FieldType = "number"
	FieldEnum   FieldType = "enum"
	FieldDate   FieldType = "date"
	FieldBool   FieldType = "bool"
)

// State is one kanban column in the board.
type State struct {
	Name     string `json:"name"`
	Display  string `json:"display,omitempty"`
	Color    string `json:"color,omitempty"`
	Terminal bool   `json:"terminal,omitempty"`
	Eligible bool   `json:"eligible,omitempty"`
}

// Field is a custom field definition.
type Field struct {
	Name       string    `json:"name"`
	Display    string    `json:"display,omitempty"`
	Type       FieldType `json:"type"`
	Required   bool      `json:"required,omitempty"`
	EnumValues []string  `json:"enum_values,omitempty"`
	Default    any       `json:"default,omitempty"`
}

// View is a saved board filter/sort/group preset. Shared across operators
// via board.json; the studio's view picker loads one to restore the
// search query, label/assignee filters, card sort, and swimlane grouping.
type View struct {
	Name     string   `json:"name"`
	Search   string   `json:"search,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Assignee string   `json:"assignee,omitempty"`
	// Bot scopes the view to a single bot (Issue.Bot). Additive to the
	// group-by-bot swimlane lens: this is a persisted FILTER, so an
	// operator can save "the X pipeline" as a saved View.
	Bot     string `json:"bot,omitempty"`
	Sort    string `json:"sort,omitempty"`
	GroupBy string `json:"group_by,omitempty"`
}

// Board is the kanban configuration: ordered states + custom field schema
// + saved views.
type Board struct {
	States    []State   `json:"states"`
	Fields    []Field   `json:"fields,omitempty"`
	Views     []View    `json:"views,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultBoard returns the recommended starter board.
//
// `inbox` is the leftmost state and receives bot-emitted findings —
// short observations that aren't worth dispatching alone (a doc drift,
// a security smell, a bug surfaced during a feature run). Operators
// triage by dragging inbox → backlog (promote) or deleting the card
// (dismiss). Not eligible: the dispatcher never auto-picks inbox.
//
// Includes the `bot_args` custom field that the dispatcher reads at
// dispatch time (encoded `--var key=value` overrides per ticket).
// Bots like whats-next set this on create_issue; without it in the
// default schema, fresh local stores reject the field with
// `unknown field "bot_args"` and the bot wastes turns retrying.
func DefaultBoard() *Board {
	return &Board{
		States: []State{
			{Name: StateInbox, Display: "Inbox"},
			{Name: StateBacklog, Display: "Backlog"},
			{Name: StateReady, Display: "Ready", Eligible: true},
			{Name: StateWaitingDeps, Display: "Waiting on deps"},
			{Name: StateInProgress, Display: "In progress", Eligible: true},
			{Name: StateAwaitingInput, Display: "Awaiting input"},
			{Name: StateReview, Display: "Review"},
			{Name: StateDone, Display: "Done", Terminal: true},
			{Name: StateBlocked, Display: "Blocked", Terminal: true},
		},
		Fields: []Field{
			{Name: "bot_args", Display: "Bot args", Type: "text"},
		},
		UpdatedAt: time.Now().UTC(),
	}
}

// StateByName returns the state matching name, or nil.
func (b *Board) StateByName(name string) *State {
	for i := range b.States {
		if b.States[i].Name == name {
			return &b.States[i]
		}
	}
	return nil
}

// UpgradeBoardSchema applies the in-place upgrades a board persisted by an
// older iterion needs to work with the current dispatcher, returning true
// when it modified the board. Shared by the filesystem store
// (loadOrInitBoard, which persists the result) and the Mongo store (Board(),
// which normalizes on read). Idempotent; operator-customised boards keep
// their ordering.
//
//   - `inbox` (bot-emitted findings land there) is prepended when missing.
//   - `waiting_deps` (tickets with open hard blockers) is inserted right
//     after `ready` when missing; if there is no `ready`, after `backlog`.
//     Boards with neither are left untouched — the launch gate still
//     works via open_blocker_count alone.
//   - `awaiting_input` (the dispatcher parks a paused card there) is
//     inserted right after `in_progress` when missing. Boards without an
//     `in_progress` state are fully custom — left untouched; the
//     dispatcher's "stays in place" fallback covers them.
func UpgradeBoardSchema(b *Board) bool {
	changed := false
	if b.StateByName(StateInbox) == nil {
		b.States = append([]State{{Name: StateInbox, Display: "Inbox"}}, b.States...)
		changed = true
	}
	if b.StateByName(StateWaitingDeps) == nil {
		if insertStateAfter(b, StateReady, State{Name: StateWaitingDeps, Display: "Waiting on deps"}) ||
			insertStateAfter(b, StateBacklog, State{Name: StateWaitingDeps, Display: "Waiting on deps"}) {
			changed = true
		}
	}
	if b.StateByName(StateAwaitingInput) == nil {
		for i, st := range b.States {
			if st.Name != StateInProgress {
				continue
			}
			states := make([]State, 0, len(b.States)+1)
			states = append(states, b.States[:i+1]...)
			states = append(states, State{Name: StateAwaitingInput, Display: "Awaiting input"})
			states = append(states, b.States[i+1:]...)
			b.States = states
			changed = true
			break
		}
	}
	return changed
}

// insertStateAfter inserts st immediately after the state named after, if
// that anchor exists. Returns true when it modified the board.
func insertStateAfter(b *Board, after string, st State) bool {
	if b == nil {
		return false
	}
	for i, cur := range b.States {
		if cur.Name != after {
			continue
		}
		states := make([]State, 0, len(b.States)+1)
		states = append(states, b.States[:i+1]...)
		states = append(states, st)
		states = append(states, b.States[i+1:]...)
		b.States = states
		return true
	}
	return false
}

// stateIndex returns the position of the state matching name, or -1.
// Reorder/delete need the slice index; StateByName only yields a pointer.
func (b *Board) stateIndex(name string) int {
	for i := range b.States {
		if b.States[i].Name == name {
			return i
		}
	}
	return -1
}

// fieldIndex returns the position of the field matching name, or -1.
// Symmetric to stateIndex; the field mutators need the slice index.
func (b *Board) fieldIndex(name string) int {
	for i := range b.Fields {
		if b.Fields[i].Name == name {
			return i
		}
	}
	return -1
}

// FieldByName returns the field matching name, or nil.
func (b *Board) FieldByName(name string) *Field {
	for i := range b.Fields {
		if b.Fields[i].Name == name {
			return &b.Fields[i]
		}
	}
	return nil
}

// Validate checks the board is internally consistent. Returns nil on success.
func (b *Board) Validate() error {
	if len(b.States) == 0 {
		return errors.New("board: at least one state required")
	}
	seen := map[string]bool{}
	for _, s := range b.States {
		if s.Name == "" {
			return errors.New("board: state name must be non-empty")
		}
		if seen[s.Name] {
			return fmt.Errorf("board: duplicate state name %q", s.Name)
		}
		seen[s.Name] = true
	}
	fseen := map[string]bool{}
	for _, f := range b.Fields {
		if f.Name == "" {
			return errors.New("board: field name must be non-empty")
		}
		if fseen[f.Name] {
			return fmt.Errorf("board: duplicate field name %q", f.Name)
		}
		switch f.Type {
		case FieldText, FieldNumber, FieldDate, FieldBool:
		case FieldEnum:
			if len(f.EnumValues) == 0 {
				return fmt.Errorf("board: enum field %q requires enum_values", f.Name)
			}
		default:
			return fmt.Errorf("board: field %q has unknown type %q", f.Name, f.Type)
		}
		fseen[f.Name] = true
	}
	vseen := map[string]bool{}
	for _, v := range b.Views {
		if v.Name == "" {
			return errors.New("board: view name must be non-empty")
		}
		if vseen[v.Name] {
			return fmt.Errorf("board: duplicate view name %q", v.Name)
		}
		vseen[v.Name] = true
	}
	return nil
}

// ValidateFieldValues checks a map of custom field values against the board
// schema. Unknown fields or wrong types fail. Required fields must be present.
func (b *Board) ValidateFieldValues(values map[string]any) error {
	for k, v := range values {
		def := b.FieldByName(k)
		if def == nil {
			return fmt.Errorf("unknown field %q", k)
		}
		if err := def.validateValue(v); err != nil {
			return fmt.Errorf("field %q: %w", k, err)
		}
	}
	for _, f := range b.Fields {
		if !f.Required {
			continue
		}
		if _, ok := values[f.Name]; !ok {
			return fmt.Errorf("required field %q missing", f.Name)
		}
	}
	return nil
}

func (f *Field) validateValue(v any) error {
	if v == nil {
		if f.Required {
			return errors.New("required field cannot be null")
		}
		return nil
	}
	switch f.Type {
	case FieldText:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected text, got %T", v)
		}
	case FieldNumber:
		switch v.(type) {
		case float64, float32, int, int32, int64:
		default:
			return fmt.Errorf("expected number, got %T", v)
		}
	case FieldEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected enum string, got %T", v)
		}
		for _, e := range f.EnumValues {
			if e == s {
				return nil
			}
		}
		return fmt.Errorf("value %q not in enum_values", s)
	case FieldDate:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected RFC3339 date string, got %T", v)
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return fmt.Errorf("invalid date: %w", err)
		}
	case FieldBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected bool, got %T", v)
		}
	}
	return nil
}
