// Package native implements iterion's first-class issue/kanban tracker.
// Issues live as one JSON file per issue under <root>/issues/, a board
// config sits at <root>/board.json, and every mutation appends a
// monotonically-sequenced record to <root>/events.jsonl. All writes are
// serialized through a single mutex; reads scan the filesystem.
package native

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/google/uuid"
)

const (
	boardFile  = "board.json"
	issuesDir  = "issues"
	eventsFile = "events.jsonl"

	dirPerm  fs.FileMode = 0o755
	filePerm fs.FileMode = 0o644
)

// Store is the filesystem-backed native tracker store. Safe for
// concurrent use.
type Store struct {
	root string

	mu    sync.Mutex
	board *Board
	seq   int64

	// index is a hot in-memory mirror of issues/<id>.json. Filesystem
	// remains authoritative — index is populated at NewStore and kept
	// in sync on every write. List + Get walk the index instead of
	// hitting the filesystem, so a board with hundreds of issues
	// doesn't pay N file reads per query.
	index map[string]*Issue

	// watcher mirrors out-of-process writes (e.g. the `iterion
	// __mcp-board` stdio MCP subprocess) into the index. nil when
	// fsnotify isn't available on the host — the Store still works,
	// it just can't see writes by other processes, which is the
	// pre-watcher status quo.
	watcher *indexWatcher

	// pendingEvents buffers events whose appendEventLocked call
	// returned an error (transient fsync failure, NFS hiccup). Every
	// subsequent successful event flush drains the buffer first so a
	// downstream tailer eventually sees every state transition. State
	// recovery via populateIndex doesn't depend on events.jsonl, so
	// holding the buffer in memory is safe across the failure window.
	pendingEvents []Event

	// commentDispatcher, when set, lets a comment whose body leads with a
	// "/command" launch a bot — the native/local twin of the forge
	// issue-comment trigger. handleAddComment consults it for any comment the
	// request didn't already resolve (no explicit bot/bot_args). The resolver —
	// a server closure — does the command→bot lookup + the open_mr /
	// source_issue_ref stamp, keeping the store decoupled from the bot registry.
	commentDispatcher CommentDispatcher
}

// CommentDispatcher resolves a board-issue comment that leads with a "/command"
// into a bot launch: the bot to assign, the per-run bot_args (including the
// open_mr / source_issue_ref stamp for an opens-MR command), and the
// dispatch-eligible state to move the issue to. ok=false means "just record the
// comment, launch nothing". Installed by the server via SetCommentDispatcher;
// nil in a bare store (a plain `iterion dispatch` daemon or a unit test), where
// the comment is recorded with no dispatch — exactly the prior behaviour.
type CommentDispatcher func(iss Issue, commentBody string) (bot string, botArgs map[string]string, transitionTo string, ok bool)

// SetCommentDispatcher installs the slash-command resolver consulted by the
// POST /issues/{id}/comments handler. Called once at wiring time.
func (s *Store) SetCommentDispatcher(d CommentDispatcher) {
	s.mu.Lock()
	s.commentDispatcher = d
	s.mu.Unlock()
}

// getCommentDispatcher returns the installed resolver (nil if none) under the
// store lock, so a wiring-time SetCommentDispatcher races cleanly with serving.
func (s *Store) getCommentDispatcher() CommentDispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commentDispatcher
}

