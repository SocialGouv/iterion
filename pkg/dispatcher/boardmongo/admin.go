package boardmongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// This file makes *Store satisfy native.BoardAdmin — the board-CONFIG
// mutation surface (columns, custom fields, saved views, label vocabulary)
// plus the cascades to issues. It MIRRORS the filesystem *native.Store
// implementations (store.go AddState…DeleteLabel) exactly: same validation,
// same sentinel errors / error strings, same return semantics (touched
// counts), and the same event vocabulary in the same order — per-issue
// cascade events first, then one op-discriminated EvtBoardUpdated for the
// state/field config ops; label ops emit only per-issue events. The only
// difference is the substrate: instead of an in-memory index under a mutex,
// the cascade walks the tenant's Mongo issue docs (always tenant-scoped) and
// rewrites them via s.replace. There is no cross-op lock here (each Mongo op
// is independent), so a board with thousands of issues sees the same
// partial-progress contract native documents on a mid-cascade failure.
var _ native.BoardAdmin = (*Store)(nil)

// persistBoard is the Mongo twin of native's setBoardLocked: it validates the
// candidate board and writes it WITHOUT emitting an event. Each config mutator
// emits its own precise op-discriminated EvtBoardUpdated after any per-issue
// cascade completes (matching native's emit ordering exactly). SetBoard, by
// contrast, emits the bare EvtBoardUpdated — so the config ops must not route
// through it or they'd double-emit.
func (s *Store) persistBoard(ctx context.Context, b *native.Board) error {
	if err := b.Validate(); err != nil {
		return err
	}
	b.UpdatedAt = time.Now().UTC()
	if _, err := s.config.ReplaceOne(ctx, bson.M{"_id": s.tenant}, configDoc{Tenant: s.tenant, Board: *b}, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("boardmongo: persist board: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// State (column) management. AddState/UpdateState/ReorderStates never touch
// issues; RenameState/DeleteState cascade via migrateState.
// ---------------------------------------------------------------------------

// migrateState rewrites every tenant issue in state `from` to `to`, emitting
// one EvtIssueState per touched issue with the given reason. Mirrors native's
// migrateStateLocked, including the per-issue partial-progress contract.
func (s *Store) migrateState(ctx context.Context, from, to, reason string) (int, error) {
	all, err := s.listAll(ctx)
	if err != nil {
		return 0, err
	}
	touched := 0
	for i := range all {
		iss := all[i]
		if iss.State != from {
			continue
		}
		iss.State = to
		iss.UpdatedAt = time.Now().UTC()
		if err := s.replace(ctx, &iss, "state"); err != nil {
			return touched, fmt.Errorf("boardmongo: write %s during state migration: %w", iss.ID, err)
		}
		if err := s.emit(native.Event{
			Type:    native.EvtIssueState,
			IssueID: iss.ID,
			Payload: map[string]any{"from": from, "to": to, "reason": reason},
		}); err != nil {
			return touched, err
		}
		touched++
	}
	return touched, nil
}

// AddState appends a new column to the board. Rejects an empty or duplicate
// name. No issue migration. Mirrors native.Store.AddState.
func (s *Store) AddState(st native.State) error {
	if st.Name == "" {
		return errors.New("boardmongo: state name cannot be empty")
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	if board.StateByName(st.Name) != nil {
		return fmt.Errorf("boardmongo: state %q already exists", st.Name)
	}
	board.States = append(board.States, st)
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "state_add", "state": st.Name},
	})
}

// RenameState renames a column and cascades to every issue in it. Renaming
// onto an existing column is refused; renaming to itself is a no-op. The
// board is renamed first, then issues are migrated. Mirrors
// native.Store.RenameState.
func (s *Store) RenameState(from, to string) (int, error) {
	if from == "" || to == "" {
		return 0, errors.New("boardmongo: state name cannot be empty")
	}
	if from == to {
		return 0, nil
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	idx := stateIndex(board, from)
	if idx < 0 {
		return 0, fmt.Errorf("boardmongo: unknown state %q", from)
	}
	if board.StateByName(to) != nil {
		return 0, fmt.Errorf("boardmongo: target state %q already exists; delete-with-migrate to merge columns", to)
	}
	board.States[idx].Name = to
	if err := s.persistBoard(ctx, board); err != nil {
		return 0, err
	}
	touched, err := s.migrateState(ctx, from, to, tracker.ReasonStateRename)
	if err != nil {
		return touched, err
	}
	return touched, s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "state_rename", "from": from, "to": to},
	})
}

