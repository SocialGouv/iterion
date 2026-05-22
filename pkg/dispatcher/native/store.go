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

	// pendingEvents buffers events whose appendEventLocked call
	// returned an error (transient fsync failure, NFS hiccup). Every
	// subsequent successful event flush drains the buffer first so a
	// downstream tailer eventually sees every state transition. State
	// recovery via populateIndex doesn't depend on events.jsonl, so
	// holding the buffer in memory is safe across the failure window.
	pendingEvents []Event
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
	return s, nil
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
	return &c
}

func (f ListFilter) match(iss *Issue) bool {
	if len(f.States) > 0 && !containsString(f.States, iss.State) {
		return false
	}
	for _, want := range f.Labels {
		if !containsString(iss.Labels, want) {
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
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("native store: open events: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("native store: write event: %w", err)
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
		fmt.Fprintf(os.Stderr, "native store: WARN event log fsync failed; buffered for replay on next operation: %v\n", err)
		return err
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
	return &c
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
