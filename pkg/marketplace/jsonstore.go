package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/store"
)

// jsonStoreFile is the on-disk filename inside the configured dir. The
// file holds the full array of entries; mutations are serialised
// through a single RWMutex and written atomically via
// store.WriteFileAtomic, mirroring how pkg/dispatcher/native writes
// board.json.
const jsonStoreFile = "marketplace.json"

const (
	dirPerm  fs.FileMode = 0o755
	filePerm fs.FileMode = 0o644
)

// JSONStore is the file-backed Store, used in self-host / local mode.
// All entries live in <dir>/marketplace.json; the file is loaded once
// at NewJSONStore and rewritten atomically on every mutation. A single
// flat file is fine at the scale this serves (tens, maybe hundreds of
// entries) — the marketplace is not the dispatcher hot path.
type JSONStore struct {
	dir string

	mu      sync.RWMutex
	entries map[string]Entry // slug -> entry
}

// NewJSONStore opens (or initialises) the JSON-backed Store at dir. The
// directory is created if missing; a missing marketplace.json is
// treated as an empty registry. A malformed file is a hard error — the
// operator must repair it (or delete it) before the server starts.
func NewJSONStore(dir string) (*JSONStore, error) {
	if dir == "" {
		return nil, errors.New("marketplace: store dir required")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("marketplace: mkdir: %w", err)
	}
	s := &JSONStore{dir: dir, entries: map[string]Entry{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *JSONStore) path() string { return filepath.Join(s.dir, jsonStoreFile) }

func (s *JSONStore) load() error {
	data, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("marketplace: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var arr []Entry
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("marketplace: parse %s: %w", s.path(), err)
	}
	for _, e := range arr {
		if e.Slug == "" {
			continue
		}
		s.entries[e.Slug] = e
	}
	return nil
}

// writeLocked serialises the in-memory map to disk atomically. Caller
// must hold s.mu in exclusive mode.
func (s *JSONStore) writeLocked() error {
	arr := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		arr = append(arr, e)
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].Slug < arr[j].Slug })
	data, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return fmt.Errorf("marketplace: marshal: %w", err)
	}
	if err := store.WriteFileAtomic(s.path(), data, filePerm); err != nil {
		return fmt.Errorf("marketplace: write: %w", err)
	}
	return nil
}

// List returns every entry matching q, sorted by Installs desc, then
// Slug asc for deterministic output. A zero-value Query returns every
// entry.
func (s *JSONStore) List(_ context.Context, q Query) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text := strings.ToLower(strings.TrimSpace(q.Text))
	tag := strings.ToLower(strings.TrimSpace(q.Tag))
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if !matchEntry(e, text, tag) {
			continue
		}
		if q.Kind != "" && EffectiveKind(e) != q.Kind {
			continue
		}
		if !Visible(e, q.Viewer) {
			continue
		}
		out = append(out, cloneEntry(e))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Installs != out[j].Installs {
			return out[i].Installs > out[j].Installs
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

func (s *JSONStore) Get(_ context.Context, slug string) (*Entry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[slug]
	if !ok {
		return nil, false, nil
	}
	c := cloneEntry(e)
	return &c, true, nil
}

// Upsert inserts or replaces an entry keyed by Slug. Install counts on
// a re-submitted entry are preserved when the caller's Installs is 0
// (the common submit-form case), so a refresh doesn't reset popularity.
func (s *JSONStore) Upsert(_ context.Context, e Entry) error {
	if strings.TrimSpace(e.Slug) == "" {
		return errors.New("marketplace: slug required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.entries[e.Slug]; ok {
		if e.Installs == 0 {
			e.Installs = prev.Installs
		}
		if e.CreatedAt == "" {
			e.CreatedAt = prev.CreatedAt
		}
	}
	s.entries[e.Slug] = e
	return s.writeLocked()
}

// IncrementInstalls bumps the install counter for slug. Missing slug
// returns an error.
func (s *JSONStore) IncrementInstalls(_ context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[slug]
	if !ok {
		return fmt.Errorf("marketplace: entry %q not found", slug)
	}
	e.Installs++
	s.entries[slug] = e
	return s.writeLocked()
}

// SetStatus transitions slug's moderation status, optionally guarded by
// expect (CAS). The review metadata is stamped onto the entry. In local
// single-tenant mode the moderation handlers don't call this (submits
// auto-approve), but the method is here so the interface is satisfied
// and tests can exercise it.
func (s *JSONStore) SetStatus(_ context.Context, slug string, expect, next Status, review Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[slug]
	if !ok {
		return fmt.Errorf("marketplace: entry %q not found", slug)
	}
	if expect != "" && EffectiveStatus(e) != expect {
		return ErrStatusConflict
	}
	e.Status = next
	e.ReviewedBy = review.By
	e.ReviewedAt = review.At
	if next == StatusRejected {
		e.RejectReason = review.Reason
	} else {
		e.RejectReason = ""
	}
	s.entries[slug] = e
	return s.writeLocked()
}

// ListForModeration returns entries in the requested moderation states
// (default {StatusPending}), scoped to q.OrgIDs unless q.All is set.
// Results are sorted by CreatedAt asc (oldest pending first) then Slug.
func (s *JSONStore) ListForModeration(_ context.Context, q ModerationQuery) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = []Status{StatusPending}
	}
	want := make(map[Status]struct{}, len(statuses))
	for _, st := range statuses {
		want[st] = struct{}{}
	}
	out := make([]Entry, 0)
	for _, e := range s.entries {
		if _, ok := want[EffectiveStatus(e)]; !ok {
			continue
		}
		if !q.All && !orgInSet(e.OrgID, q.OrgIDs) {
			continue
		}
		out = append(out, cloneEntry(e))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// Delete removes slug. A missing slug is a no-op (not an error).
func (s *JSONStore) Delete(_ context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[slug]; !ok {
		return nil
	}
	delete(s.entries, slug)
	return s.writeLocked()
}

// orgInSet reports whether org is one of ids. An empty org never matches
// a non-empty set (org-scoped entries with no owner stay out of scoped
// moderation queues).
func orgInSet(org string, ids []string) bool {
	for _, id := range ids {
		if id == org {
			return true
		}
	}
	return false
}

// matchEntry returns true when e satisfies the (lowercased) text and
// tag filters. text matches Slug/Name/DisplayName/Description/Author or
// any tag; tag requires an exact case-insensitive tag match.
func matchEntry(e Entry, text, tag string) bool {
	if tag != "" {
		found := false
		for _, t := range e.Tags {
			if strings.EqualFold(t, tag) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if text != "" {
		hay := strings.ToLower(strings.Join([]string{
			e.Slug, e.Name, e.DisplayName, e.Description, e.Author,
		}, " "))
		if !strings.Contains(hay, text) {
			for _, t := range e.Tags {
				if strings.Contains(strings.ToLower(t), text) {
					return true
				}
			}
			return false
		}
	}
	return true
}

// cloneEntry returns a deep copy so List/Get callers can mutate the
// result without racing the in-memory map.
func cloneEntry(in Entry) Entry {
	out := in
	if in.Tags != nil {
		out.Tags = append([]string(nil), in.Tags...)
	}
	if in.Presets != nil {
		out.Presets = make([]EntryPreset, len(in.Presets))
		for i, p := range in.Presets {
			ep := p
			if p.Skills != nil {
				ep.Skills = append([]string(nil), p.Skills...)
			}
			out.Presets[i] = ep
		}
	}
	return out
}
