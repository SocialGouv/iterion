package native

import (
	"errors"
	"fmt"
	"time"
)

// applyFieldRewriteLocked rewrites each indexed issue's Fields map via
// transform, persisting + reindexing + emitting EvtIssueUpdated per
// changed issue. The caller already holds s.mu. Returns issues touched.
// Mirrors applyLabelRewriteLocked's partial-progress contract.
func (s *Store) applyFieldRewriteLocked(
	transform func(fields map[string]any) (map[string]any, bool),
	reason string,
) (int, error) {
	touched := 0
	for id, iss := range s.index {
		if len(iss.Fields) == 0 {
			continue
		}
		nextFields, changed := transform(iss.Fields)
		if !changed {
			continue
		}
		next := cloneIssue(iss)
		next.Fields = nextFields
		next.UpdatedAt = time.Now().UTC()
		if err := s.writeIssueLocked(next); err != nil {
			return touched, fmt.Errorf("native store: write %s during %s: %w", id, reason, err)
		}
		s.index[id] = next
		if err := s.emitPostCommitEvent(Event{
			Type:    EvtIssueUpdated,
			IssueID: id,
			Payload: map[string]any{"changed": []string{"fields"}, "reason": reason},
		}); err != nil {
			return touched, err
		}
		touched++
	}
	return touched, nil
}

// AddField appends a new custom-field definition. Rejects empty/duplicate
// names; the candidate board is validated (enum needs values, known type).
func (s *Store) AddField(f Field) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("AddField", &err)
	if f.Name == "" {
		return errors.New("native store: field name cannot be empty")
	}
	if s.board.FieldByName(f.Name) != nil {
		return fmt.Errorf("native store: field %q already exists", f.Name)
	}
	next := cloneBoard(s.board)
	next.Fields = append(next.Fields, f)
	return s.commitBoardLocked(next, map[string]any{"op": "field_add", "field": f.Name})
}

// FieldPatch carries the editable definition fields for UpdateField. A
// nil pointer leaves the corresponding attribute untouched. Renames go
// through RenameField (they cascade), never here.
type FieldPatch struct {
	Display    *string    `json:"display,omitempty"`
	Type       *FieldType `json:"type,omitempty"`
	Required   *bool      `json:"required,omitempty"`
	EnumValues *[]string  `json:"enum_values,omitempty"`
}

// UpdateField edits a field definition in place (no rename, no value
// migration). The amended board is validated before commit.
func (s *Store) UpdateField(name string, p FieldPatch) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("UpdateField", &err)
	idx := s.board.fieldIndex(name)
	if idx < 0 {
		return fmt.Errorf("native store: unknown field %q", name)
	}
	next := cloneBoard(s.board)
	f := &next.Fields[idx]
	if p.Display != nil {
		f.Display = *p.Display
	}
	if p.Type != nil {
		f.Type = *p.Type
	}
	if p.Required != nil {
		f.Required = *p.Required
	}
	if p.EnumValues != nil {
		f.EnumValues = *p.EnumValues
	}
	return s.commitBoardLocked(next, map[string]any{"op": "field_update", "field": name})
}

// RenameField renames a field definition and cascades the key across
// every issue's Fields map. Refuses renaming onto an existing field.
func (s *Store) RenameField(from, to string) (touched int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("RenameField", &err)
	if from == "" || to == "" {
		return 0, errors.New("native store: field name cannot be empty")
	}
	if from == to {
		return 0, nil
	}
	idx := s.board.fieldIndex(from)
	if idx < 0 {
		return 0, fmt.Errorf("native store: unknown field %q", from)
	}
	if s.board.FieldByName(to) != nil {
		return 0, fmt.Errorf("native store: target field %q already exists", to)
	}
	next := cloneBoard(s.board)
	next.Fields[idx].Name = to
	if err := s.setBoardLocked(next); err != nil {
		return 0, err
	}
	touched, err = s.applyFieldRewriteLocked(func(fields map[string]any) (map[string]any, bool) {
		v, ok := fields[from]
		if !ok {
			return fields, false
		}
		out := make(map[string]any, len(fields))
		for k, val := range fields {
			if k == from {
				continue
			}
			out[k] = val
		}
		out[to] = v
		return out, true
	}, "field_rename")
	if err != nil {
		return touched, err
	}
	return touched, s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "field_rename", "from": from, "to": to},
	})
}