// DeleteState removes a column. If it still holds issues, migrateTo must name
// another existing column (else native.ErrStateNotEmpty). Refuses to delete
// the last column. Issues are migrated first, then the column is dropped.
// Mirrors native.Store.DeleteState.
func (s *Store) DeleteState(name, migrateTo string) (int, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	if stateIndex(board, name) < 0 {
		return 0, fmt.Errorf("boardmongo: unknown state %q", name)
	}
	if len(board.States) <= 1 {
		return 0, errors.New("boardmongo: cannot delete the last column")
	}
	all, err := s.listAll(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range all {
		if all[i].State == name {
			count++
		}
	}
	touched := 0
	if count > 0 {
		if migrateTo == "" {
			return 0, native.ErrStateNotEmpty
		}
		if migrateTo == name {
			return 0, errors.New("boardmongo: migration target must differ from the deleted state")
		}
		if board.StateByName(migrateTo) == nil {
			return 0, fmt.Errorf("boardmongo: unknown migration target %q", migrateTo)
		}
		// A terminal column emptied into a working one is a bulk reopen —
		// held to the same dependents check as the single-card Reopen, on
		// both twins.
		ptrs := make([]*native.Issue, len(all))
		for i := range all {
			ptrs[i] = &all[i]
		}
		if err := native.ReopenMigrationAllowed(board, ptrs, name, migrateTo); err != nil {
			return 0, err
		}
		touched, err = s.migrateState(ctx, name, migrateTo, tracker.ReasonStateDelete)
		if err != nil {
			return touched, err
		}
	}
	idx := stateIndex(board, name)
	board.States = append(board.States[:idx], board.States[idx+1:]...)
	if err := s.persistBoard(ctx, board); err != nil {
		return touched, err
	}
	return touched, s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "state_delete", "state": name, "migrate_to": migrateTo},
	})
}

// UpdateState edits a column's display/color/eligible/terminal flags. Never
// renames, never migrates issues. Mirrors native.Store.UpdateState.
func (s *Store) UpdateState(name string, p native.StatePatch) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	idx := stateIndex(board, name)
	if idx < 0 {
		return fmt.Errorf("boardmongo: unknown state %q", name)
	}
	st := &board.States[idx]
	if p.Display != nil {
		st.Display = *p.Display
	}
	if p.Color != nil {
		st.Color = *p.Color
	}
	if p.Eligible != nil {
		st.Eligible = *p.Eligible
	}
	if p.Terminal != nil {
		st.Terminal = *p.Terminal
	}
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "state_update", "state": name},
	})
}

// ReorderStates rewrites the column order. `order` must be a permutation of
// the current state names. Never migrates issues. Mirrors
// native.Store.ReorderStates.
func (s *Store) ReorderStates(order []string) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	reordered, err := reorderByName(board.States, order, func(st native.State) string { return st.Name }, "state")
	if err != nil {
		return err
	}
	board.States = reordered
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "state_reorder"},
	})
}

// ---------------------------------------------------------------------------
// Custom field schema management. AddField/UpdateField/ReorderFields never
// touch issues; RenameField/DeleteField cascade the issue.Fields key map.
// ---------------------------------------------------------------------------