// NewStore opens (or initializes) the native tracker at root. If
// board.json is absent a default board is written.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("native store: root path required")
	}
	if err := os.MkdirAll(filepath.Join(root, issuesDir), dirPerm); err != nil {
		return nil, fmt.Errorf("native store: mkdir: %w", err)
	}
	s := &Store{root: root, index: map[string]*Issue{}}
	if err := s.loadOrInitBoard(); err != nil {
		return nil, err
	}
	// Seed the event sequence counter from any existing log so a
	// fresh process opening a pre-existing store doesn't restart Seq
	// at 0 and produce duplicate sequence numbers in events.jsonl.
	// Best-effort: a partial scan still advances seq past the
	// readable prefix; the warning is for the operator.
	var maxSeq int64 = -1
	if err := s.ScanEvents(func(e *Event) bool {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		return true
	}); err != nil {
		_ = err
	}
	s.seq = maxSeq + 1

	// Populate the index from disk. Corrupt files are skipped (a
	// warning would be nice but the store doesn't carry a logger).
	if err := s.populateIndex(); err != nil {
		return nil, err
	}

	// Start the fsnotify watcher AFTER the initial index population
	// so the watcher can never overwrite a fresh load with a stale
	// disk snapshot. A failure here is non-fatal — the Store keeps
	// working, just blind to out-of-process writes (the historical
	// behaviour). We don't log because the package carries no logger
	// today; the missing-watcher symptom (stale board reads) is
	// already documented as a known mode in the cache-desync finding.
	if w, err := startIndexWatcher(s); err == nil {
		s.watcher = w
	}
	return s, nil
}

// Close releases store-owned resources (currently the fsnotify
// watcher goroutine). Safe to call multiple times; safe on a Store
// whose watcher never started.
func (s *Store) Close() error {
	if s == nil || s.watcher == nil {
		return nil
	}
	return s.watcher.Close()
}

func (s *Store) populateIndex() error {
	entries, err := os.ReadDir(filepath.Join(s.root, issuesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("native store: scan issues: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		id := decodeID(strings.TrimSuffix(e.Name(), ".json"))
		iss, err := s.readIssueFromDisk(id)
		if err != nil {
			continue
		}
		s.index[id] = iss
	}
	return nil
}

// readIssueFromDisk bypasses the index — used only at NewStore to
// populate the cache from the authoritative on-disk files. Post-init
// reads should go through the index via readIssueLocked.
func (s *Store) readIssueFromDisk(id string) (*Issue, error) {
	data, err := os.ReadFile(s.issuePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, tracker.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("native store: read issue: %w", err)
	}
	var iss Issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return nil, fmt.Errorf("native store: parse issue %s: %w", id, err)
	}
	return &iss, nil
}

// Root returns the on-disk root directory.
func (s *Store) Root() string { return s.root }

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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "state_add", "state": st.Name},
	})
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
	touched, err = s.migrateStateLocked(from, to, "state_rename")
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
		touched, err = s.migrateStateLocked(name, migrateTo, "state_delete")
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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "state_update", "state": name},
	})
}

// ---------------------------------------------------------------------------
// Custom field schema management (board.Fields). Mirrors the state
// mutators: granular ops, atomic under s.mu, with a key cascade across
// issue.Fields maps on rename/delete so no issue is left referencing a
// field the schema no longer knows (which Update's ValidateFieldValues
// would otherwise reject).
// ---------------------------------------------------------------------------

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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "field_add", "field": f.Name},
	})
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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "field_update", "field": name},
	})
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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "field_reorder"},
	})
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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "view_save", "view": v.Name},
	})
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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "view_delete", "view": name},
	})
}

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
	if err := s.setBoardLocked(next); err != nil {
		return err
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtBoardUpdated,
		Payload: map[string]any{"op": "state_reorder"},
	})
}

// Create persists a new issue. The State must be one of the configured
// board states; if empty, the first state is used. ID is generated if
// missing.
func (s *Store) Create(in Issue) (created *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Create", &err)

	if in.Title == "" {
		return nil, errors.New("issue: title required")
	}
	if in.State == "" {
		in.State = s.board.States[0].Name
	}
	if s.board.StateByName(in.State) == nil {
		return nil, fmt.Errorf("issue: unknown state %q", in.State)
	}
	if err := s.board.ValidateFieldValues(in.Fields); err != nil {
		return nil, err
	}

	if in.ID == "" {
		in.ID = "native:" + uuid.NewString()
	} else if err := validateIssueID(in.ID); err != nil {
		return nil, err
	}
	if _, exists := s.index[in.ID]; exists {
		return nil, fmt.Errorf("issue: id %q already exists", in.ID)
	}
	now := time.Now().UTC()
	in.CreatedAt = now
	in.UpdatedAt = now
	if err := s.writeIssueLocked(&in); err != nil {
		return nil, err
	}
	s.index[in.ID] = cloneIssue(&in)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueCreated,
		IssueID: in.ID,
		Payload: map[string]any{"state": in.State, "title": in.Title},
	}); err != nil {
		return nil, err
	}
	clone := in
	return &clone, nil
}

