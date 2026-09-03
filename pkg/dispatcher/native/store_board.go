package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Board returns a defensive copy of the current board config.
func (s *Store) Board() *Board {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneBoard(s.board)
}

// SetBoard validates and replaces the board configuration. The disk
// write happens BEFORE the in-memory swap so a write failure leaves
// both the live store and on-disk state consistent on the old board
// — the previous order (swap → write) silently diverged in-memory
// from disk on EIO / quota / permission errors (F-CD-9).
//
// SetBoard does NOT migrate issues: replacing the state list here leaves
// issues pointing at states that may no longer exist (they fall into the
// studio's "__unmapped__" bucket). Use it only for whole-board seeds and
// no-migration edits. Column renames/deletes that must move issues go
// through RenameState/DeleteState, which cascade across the issue files.
func (s *Store) SetBoard(b *Board) (err error) {
	if err := b.Validate(); err != nil {
		return err
	}
	clone := cloneBoard(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetBoard", &err)
	prev := s.board
	s.board = clone
	if err := s.writeBoardLocked(); err != nil {
		s.board = prev
		return err
	}
	return s.emitPostCommitEvent(Event{Type: EvtBoardUpdated})
}

// ErrStateNotEmpty is returned by DeleteState when the target column
// still holds issues and no migration target was supplied. The HTTP
// layer maps it to 409 so the UI can prompt for a destination column.
var ErrStateNotEmpty = errors.New("native store: state has issues; migration target required")

// setBoardLocked validates a candidate board, swaps it in, and persists
// it, rolling back to the previous board on a write failure (mirrors
// SetBoard's commit discipline). The caller already holds s.mu. It does
// NOT emit an event — column mutators emit a precise EvtBoardUpdated with
// an op discriminator after any per-issue cascade completes.
func (s *Store) setBoardLocked(next *Board) error {
	if err := next.Validate(); err != nil {
		return err
	}
	prev := s.board
	s.board = next
	if err := s.writeBoardLocked(); err != nil {
		s.board = prev
		return err
	}
	return nil
}

// commitBoardLocked persists next and, on success, emits the
// board-updated event carrying payload. The caller must already hold
// s.mu. Used by the mutators (Add/Update State/Field, ...) whose
// commit isn't interleaved with a cascade across issues — those call
// setBoardLocked and emitPostCommitEvent separately instead.
func (s *Store) commitBoardLocked(next *Board, payload map[string]any) error {
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{Type: EvtBoardUpdated, Payload: payload})
}

// migrateStateLocked rewrites every indexed issue in state `from` to
// state `to`, emitting one EvtIssueState per touched issue with the
// given reason. The caller already holds s.mu and has validated both
// states. Returns the number of issues moved. A mid-loop write failure
// leaves earlier issues migrated and later ones untouched — acceptable
// and self-consistent (those issues simply stay in `from` until retried,
// or surface in the "__unmapped__" bucket if the column is already gone);
// recoverMutator rebuilds the index from disk on panic. Mirrors the
// partial-progress contract of applyLabelRewriteLocked.
func (s *Store) migrateStateLocked(from, to, reason string) (int, error) {
	touched := 0
	for id, iss := range s.index {
		if iss.State != from {
			continue
		}
		// Clone before mutating: index entries are shared with reader
		// goroutines holding earlier defensive copies.
		next := cloneIssue(iss)
		next.State = to
		next.UpdatedAt = time.Now().UTC()
		if err := s.writeIssueLocked(next); err != nil {
			return touched, fmt.Errorf("native store: write %s during state migration: %w", id, err)
		}
		s.index[id] = next
		if err := s.emitPostCommitEvent(Event{
			Type:    EvtIssueState,
			IssueID: id,
			Payload: map[string]any{"from": from, "to": to, "reason": reason},
		}); err != nil {
			return touched, err
		}
		touched++
	}
	return touched, nil
}