// applyFieldRewrite rewrites each tenant issue's Fields map via transform,
// persisting + emitting EvtIssueUpdated per changed issue. Mirrors native's
// applyFieldRewriteLocked, including the partial-progress contract.
func (s *Store) applyFieldRewrite(ctx context.Context, transform func(fields map[string]any) (map[string]any, bool), reason string) (int, error) {
	all, err := s.listAll(ctx)
	if err != nil {
		return 0, err
	}
	touched := 0
	for i := range all {
		iss := all[i]
		if len(iss.Fields) == 0 {
			continue
		}
		nextFields, changed := transform(iss.Fields)
		if !changed {
			continue
		}
		iss.Fields = nextFields
		iss.UpdatedAt = time.Now().UTC()
		if err := s.replace(ctx, &iss, "fields"); err != nil {
			return touched, fmt.Errorf("boardmongo: write %s during %s: %w", iss.ID, reason, err)
		}
		if err := s.emit(native.Event{
			Type:    native.EvtIssueUpdated,
			IssueID: iss.ID,
			Payload: map[string]any{"changed": []string{"fields"}, "reason": reason},
		}); err != nil {
			return touched, err
		}
		touched++
	}
	return touched, nil
}

// AddField appends a new custom-field definition. Rejects empty/duplicate
// names; the candidate board is validated (enum needs values, known type) by
// persistBoard. Mirrors native.Store.AddField.
func (s *Store) AddField(f native.Field) error {
	if f.Name == "" {
		return errors.New("boardmongo: field name cannot be empty")
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	if board.FieldByName(f.Name) != nil {
		return fmt.Errorf("boardmongo: field %q already exists", f.Name)
	}
	board.Fields = append(board.Fields, f)
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "field_add", "field": f.Name},
	})
}

// UpdateField edits a field definition in place (no rename, no value
// migration). The amended board is validated before commit. Mirrors
// native.Store.UpdateField.
func (s *Store) UpdateField(name string, p native.FieldPatch) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	idx := fieldIndex(board, name)
	if idx < 0 {
		return fmt.Errorf("boardmongo: unknown field %q", name)
	}
	f := &board.Fields[idx]
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
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "field_update", "field": name},
	})
}

// RenameField renames a field definition and cascades the key across every
// issue's Fields map. Refuses renaming onto an existing field. Mirrors
// native.Store.RenameField.
func (s *Store) RenameField(from, to string) (int, error) {
	if from == "" || to == "" {
		return 0, errors.New("boardmongo: field name cannot be empty")
	}
	if from == to {
		return 0, nil
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	idx := fieldIndex(board, from)
	if idx < 0 {
		return 0, fmt.Errorf("boardmongo: unknown field %q", from)
	}
	if board.FieldByName(to) != nil {
		return 0, fmt.Errorf("boardmongo: target field %q already exists", to)
	}
	board.Fields[idx].Name = to
	if err := s.persistBoard(ctx, board); err != nil {
		return 0, err
	}
	touched, err := s.applyFieldRewrite(ctx, func(fields map[string]any) (map[string]any, bool) {
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
	}, tracker.ReasonFieldRename)
	if err != nil {
		return touched, err
	}
	return touched, s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "field_rename", "from": from, "to": to},
	})
}

// DeleteField removes a field definition and strips its key from every issue.
// Mirrors native.Store.DeleteField.
func (s *Store) DeleteField(name string) (int, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	idx := fieldIndex(board, name)
	if idx < 0 {
		return 0, fmt.Errorf("boardmongo: unknown field %q", name)
	}
	touched, err := s.applyFieldRewrite(ctx, func(fields map[string]any) (map[string]any, bool) {
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
	}, tracker.ReasonFieldDelete)
	if err != nil {
		return touched, err
	}
	board.Fields = append(board.Fields[:idx], board.Fields[idx+1:]...)
	if err := s.persistBoard(ctx, board); err != nil {
		return touched, err
	}
	return touched, s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "field_delete", "field": name},
	})
}

// ReorderFields rewrites the field order. `order` must be a permutation of the
// current field names. Never touches issues. Mirrors
// native.Store.ReorderFields.
func (s *Store) ReorderFields(order []string) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	reordered, err := reorderByName(board.Fields, order, func(f native.Field) string { return f.Name }, "field")
	if err != nil {
		return err
	}
	board.Fields = reordered
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "field_reorder"},
	})
}

// ---------------------------------------------------------------------------
// Saved views: named filter/sort/group presets. No issue migration.
// ---------------------------------------------------------------------------