// recoverMutator wraps a Store mutator in defer-recover. A panic
// during disk I/O, index mutation, or event emission would otherwise
// take down the dispatcher process; here we reload the index from disk
// so any partially-applied in-memory state is replaced with the
// canonical on-disk view, and surface the panic as a returned error
// so the caller (HTTP handler, MCP tool, etc.) reports it instead of
// crashing.
func (s *Store) recoverMutator(name string, err *error) {
	r := recover()
	if r == nil {
		return
	}
	// Best-effort: drop the in-memory index and rebuild from disk so
	// later reads don't see a half-mutated state. A reload failure
	// here is folded into the returned error so the caller knows the
	// store is in a degraded state and the process should probably
	// be restarted to recover.
	s.index = map[string]*Issue{}
	if reloadErr := s.populateIndex(); reloadErr != nil {
		*err = fmt.Errorf("native store: %s panicked (%v) and index reload failed (%v) — restart recommended", name, r, reloadErr)
		return
	}
	*err = fmt.Errorf("native store: %s panicked: %v", name, r)
}

// Get returns a defensive copy of the issue with the given ID.
func (s *Store) Get(id string) (*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if iss, ok := s.index[id]; ok {
		return cloneIssue(iss), nil
	}
	return nil, tracker.ErrNotFound
}

// ListFilter constrains the result of List. Zero-value fields don't filter.
type ListFilter struct {
	States   []string
	Labels   []string
	Assignee string
	Claimed  *bool
}