// AddState appends a new column to the board. The column lands last; the
// operator reorders afterward via ReorderStates. Rejects an empty or
// duplicate name. No issue migration.
func (s *Store) AddState(st State) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("AddState", &err)
	if st.Name == "" {
		return errors.New("native store: state name cannot be empty")
	}
	if s.board.StateByName(st.Name) != nil {
		return fmt.Errorf("native store: state %q already exists", st.Name)
	}
	next := cloneBoard(s.board)
	next.States = append(next.States, st)
	return s.commitBoardLocked(next, map[string]any{"op": "state_add", "state": st.Name})
}

// RenameState renames a column and cascades the change to every issue in
// it. Renaming onto an existing column is refused (it would silently
// merge two columns' semantics — delete-with-migrate is the explicit path
// for that). Renaming to itself is a no-op. Returns the number of issues
// touched. The board is renamed first, then issues are migrated, so a
// mid-cascade failure leaves a renamed column with some issues still
// carrying the old name (they land in "__unmapped__" until retried).
func (s *Store) RenameState(from, to string) (touched int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("RenameState", &err)
	if from == "" || to == "" {
		return 0, errors.New("native store: state name cannot be empty")
	}
	if from == to {
		return 0, nil
	}
	idx := s.board.stateIndex(from)
	if idx < 0 {
		return 0, fmt.Errorf("native store: unknown state %q", from)
	}
	if s.board.StateByName(to) != nil {
		return 0, fmt.Errorf("native store: target state %q already exists; delete-with-migrate to merge columns", to)
	}
	next := cloneBoard(s.board)
	next.States[idx].Name = to
	if err := s.setBoardLocked(next); err != nil {
		return 0, err
	}
	touched, err = s.migrateStateLocked(from, to, tracker.ReasonStateRename)
	if err != nil {
		return touched, err
	}
	return touched, s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "state_rename", "from": from, "to": to},
	})
}

// DeleteState removes a column. If it still holds issues, migrateTo must
// name another existing column to receive them (else ErrStateNotEmpty).
// Refuses to delete the last remaining column. Issues are migrated first,
// then the column is dropped, so no issue is ever left in a column that
// no longer exists. Returns the number of issues migrated.
func (s *Store) DeleteState(name, migrateTo string) (touched int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("DeleteState", &err)
	if s.board.stateIndex(name) < 0 {
		return 0, fmt.Errorf("native store: unknown state %q", name)
	}
	if len(s.board.States) <= 1 {
		return 0, errors.New("native store: cannot delete the last column")
	}
	count := 0
	for _, iss := range s.index {
		if iss.State == name {
			count++
		}
	}
	if count > 0 {
		if migrateTo == "" {
			return 0, ErrStateNotEmpty
		}
		if migrateTo == name {
			return 0, errors.New("native store: migration target must differ from the deleted state")
		}
		if s.board.StateByName(migrateTo) == nil {
			return 0, fmt.Errorf("native store: unknown migration target %q", migrateTo)
		}
		// Deleting a TERMINAL column with a working-state target reopens
		// every card in it at once — a reopen more powerful than Reopen
		// itself, which refuses when dependents were already promoted on a
		// card's completion. Hold it to the same bar rather than letting
		// the column editor be the way around the sink.
		if err := s.reopenMigrationAllowedLocked(name, migrateTo); err != nil {
			return 0, err
		}
		touched, err = s.migrateStateLocked(name, migrateTo, tracker.ReasonStateDelete)
		if err != nil {
			return touched, err
		}
	}
	next := cloneBoard(s.board)
	idx := next.stateIndex(name)
	next.States = append(next.States[:idx], next.States[idx+1:]...)
	if err := s.setBoardLocked(next); err != nil {
		return touched, err
	}
	return touched, s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "state_delete", "state": name, "migrate_to": migrateTo},
	})
}

// reopenMigrationAllowedLocked refuses a column migration that would
// carry cards ACROSS the terminal boundary while dependents are still
// standing on their completion. Same predicate as Reopen — a bulk gesture
// does not earn an exemption the single-card one is refused.
func (s *Store) reopenMigrationAllowedLocked(from, to string) error {
	all := make([]*Issue, 0, len(s.index))
	for _, iss := range s.index {
		all = append(all, iss)
	}
	return ReopenMigrationAllowed(s.board, all, from, to)
}

