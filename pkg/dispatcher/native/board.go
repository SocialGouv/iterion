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
	StateBacklog    = "backlog"
	StateReady      = "ready"
	StateInProgress = "in_progress"
	StateReview     = "review"
	StateDone       = "done"
	StateBlocked    = "blocked"
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

// Board is the kanban configuration: ordered states + custom field schema.
type Board struct {
	States    []State   `json:"states"`
	Fields    []Field   `json:"fields,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultBoard returns the recommended starter board.
//
// Includes the `bot_args` custom field that the dispatcher reads at
// dispatch time (encoded `--var key=value` overrides per ticket).
// Bots like whats-next set this on create_issue; without it in the
// default schema, fresh local stores reject the field with
// `unknown field "bot_args"` and the bot wastes turns retrying.
func DefaultBoard() *Board {
	return &Board{
		States: []State{
			{Name: StateBacklog, Display: "Backlog"},
			{Name: StateReady, Display: "Ready", Eligible: true},
			{Name: StateInProgress, Display: "In progress", Eligible: true},
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