// List returns defensive copies of issues matching the filter, sorted
// by priority desc, then created_at asc. Walks the in-memory index —
// no filesystem I/O on the hot path.
//
// Note: every match incurs a full cloneIssue under the store mutex.
// At the current sub-1k-issue usage this is invisible; once a board
// holds more than ~1k open issues the dispatcher poller (which calls
// List on every tick) starts to contend with mutators. The cheap
// remediation is to filter-and-count first under the read lock, drop
// the lock, then clone outside it — defer until benchmarks show real
// contention.
func (s *Store) List(filter ListFilter) ([]*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Issue, 0, len(s.index))
	for _, iss := range s.index {
		if !filter.match(iss) {
			continue
		}
		out = append(out, cloneIssue(iss))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// cloneIssue returns a deep copy of an issue so callers receive their
// own mutable instance and cannot mutate the in-memory cache.
func cloneIssue(in *Issue) *Issue {
	c := *in
	if in.Labels != nil {
		c.Labels = append([]string(nil), in.Labels...)
	}
	if in.Blockers != nil {
		c.Blockers = append([]string(nil), in.Blockers...)
	}
	if in.Fields != nil {
		c.Fields = make(map[string]any, len(in.Fields))
		for k, v := range in.Fields {
			c.Fields[k] = v
		}
	}
	if in.BotArgs != nil {
		c.BotArgs = make(map[string]string, len(in.BotArgs))
		for k, v := range in.BotArgs {
			c.BotArgs[k] = v
		}
	}
	if in.Comments != nil {
		c.Comments = append([]Comment(nil), in.Comments...)
	}
	return &c
}

func (f ListFilter) match(iss *Issue) bool {
	if len(f.States) > 0 && !slices.Contains(f.States, iss.State) {
		return false
	}
	for _, want := range f.Labels {
		if !slices.Contains(iss.Labels, want) {
			return false
		}
	}
	if f.Assignee != "" && iss.Assignee != f.Assignee {
		return false
	}
	if f.Claimed != nil {
		hasClaim := iss.Claim != ""
		if *f.Claimed != hasClaim {
			return false
		}
	}
	return true
}

// Patch describes a partial update to an issue. Pointer fields are nil
// when the corresponding field is not being changed.
type Patch struct {
	Title    *string
	Body     *string
	Labels   *[]string
	Priority *int
	Assignee *string
	Blockers *[]string
	// Fields is merged into the issue's Fields. A nil value deletes the key.
	Fields map[string]any
	// Bot, when non-nil, sets the per-ticket bot override (empty string
	// clears it). The dispatcher resolves it to a workflow at launch.
	Bot *string
	// BotArgs, when non-nil, replaces the issue's bot args wholesale
	// (a nil map deletes; an empty map clears with no entries). This
	// mirrors how Labels and Blockers are handled — the entire
	// collection swaps. Per-key partial updates aren't useful because
	// the studio always sends the full form state.
	BotArgs *map[string]string
}

// Update applies the patch and emits an issue_updated event with the
// list of changed top-level fields. State changes are not supported here;
// use SetState.
func (s *Store) Update(id string, p Patch) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Update", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, err
	}
	changed := []string{}
	if p.Title != nil && *p.Title != iss.Title {
		iss.Title = *p.Title
		changed = append(changed, "title")
	}
	if p.Body != nil && *p.Body != iss.Body {
		iss.Body = *p.Body
		changed = append(changed, "body")
	}
	if p.Labels != nil {
		iss.Labels = append([]string(nil), (*p.Labels)...)
		changed = append(changed, "labels")
	}
	if p.Priority != nil && *p.Priority != iss.Priority {
		iss.Priority = *p.Priority
		changed = append(changed, "priority")
	}
	if p.Assignee != nil && *p.Assignee != iss.Assignee {
		iss.Assignee = *p.Assignee
		changed = append(changed, "assignee")
	}
	if p.Blockers != nil {
		iss.Blockers = append([]string(nil), (*p.Blockers)...)
		changed = append(changed, "blockers")
	}
	if len(p.Fields) > 0 {
		merged := map[string]any{}
		for k, v := range iss.Fields {
			merged[k] = v
		}
		for k, v := range p.Fields {
			if v == nil {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if err := s.board.ValidateFieldValues(merged); err != nil {
			return nil, err
		}
		iss.Fields = merged
		changed = append(changed, "fields")
	}
	if p.Bot != nil && *p.Bot != iss.Bot {
		iss.Bot = *p.Bot
		changed = append(changed, "bot")
	}
	if p.BotArgs != nil {
		var next map[string]string
		if len(*p.BotArgs) > 0 {
			next = make(map[string]string, len(*p.BotArgs))
			for k, v := range *p.BotArgs {
				next[k] = v
			}
		}
		iss.BotArgs = next
		changed = append(changed, "bot_args")
	}
	if len(changed) == 0 {
		return iss, nil
	}
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueUpdated,
		IssueID: iss.ID,
		Payload: map[string]any{"changed": changed},
	}); err != nil {
		return nil, err
	}
	return iss, nil
}

// SetState transitions an issue, validating against the board. Returns
// tracker.ErrTransitionRejected if newState is unknown.
func (s *Store) SetState(id, newState string) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetState", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, err
	}
	if s.board.StateByName(newState) == nil {
		return nil, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, newState)
	}
	if iss.State == newState {
		return iss, nil
	}
	old := iss.State
	iss.State = newState
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueState,
		IssueID: iss.ID,
		Payload: map[string]any{"from": old, "to": newState},
	}); err != nil {
		return nil, err
	}
	return iss, nil
}

// Delete removes the issue file and emits an issue_deleted event.
func (s *Store) Delete(id string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Delete", &err)
	if _, ok := s.index[id]; !ok {
		return tracker.ErrNotFound
	}
	if err := os.Remove(s.issuePath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("native store: remove issue: %w", err)
	}
	delete(s.index, id)
	return s.emitPostCommitEvent(Event{Type: EvtIssueDeleted, IssueID: id})
}