// ReopenMigrationAllowed refuses a column migration that carries cards
// ACROSS the terminal boundary while dependents still stand on their
// completion — the bulk form of Reopen's own check, shared by both twins
// so the board editor cannot be a way around the sink on one of them.
func ReopenMigrationAllowed(b *Board, all []*Issue, from, to string) error {
	src := b.StateByName(from)
	dst := b.StateByName(to)
	if src == nil || !src.Terminal || (dst != nil && dst.Terminal) {
		return nil
	}
	idx := promotedDependents(all)
	for _, iss := range all {
		if iss.State != from {
			continue
		}
		if err := reopenBlocked(idx, iss.ID, iss.State); err != nil {
			return err
		}
	}
	return nil
}

// StatePatch carries the editable per-column fields for UpdateState.
// Nil pointers leave the corresponding field untouched.
type StatePatch struct {
	Display  *string `json:"display,omitempty"`
	Color    *string `json:"color,omitempty"`
	Eligible *bool   `json:"eligible,omitempty"`
	Terminal *bool   `json:"terminal,omitempty"`
}

// UpdateState edits a column's display name, color, and eligible/terminal
// flags. It never renames (that cascades — use RenameState) and never
// migrates issues.
func (s *Store) UpdateState(name string, p StatePatch) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("UpdateState", &err)
	idx := s.board.stateIndex(name)
	if idx < 0 {
		return fmt.Errorf("native store: unknown state %q", name)
	}
	next := cloneBoard(s.board)
	st := &next.States[idx]
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
	return s.commitBoardLocked(next, map[string]any{"op": "state_update", "state": name})
}

// ---------------------------------------------------------------------------
// Custom field schema management (board.Fields). Mirrors the state
// mutators: granular ops, atomic under s.mu, with a key cascade across
// issue.Fields maps on rename/delete so no issue is left referencing a
// field the schema no longer knows (which Update's ValidateFieldValues
// would otherwise reject).
// ---------------------------------------------------------------------------

// ReorderStates rewrites the column order. `order` must be a permutation
// of the current state names (same set, no missing/extra/duplicate
// entries). Never migrates issues.
func (s *Store) ReorderStates(order []string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("ReorderStates", &err)
	reordered, err := reorderByName(s.board.States, order, func(st State) string { return st.Name }, "state")
	if err != nil {
		return err
	}
	next := cloneBoard(s.board)
	next.States = reordered
	return s.commitBoardLocked(next, map[string]any{"op": "state_reorder"})
}

// loadOrInitBoard runs at construction time, before s is exposed to any
// concurrent caller, so it does not lock.
func (s *Store) loadOrInitBoard() error {
	p := filepath.Join(s.root, boardFile)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		s.board = DefaultBoard()
		return s.writeBoardLocked()
	}
	if err != nil {
		return fmt.Errorf("native store: read board: %w", err)
	}
	var b Board
	if err := json.Unmarshal(data, &b); err != nil {
		return fmt.Errorf("native store: parse board: %w", err)
	}
	if err := b.Validate(); err != nil {
		return fmt.Errorf("native store: invalid board: %w", err)
	}
	s.board = &b
	// Boards persisted by an older iterion may predate the `inbox` /
	// `awaiting_input` states — apply the shared schema upgrade once and
	// persist, so bots emitting findings and the dispatcher's paused-run
	// parking work without manual board.json edits.
	if UpgradeBoardSchema(s.board) {
		if err := s.writeBoardLocked(); err != nil {
			return fmt.Errorf("native store: persist board schema upgrade: %w", err)
		}
	}
	return nil
}

func (s *Store) writeBoardLocked() error {
	s.board.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(s.board, "", "  ")
	if err != nil {
		return fmt.Errorf("native store: marshal board: %w", err)
	}
	p := filepath.Join(s.root, boardFile)
	if err := store.WriteFileAtomic(p, data, filePerm); err != nil {
		return fmt.Errorf("native store: write board: %w", err)
	}
	return nil
}

func cloneBoard(b *Board) *Board {
	c := *b
	c.States = append([]State(nil), b.States...)
	c.Fields = append([]Field(nil), b.Fields...)
	c.Views = append([]View(nil), b.Views...)
	return &c
}
