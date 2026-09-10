package native

import (
	"slices"
	"strings"
	"time"
)

// AdjustLabelList is the ONE definition of a relative label change,
// shared by the FS twin (in its critical section) and the Mongo twin
// (which mirrors it in an aggregation pipeline and uses it to describe
// what that pipeline wrote). Existing labels keep their order; adds not
// already present are appended in request order, deduplicated; removes
// are applied last, so a label named on both sides is removed. Blank
// entries are ignored. added/removed report the actual delta — both
// empty means nothing changed and nothing should be written.
func AdjustLabelList(labels, add, remove []string) (out, added, removed []string) {
	drop := map[string]bool{}
	for _, l := range remove {
		if l = strings.TrimSpace(l); l != "" {
			drop[l] = true
		}
	}
	out = make([]string, 0, len(labels)+len(add))
	present := map[string]bool{}
	for _, l := range labels {
		if drop[l] {
			removed = append(removed, l)
			continue
		}
		out = append(out, l)
		present[l] = true
	}
	for _, l := range add {
		l = strings.TrimSpace(l)
		if l == "" || present[l] || drop[l] {
			continue
		}
		out = append(out, l)
		added = append(added, l)
		present[l] = true
	}
	return out, added, removed
}

// CleanLabels trims a request's labels and drops blanks and duplicates
// (order kept) — the normalisation both twins apply to add/remove input.
func CleanLabels(in []string) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// AdjustLabels is the relative label write behind the add_labels /
// remove_labels board operations (see BoardStore). One critical section:
// the card as it IS is what the delta applies to, never a snapshot the
// caller took earlier — a one-shot trigger label consumed between a
// bot's read and its write stays consumed.
func (s *Store) AdjustLabels(id string, add, remove []string) (updated *Issue, changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("AdjustLabels", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, false, err
	}
	out, added, removed := AdjustLabelList(iss.Labels, add, remove)
	if len(added) == 0 && len(removed) == 0 {
		return iss, false, nil
	}
	iss.Labels = out
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, false, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueUpdated,
		IssueID: iss.ID,
		Payload: LabelsAdjustedPayload(added, removed),
	}); err != nil {
		return nil, false, err
	}
	return iss, true, nil
}

// LabelsAdjustedPayload is the event body a relative label change emits
// on both twins: the same `changed: [labels]` shape Update emits (the
// trigger spine's card.updated reads it the same way), plus the delta
// for the audit tail.
func LabelsAdjustedPayload(added, removed []string) map[string]any {
	p := map[string]any{"changed": []string{"labels"}}
	if len(added) > 0 {
		p["labels_added"] = append([]string(nil), added...)
	}
	if len(removed) > 0 {
		p["labels_removed"] = append([]string(nil), removed...)
	}
	return p
}
