package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tag limits. Operators tag runs with short labels ('release', 'flaky',
// 'customer-x') to filter and group them in the studio; these bounds keep
// the list a UI affordance, not a free-text blob.
const (
	// MaxTagLen is the maximum length (in bytes) of a single run tag.
	MaxTagLen = 32
	// MaxTagsPerRun is the maximum number of distinct tags on one run.
	MaxTagsPerRun = 20
)

// RunTagStore is an optional interface implemented by stores that persist
// a run's operator-assigned tags — the short filter/group labels shown as
// chips in the studio run header. It is a whole-list overwrite (SetRunTags
// replaces the full set), mirroring RunGitMetaStore's single-snapshot-per-
// run shape rather than the append-only PlanStore. Both the filesystem
// store (runs/<id>/tags.json) and the Mongo store (run_tags collection,
// one doc per run) satisfy it, so tags round-trip identically in local and
// cloud mode. Callers MUST nil-check via AsRunTagStore.
type RunTagStore interface {
	// SetRunTags replaces the run's full tag set with tags (already
	// normalized by the caller — see NormalizeTags). Passing an empty
	// slice clears the tags. Idempotent.
	SetRunTags(ctx context.Context, runID string, tags []string) error
	// GetRunTags returns the run's tags in stored order, or an empty
	// slice when the run has none (never nil-vs-empty ambiguity for the
	// HTTP surface to worry about).
	GetRunTags(ctx context.Context, runID string) ([]string, error)
}

// AsRunTagStore returns s as RunTagStore when the backend persists run
// tags, or nil otherwise. Both the filesystem and Mongo stores satisfy it.
func AsRunTagStore(s RunStore) RunTagStore {
	if s == nil {
		return nil
	}
	t, _ := s.(RunTagStore)
	return t
}

// NormalizeTags cleans and validates a caller-supplied tag list: each tag
// is trimmed, empty tags are dropped, and the remainder is deduplicated
// preserving first-seen order (case-sensitive — 'Release' and 'release'
// are distinct). It returns an error when any tag exceeds MaxTagLen bytes
// or the deduplicated set exceeds MaxTagsPerRun, so the PUT handler can map
// that to a 400 rather than silently truncating. A nil/empty input returns
// an empty (non-nil) slice — "clear the tags".
func NormalizeTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len(t) > MaxTagLen {
			return nil, fmt.Errorf("tag %q exceeds %d characters", t, MaxTagLen)
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) > MaxTagsPerRun {
		return nil, fmt.Errorf("too many tags: %d (max %d)", len(out), MaxTagsPerRun)
	}
	return out, nil
}

// runTagsFile is the on-disk wire shape of runs/<id>/tags.json.
type runTagsFile struct {
	Tags []string `json:"tags"`
}

// tagsPath validates runID and returns <root>/runs/<runID>/tags.json.
func (s *FilesystemRunStore) tagsPath(runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "runs", runID, "tags.json"), nil
}

// SetRunTags implements RunTagStore over runs/<id>/tags.json — a single
// whole-list file rewritten atomically on each save.
func (s *FilesystemRunStore) SetRunTags(_ context.Context, runID string, tags []string) error {
	p, err := s.tagsPath(runID)
	if err != nil {
		return err
	}
	if tags == nil {
		tags = []string{}
	}
	data, err := json.MarshalIndent(runTagsFile{Tags: tags}, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run tags: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run dir for tags: %w", err)
	}
	return writeFileAtomic(p, data, filePerm)
}

// GetRunTags implements RunTagStore: reads runs/<id>/tags.json, returning
// an empty slice when the run never recorded any tags.
func (s *FilesystemRunStore) GetRunTags(_ context.Context, runID string) ([]string, error) {
	p, err := s.tagsPath(runID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("store: read run tags: %w", err)
	}
	var f runTagsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("store: decode run tags: %w", err)
	}
	if f.Tags == nil {
		f.Tags = []string{}
	}
	return f.Tags, nil
}
