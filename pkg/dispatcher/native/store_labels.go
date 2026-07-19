package native

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

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