// DeleteField removes a field definition and strips its key from every
// issue (so no issue keeps a value the schema no longer validates).
func (s *Store) DeleteField(name string) (touched int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("DeleteField", &err)
	idx := s.board.fieldIndex(name)
	if idx < 0 {
		return 0, fmt.Errorf("native store: unknown field %q", name)
	}
	touched, err = s.applyFieldRewriteLocked(func(fields map[string]any) (map[string]any, bool) {
		if _, ok := fields[name]; !ok {
			return fields, false
		}
		out := make(map[string]any, len(fields))
		for k, val := range fields {
			if k != name {
				out[k] = val
			}
		}
		return out, true
	}, "field_delete")
	if err != nil {
		return touched, err
	}
	next := cloneBoard(s.board)
	next.Fields = append(next.Fields[:idx], next.Fields[idx+1:]...)
	if err := s.setBoardLocked(next); err != nil {
		return touched, err
	}
	return touched, s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "field_delete", "field": name},
	})
}

// reorderByName validates that `order` is a permutation of the names of
// `items` (per the name accessor) and returns a new slice in that order.
// `kind` labels errors ("state"/"field"). Shared by ReorderStates and
// ReorderFields, whose only difference is the element type.
func reorderByName[T any](items []T, order []string, name func(T) string, kind string) ([]T, error) {
	if len(order) != len(items) {
		return nil, fmt.Errorf("native store: reorder expects %d %ss, got %d", len(items), kind, len(order))
	}
	pos := make(map[string]int, len(items))
	for i, it := range items {
		pos[name(it)] = i
	}
	seen := map[string]bool{}
	out := make([]T, 0, len(order))
	for _, n := range order {
		if seen[n] {
			return nil, fmt.Errorf("native store: duplicate %s %q in reorder", kind, n)
		}
		i, ok := pos[n]
		if !ok {
			return nil, fmt.Errorf("native store: unknown %s %q in reorder", kind, n)
		}
		seen[n] = true
		out = append(out, items[i])
	}
	return out, nil
}

// ReorderFields rewrites the field order. `order` must be a permutation
// of the current field names. Never touches issues.
func (s *Store) ReorderFields(order []string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("ReorderFields", &err)
	reordered, err := reorderByName(s.board.Fields, order, func(f Field) string { return f.Name }, "field")
	if err != nil {
		return err
	}
	next := cloneBoard(s.board)
	next.Fields = reordered
	return s.commitBoardLocked(next, map[string]any{"op": "field_reorder"})
}

// ---------------------------------------------------------------------------
// Saved views (board.Views): named filter/sort/group presets, persisted in
// board.json so they're shared across operators. No issue migration.
// ---------------------------------------------------------------------------

// SaveView upserts a named view (replaces by name if it exists, else
// appends). Rejects an empty name.
func (s *Store) SaveView(v View) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SaveView", &err)
	if v.Name == "" {
		return errors.New("native store: view name cannot be empty")
	}
	next := cloneBoard(s.board)
	replaced := false
	for i := range next.Views {
		if next.Views[i].Name == v.Name {
			next.Views[i] = v
			replaced = true
			break
		}
	}
	if !replaced {
		next.Views = append(next.Views, v)
	}
	return s.commitBoardLocked(next, map[string]any{"op": "view_save", "view": v.Name})
}

// DeleteView removes a named view. Unknown names error.
func (s *Store) DeleteView(name string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("DeleteView", &err)
	idx := -1
	for i := range s.board.Views {
		if s.board.Views[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("native store: unknown view %q", name)
	}
	next := cloneBoard(s.board)
	next.Views = append(next.Views[:idx], next.Views[idx+1:]...)
	return s.commitBoardLocked(next, map[string]any{"op": "view_delete", "view": name})
}