// Claim sets the claim marker. Returns tracker.ErrClaimConflict if the
// issue is already claimed by a different marker. Idempotent for the
// same marker.
func (s *Store) Claim(id, marker string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Claim", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	if iss.Claim != "" && iss.Claim != marker {
		return fmt.Errorf("%w: held by %s", tracker.ErrClaimConflict, iss.Claim)
	}
	if iss.Claim == marker {
		return nil
	}
	iss.Claim = marker
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type: EvtIssueClaimed, IssueID: id,
		Payload: map[string]any{"marker": marker},
	})
}

// SetLastRun stamps the most recent dispatcher-spawned run that
// processed the issue onto its record. Idempotent — passing the same
// runID + workdir as the current values is a no-op (no write, no
// event). Empty strings are written as-is so the operator can clear
// the stamp if needed.
//
// The dispatcher calls this on every finishRun (success or failure)
// so the studio's IssueModal can always link back to the most recent
// run that touched the issue.
func (s *Store) SetLastRun(id, runID, workdir string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetLastRun", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	if iss.LastRunID == runID && iss.LastWorkdir == workdir {
		return nil
	}
	iss.LastRunID = runID
	iss.LastWorkdir = workdir
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type:    EvtIssueLastRun,
		IssueID: id,
		Payload: map[string]any{"run_id": runID, "workdir": workdir},
	})
}

// AddComment appends a note to the issue's discussion thread and returns
// the updated issue plus the created comment. Author is a free-form
// display name; body must be non-empty. The append is persisted to
// issues/<id>.json and an EvtIssueComment record is emitted so external
// tailers (studio, webhook bridge) observe new comments.
func (s *Store) AddComment(id, author, body string) (updated *Issue, comment *Comment, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("AddComment", &err)
	if strings.TrimSpace(body) == "" {
		return nil, nil, errors.New("comment: body required")
	}
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, nil, err
	}
	c := Comment{
		ID:        uuid.NewString(),
		Author:    author,
		Body:      body,
		CreatedAt: time.Now().UTC(),
	}
	iss.Comments = append(iss.Comments, c)
	iss.UpdatedAt = c.CreatedAt
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, nil, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueComment,
		IssueID: id,
		Payload: map[string]any{"comment_id": c.ID, "author": author},
	}); err != nil {
		return nil, nil, err
	}
	return cloneIssue(iss), &c, nil
}

// Release clears the claim if it matches the given marker. Releasing an
// already-unclaimed issue is a no-op.
func (s *Store) Release(id, marker string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Release", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	if iss.Claim == "" {
		return nil
	}
	if iss.Claim != marker {
		return fmt.Errorf("%w: held by %s", tracker.ErrClaimConflict, iss.Claim)
	}
	iss.Claim = ""
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type: EvtIssueReleased, IssueID: id,
		Payload: map[string]any{"marker": marker},
	})
}

// Resolve returns the full issue ID matching the given prefix. The
// prefix may be the bare UUID (without the "native:" scheme) or the
// full ID. Returns tracker.ErrNotFound if no issue matches and a
// distinct error if multiple match. Walks the in-memory index, so
// O(N) over distinct issues with no filesystem I/O.
func (s *Store) Resolve(prefix string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := prefix
	if !strings.HasPrefix(prefix, "native:") {
		want = "native:" + prefix
	}
	var matches []string
	for id := range s.index {
		if id == want || strings.HasPrefix(id, want) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", tracker.ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("native store: ambiguous prefix %q matches %d issues", prefix, len(matches))
	}
}

// ScanEvents streams events from events.jsonl through visit, in file
// order. Returning false from visit stops the scan. Safe to call
// concurrently with writes — the file is append-only.
func (s *Store) ScanEvents(visit func(*Event) bool) error {
	p := filepath.Join(s.root, eventsFile)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 10*1024*1024)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if !visit(&e) {
			return nil
		}
	}
	return scanner.Err()
}