// SaveView upserts a named view (replaces by name if it exists, else
// appends). Rejects an empty name. Mirrors native.Store.SaveView.
func (s *Store) SaveView(v native.View) error {
	if v.Name == "" {
		return errors.New("boardmongo: view name cannot be empty")
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	replaced := false
	for i := range board.Views {
		if board.Views[i].Name == v.Name {
			board.Views[i] = v
			replaced = true
			break
		}
	}
	if !replaced {
		board.Views = append(board.Views, v)
	}
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "view_save", "view": v.Name},
	})
}

// DeleteView removes a named view. Unknown names error. Mirrors
// native.Store.DeleteView.
func (s *Store) DeleteView(name string) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	idx := -1
	for i := range board.Views {
		if board.Views[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("boardmongo: unknown view %q", name)
	}
	board.Views = append(board.Views[:idx], board.Views[idx+1:]...)
	if err := s.persistBoard(ctx, board); err != nil {
		return err
	}
	return s.emit(native.Event{
		Type:    native.EvtBoardUpdated,
		Payload: map[string]any{"op": "view_delete", "view": name},
	})
}

// ---------------------------------------------------------------------------
// Label vocabulary management. Rename/Merge/Delete rewrite every issue's
// Labels slice; they emit only per-issue label events (no board event),
// exactly like native.
// ---------------------------------------------------------------------------

// applyLabelRewrite is the shared scan-and-rewrite loop for
// Rename/Merge/Delete labels. transform receives an issue's current label
// slice and returns (new slice, changed?). On change the issue is rewritten
// and a per-issue label event is appended. Mirrors native's
// applyLabelRewriteLocked, including its event payload shape ({issue_id} +
// the op fields).
func (s *Store) applyLabelRewrite(ctx context.Context, transform func(labels []string) ([]string, bool), eventType native.EventType, payload map[string]any) (int, error) {
	all, err := s.listAll(ctx)
	if err != nil {
		return 0, err
	}
	touched := 0
	for i := range all {
		iss := all[i]
		// CAS-guarded on the labels this sweep READ, re-read + re-transform
		// on a miss: the sweep's listAll snapshot ages for the whole walk,
		// and an unguarded write re-applied it — resurrecting a one-shot
		// label the trigger spine had atomically consumed in the window
		// (same class as Update; the transform is pure over labels, so the
		// replay is exact).
		wrote := false
		wanted := false
		for attempt := 0; attempt < 3; attempt++ {
			newLabels, changed := transform(iss.Labels)
			if !changed {
				break
			}
			wanted = true
			preLabels := append([]string(nil), iss.Labels...)
			iss.Labels = newLabels
			iss.UpdatedAt = time.Now().UTC()
			matched, err := s.replaceGuarded(ctx, &iss, bson.M{"issue.labels": preLabels}, "labels")
			if err != nil {
				return touched, fmt.Errorf("boardmongo: write %s during %s: %w", iss.ID, eventType, err)
			}
			if matched {
				wrote = true
				break
			}
			fresh, err := s.get(ctx, iss.ID)
			if err != nil {
				if errors.Is(err, tracker.ErrNotFound) {
					break // deleted mid-sweep — a benign race, like the FS twin
				}
				return touched, fmt.Errorf("boardmongo: re-read %s during %s: %w", iss.ID, eventType, err)
			}
			iss = *fresh
		}
		if !wrote {
			if wanted {
				// The sweep WANTED to rewrite this card and lost the CAS
				// on every attempt — swallowing that leaves a label the
				// operator asked to remove (possibly a consume_labels
				// trigger) on the card, under a green return.
				return touched, fmt.Errorf("boardmongo: %s: card %s lost the label CAS on every attempt — re-run the operation", eventType, iss.ID)
			}
			continue
		}
		evtPayload := map[string]any{"issue_id": iss.ID}
		for k, v := range payload {
			evtPayload[k] = v
		}
		if err := s.emit(native.Event{Type: eventType, IssueID: iss.ID, Payload: evtPayload}); err != nil {
			return touched, err
		}
		touched++
	}
	return touched, nil
}

