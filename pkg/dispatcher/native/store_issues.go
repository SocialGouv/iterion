package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/google/uuid"
)

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
	if in.External != nil {
		ext := *in.External
		c.External = &ext
	}
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
	if in.Runs != nil {
		c.Runs = append([]RunRef(nil), in.Runs...)
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
	// External, when non-nil, sets the card's forge linkage (the
	// forge→board sync worker refreshes url/state; push-to-forge stamps a
	// previously-unlinked card). A nil pointer leaves the existing link.
	External *ExternalRef
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
	if p.External != nil {
		ext := *p.External
		iss.External = &ext
		changed = append(changed, "external")
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

// Colon is illegal in NTFS filenames; encode "native:<uuid>" → "native__<uuid>"
// for safe cross-platform storage. UUIDs never contain a literal "__".
func encodeID(id string) string { return strings.ReplaceAll(id, ":", "__") }
func decodeID(s string) string  { return strings.ReplaceAll(s, "__", ":") }