// ---------------------------------------------------------------------------
// internals — must be called with s.mu held
// ---------------------------------------------------------------------------

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
	// Pre-upgrade stores predate the `inbox` state. Prepend it once so
	// bots emitting findings (which target inbox) work after upgrade
	// without manual board.json edits. Idempotent: skipped when inbox
	// is already present (operator-customised boards keep their order).
	if s.board.StateByName(StateInbox) == nil {
		s.board.States = append([]State{{Name: StateInbox, Display: "Inbox"}}, s.board.States...)
		if err := s.writeBoardLocked(); err != nil {
			return fmt.Errorf("native store: persist inbox upgrade: %w", err)
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

func (s *Store) writeIssueLocked(iss *Issue) error {
	if err := validateIssueID(iss.ID); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.root, issuesDir), dirPerm); err != nil {
		return err
	}
	data, err := json.MarshalIndent(iss, "", "  ")
	if err != nil {
		return fmt.Errorf("native store: marshal issue: %w", err)
	}
	p := s.issuePath(iss.ID)
	if err := store.WriteFileAtomic(p, data, filePerm); err != nil {
		return fmt.Errorf("native store: write issue: %w", err)
	}
	return nil
}

// LabelUsage is one row of the AggregateLabels result.
type LabelUsage struct {
	Label      string `json:"label"`
	Count      int    `json:"count"`
	LastUsedAt string `json:"last_used_at,omitempty"` // RFC3339; empty when no timestamp survived the scan.
}