// RenameLabel rewrites every occurrence of `from` to `to` across all issues.
// No-op when from == to; native.ErrLabelEmpty if either side is empty.
// Dedupes onto an issue already carrying `to`. Mirrors native.Store.RenameLabel.
func (s *Store) RenameLabel(from, to string) (int, error) {
	if from == "" || to == "" {
		return 0, native.ErrLabelEmpty
	}
	if from == to {
		return 0, nil
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	return s.applyLabelRewrite(ctx, func(labels []string) ([]string, bool) {
		out := make([]string, 0, len(labels))
		changed := false
		seenTo := slices.Contains(labels, to)
		for _, l := range labels {
			if l == from {
				if seenTo {
					// `to` already on this issue → just drop `from`.
					changed = true
					continue
				}
				out = append(out, to)
				changed = true
				continue
			}
			out = append(out, l)
		}
		return out, changed
	}, native.EvtLabelRename, map[string]any{"from": from, "to": to})
}

// MergeLabels is rename's near-twin (audit event "label_merge"): every issue
// carrying `from` ends up carrying `to` and no longer `from`. Mirrors
// native.Store.MergeLabels.
func (s *Store) MergeLabels(from, to string) (int, error) {
	if from == "" || to == "" {
		return 0, native.ErrLabelEmpty
	}
	if from == to {
		return 0, nil
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	return s.applyLabelRewrite(ctx, func(labels []string) ([]string, bool) {
		out := make([]string, 0, len(labels))
		changed := false
		seenTo := slices.Contains(labels, to)
		for _, l := range labels {
			if l == from {
				if !seenTo {
					out = append(out, to)
					seenTo = true
				}
				changed = true
				continue
			}
			out = append(out, l)
		}
		return out, changed
	}, native.EvtLabelMerge, map[string]any{"from": from, "to": to})
}

// DeleteLabel strips `label` from every issue that carries it. Mirrors
// native.Store.DeleteLabel.
func (s *Store) DeleteLabel(label string) (int, error) {
	if label == "" {
		return 0, native.ErrLabelEmpty
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	return s.applyLabelRewrite(ctx, func(labels []string) ([]string, bool) {
		out := make([]string, 0, len(labels))
		changed := false
		for _, l := range labels {
			if l == label {
				changed = true
				continue
			}
			out = append(out, l)
		}
		return out, changed
	}, native.EvtLabelDelete, map[string]any{"label": label})
}

// ---------------------------------------------------------------------------
// Board-slice helpers. native's stateIndex/fieldIndex/reorderByName are
// unexported; these tenant-side equivalents operate on the *native.Board
// snapshot returned by s.Board().
// ---------------------------------------------------------------------------

func stateIndex(b *native.Board, name string) int {
	for i := range b.States {
		if b.States[i].Name == name {
			return i
		}
	}
	return -1
}

func fieldIndex(b *native.Board, name string) int {
	for i := range b.Fields {
		if b.Fields[i].Name == name {
			return i
		}
	}
	return -1
}

// reorderByName validates that `order` is a permutation of the names of
// `items` and returns a new slice in that order. Mirrors native's unexported
// reorderByName generic.
func reorderByName[T any](items []T, order []string, name func(T) string, kind string) ([]T, error) {
	if len(order) != len(items) {
		return nil, fmt.Errorf("boardmongo: reorder expects %d %ss, got %d", len(items), kind, len(order))
	}
	pos := make(map[string]int, len(items))
	for i, it := range items {
		pos[name(it)] = i
	}
	seen := map[string]bool{}
	out := make([]T, 0, len(order))
	for _, n := range order {
		if seen[n] {
			return nil, fmt.Errorf("boardmongo: duplicate %s %q in reorder", kind, n)
		}
		i, ok := pos[n]
		if !ok {
			return nil, fmt.Errorf("boardmongo: unknown %s %q in reorder", kind, n)
		}
		seen[n] = true
		out = append(out, items[i])
	}
	return out, nil
}