// AggregateLabels walks the in-memory index and reduces (label →
// count, max(updated_at)). Sorted by count desc, label asc for
// deterministic output. Used by the REST /labels endpoint, the
// boardops list_labels MCP tool, and the studio's label-picker.
func (s *Store) AggregateLabels() []LabelUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	type acc struct {
		count int
		last  string
	}
	agg := map[string]*acc{}
	for _, iss := range s.index {
		stamp := iss.UpdatedAt.UTC().Format(time.RFC3339)
		for _, lbl := range iss.Labels {
			if lbl == "" {
				continue
			}
			cur, ok := agg[lbl]
			if !ok {
				agg[lbl] = &acc{count: 1, last: stamp}
				continue
			}
			cur.count++
			if stamp > cur.last {
				cur.last = stamp
			}
		}
	}
	out := make([]LabelUsage, 0, len(agg))
	for lbl, a := range agg {
		out = append(out, LabelUsage{Label: lbl, Count: a.count, LastUsedAt: a.last})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// RenameLabel rewrites every occurrence of `from` to `to` across all
// issues. Returns the number of issues touched. No-op when from == to,
// returns ErrLabelEmpty if either side is the empty string. Idempotent:
// running it twice on the same input touches zero issues the second
// time. Emits one issue_updated event per touched issue (labels
// changed). The whole pass holds the store mutex so concurrent writers
// can't race; for boards with thousands of issues that briefly stalls
// other mutators, which is the acceptable trade-off for atomic-ish
// vocabulary management.
func (s *Store) RenameLabel(from, to string) (int, error) {
	if from == "" || to == "" {
		return 0, ErrLabelEmpty
	}
	if from == to {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLabelRewriteLocked(func(labels []string) ([]string, bool) {
		out := make([]string, 0, len(labels))
		changed := false
		seenTo := false
		for _, l := range labels {
			if l == to {
				seenTo = true
			}
		}
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
	}, EvtLabelRename, map[string]any{"from": from, "to": to})
}

// MergeLabels is rename's near-twin: every issue carrying `from` ends
// up carrying `to` (and no longer `from`). Differs from Rename only in
// the audit event payload — emitted as "label_merge" so an operator
// reviewing events.jsonl can tell whether the operation was a typo fix
// (rename) or a vocabulary consolidation (merge). Functionally
// equivalent today.
func (s *Store) MergeLabels(from, to string) (int, error) {
	if from == "" || to == "" {
		return 0, ErrLabelEmpty
	}
	if from == to {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLabelRewriteLocked(func(labels []string) ([]string, bool) {
		out := make([]string, 0, len(labels))
		changed := false
		seenTo := false
		for _, l := range labels {
			if l == to {
				seenTo = true
			}
		}
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
	}, EvtLabelMerge, map[string]any{"from": from, "to": to})
}

// DeleteLabel strips `label` from every issue that carries it. Returns
// the count of issues touched.
func (s *Store) DeleteLabel(label string) (int, error) {
	if label == "" {
		return 0, ErrLabelEmpty
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLabelRewriteLocked(func(labels []string) ([]string, bool) {
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
	}, EvtLabelDelete, map[string]any{"label": label})
}

// ErrLabelEmpty is returned when a label vocabulary op is called with
// an empty label name (RenameLabel, MergeLabels, DeleteLabel).
var ErrLabelEmpty = errors.New("native store: label name cannot be empty")

// applyLabelRewriteLocked is the shared scan-and-rewrite loop for
// Rename/Merge/Delete. The caller already holds s.mu. transform
// receives an issue's current label slice and returns (new slice, did
// anything change?). On change, the new slice replaces the issue's
// Labels, the file is rewritten, the index is refreshed, and an event
// is appended. Returns the number of issues touched.
func (s *Store) applyLabelRewriteLocked(
	transform func(labels []string) ([]string, bool),
	eventType EventType,
	payload map[string]any,
) (int, error) {
	touched := 0
	for id, iss := range s.index {
		newLabels, changed := transform(iss.Labels)
		if !changed {
			continue
		}
		// Clone before mutating: index entries are shared with reader
		// goroutines holding earlier defensive copies. The writer path
		// always clones before publishing the new value to the index.
		next := cloneIssue(iss)
		next.Labels = newLabels
		next.UpdatedAt = time.Now().UTC()
		if err := s.writeIssueLocked(next); err != nil {
			return touched, fmt.Errorf("native store: write %s during %s: %w", id, eventType, err)
		}
		s.index[id] = next
		evtPayload := map[string]any{"issue_id": id}
		for k, v := range payload {
			evtPayload[k] = v
		}
		if err := s.emitPostCommitEvent(Event{
			Type:    eventType,
			IssueID: id,
			Payload: evtPayload,
		}); err != nil {
			return touched, err
		}
		touched++
	}
	return touched, nil
}

// readIssueLocked returns a defensive copy of the indexed issue.
// Reads after init always hit the in-memory cache; the on-disk files
// stay authoritative for crash recovery via populateIndex at NewStore.
func (s *Store) readIssueLocked(id string) (*Issue, error) {
	if iss, ok := s.index[id]; ok {
		return cloneIssue(iss), nil
	}
	return nil, tracker.ErrNotFound
}

func (s *Store) issuePath(id string) string {
	return filepath.Join(s.root, issuesDir, encodeID(id)+".json")
}

func validateIssueID(id string) error {
	raw, ok := strings.CutPrefix(id, "native:")
	if !ok || raw == "" {
		return fmt.Errorf("native store: invalid issue id %q", id)
	}
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed.String() != raw {
		return fmt.Errorf("native store: invalid issue id %q", id)
	}
	return nil
}

// writeEventLineLocked formats an event and appends a single line to
// events.jsonl with fsync. Increments s.seq on success.
func (s *Store) writeEventLineLocked(evt Event) error {
	evt.Seq = s.seq
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	line, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("native store: marshal event: %w", err)
	}
	line = append(line, '\n')
	p := filepath.Join(s.root, eventsFile)
	// O_RDWR (not O_WRONLY) so the torn-tail repair below can ReadAt the
	// final byte (ReadAt on a write-only fd returns EBADF). O_APPEND
	// still forces every write to EOF, so append semantics are unchanged.
	// Mirrors the runs-store hardening (a79ffa76).
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("native store: open events: %w", err)
	}
	defer f.Close()

	// Repair a torn final line left by a prior crash (a partial JSONL
	// record with no trailing newline). Without this the next append
	// concatenates onto the torn bytes, merging two records into one
	// corrupt line — so a tailer skips it and loses BOTH the torn tail
	// AND this event. Runs under s.mu, so seq seeding + repair are atomic
	// within this process.
	info, statErr := f.Stat()
	var preSize int64
	if statErr == nil {
		preSize = info.Size()
	}
	if statErr == nil && preSize > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, preSize-1); err != nil {
			return fmt.Errorf("native store: inspect events tail: %w", err)
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				return fmt.Errorf("native store: separate torn event tail: %w", err)
			}
			preSize++
		}
	}

	n, writeErr := f.Write(line)
	if writeErr != nil || n != len(line) {
		// Roll back a short write (typically ENOSPC mid-line) to the
		// captured size so the file stays JSONL-clean. Only safe when
		// Stat succeeded; otherwise leaving the partial line is the
		// lesser evil (a guessed offset could drop prior good lines).
		if statErr == nil {
			_ = f.Truncate(preSize)
		}
		if writeErr != nil {
			return fmt.Errorf("native store: write event: %w", writeErr)
		}
		return fmt.Errorf("native store: short write on event (wrote %d of %d bytes)", n, len(line))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("native store: sync event: %w", err)
	}
	s.seq++
	return nil
}

// appendEventLocked drains any previously-buffered events whose append
// failed before writing the new one. A transient fsync hiccup that
// previously left a gap in events.jsonl now self-heals on the next
// successful operation — external tailers see every transition in
// the correct seq order, just delayed.
func (s *Store) appendEventLocked(evt Event) error {
	if len(s.pendingEvents) > 0 {
		drained := s.pendingEvents
		s.pendingEvents = nil
		for i, p := range drained {
			if err := s.writeEventLineLocked(p); err != nil {
				// Still flaky — re-buffer the failed entry, every
				// entry after it, and the new one. The caller can
				// retry; state on disk is consistent because the
				// issue file was already updated by the mutator.
				s.pendingEvents = append(s.pendingEvents, drained[i:]...)
				s.pendingEvents = append(s.pendingEvents, evt)
				return err
			}
		}
	}
	if err := s.writeEventLineLocked(evt); err != nil {
		s.pendingEvents = append(s.pendingEvents, evt)
		return err
	}
	return nil
}

// emitPostCommitEvent appends an event after a successful issue write.
// The issue file is the authoritative source for state recovery
// (populateIndex reads them at startup, not events.jsonl), so an event
// write failure here doesn't corrupt state. The buffered-replay path
// in appendEventLocked ensures external tailers still see every
// transition once the filesystem cooperates again.
func (s *Store) emitPostCommitEvent(evt Event) error {
	if err := s.appendEventLocked(evt); err != nil {
		// The issue file + in-memory index are already updated (the
		// authoritative state) and appendEventLocked buffered the event in
		// pendingEvents for replay. events.jsonl is non-authoritative, so we
		// must NOT propagate this as a mutation failure: a caller that maps it
		// to a 4xx/5xx for a write that actually succeeded would, on retry,
		// create a duplicate issue (Create generates a fresh UUID) or re-emit
		// the mutation. Warn and swallow. (Always returns nil; the error
		// return is kept only for the call-site signatures.)
		fmt.Fprintf(os.Stderr, "native store: WARN event log fsync failed; buffered for replay on next operation: %v\n", err)
	}
	return nil
}

// Colon is illegal in NTFS filenames; encode "native:<uuid>" → "native__<uuid>"
// for safe cross-platform storage. UUIDs never contain a literal "__".
func encodeID(id string) string { return strings.ReplaceAll(id, ":", "__") }
func decodeID(s string) string  { return strings.ReplaceAll(s, "__", ":") }

func cloneBoard(b *Board) *Board {
	c := *b
	c.States = append([]State(nil), b.States...)
	c.Fields = append([]Field(nil), b.Fields...)
	c.Views = append([]View(nil), b.Views...)
	return &c
}
