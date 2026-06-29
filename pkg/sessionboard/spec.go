// Package sessionboard models the per-run "Session board": a small,
// declarative dashboard the studio renders on a run's Tasks tab. Phase 1
// (the deterministic task-list timeline) lives entirely in the studio
// front-end fed by the existing event stream. This package is Phase 2 —
// the LLM curation layer: a cheap supervisor-style coordinator watches the
// run and emits widget diffs against a persisted Spec, adding session-
// specific semantic widgets (milestone progress, blockers, a narrative
// note, a small chart) the run view does not already provide.
//
// The agent emits a declarative Spec against a fixed widget registry — it
// never emits code — so rendering stays safe, themeable, and consistent.
package sessionboard

import "strings"

// WidgetKind enumerates the renderable widget types. Kept deliberately
// small and generic: every kind shows information the technical run view
// (events / logs / graph / cost / artifacts) does NOT already surface, so
// the board complements rather than duplicates it. The studio holds the
// matching renderer per kind and ignores unknown kinds (forward-compat).
const (
	KindNote      = "note"      // a short natural-language narrative / status line
	KindMetric    = "metric"    // a single labelled value (label + value [+ hint])
	KindChecklist = "checklist" // a list of {text, done} milestone items
	KindProgress  = "progress"  // a labelled progress bar (value / max)
	KindBarChart  = "bar_chart" // labelled numeric series, rendered with Recharts
)

// knownKinds is the validation allowlist; ApplyDecision drops upserts with
// any other kind so a hallucinated kind can't poison the spec.
var knownKinds = map[string]bool{
	KindNote:      true,
	KindMetric:    true,
	KindChecklist: true,
	KindProgress:  true,
	KindBarChart:  true,
}

// Widget is one card on the board. Props carries the kind-specific payload
// (e.g. {"value": 12, "hint": "files"} for a metric); the studio renders
// it per kind. Keeping props as a free map lets new kinds ship without a
// Go schema change — validation that matters (known kind, non-empty id)
// happens in ApplyDecision.
type Widget struct {
	ID    string         `json:"id"`
	Kind  string         `json:"kind"`
	Title string         `json:"title,omitempty"`
	Props map[string]any `json:"props,omitempty"`
}

// Spec is the full board state for one run, persisted as
// runs/<id>/sessionboard.json and streamed to the studio. Version bumps on
// every applied change so the front-end can detect staleness; UpdatedSeq
// records the run event seq the board was last reconciled against.
type Spec struct {
	Version    int      `json:"version"`
	Widgets    []Widget `json:"widgets"`
	UpdatedSeq int64    `json:"updated_seq,omitempty"`
}

// BoardDecision is the curation agent's diff for one wake: widgets to
// upsert (create or replace by id) and ids to remove. Emitting diffs — not
// a full redraw — is what keeps the board convergent instead of thrashing.
type BoardDecision struct {
	Upsert []Widget `json:"upsert,omitempty"`
	Remove []string `json:"remove,omitempty"`
	// Reason is a short rationale, surfaced in logs only.
	Reason string `json:"reason,omitempty"`
	// Done signals the agent is satisfied and should stop curating until a
	// monitored signal fires again.
	Done bool `json:"done,omitempty"`
}

// ApplyDecision folds a decision into a spec, returning the new spec and
// whether anything changed. Removes are applied before upserts. Upserts
// with an unknown kind or empty id are skipped (defensive against a
// hallucinated payload). Version bumps only when the widget set actually
// changed, so a no-op decision doesn't churn the front-end.
func ApplyDecision(spec Spec, dec BoardDecision) (Spec, bool) {
	widgets := append([]Widget(nil), spec.Widgets...)
	changed := false

	if len(dec.Remove) > 0 {
		remove := make(map[string]bool, len(dec.Remove))
		for _, id := range dec.Remove {
			remove[id] = true
		}
		var kept []Widget
		for _, w := range widgets {
			if remove[w.ID] {
				changed = true
				continue
			}
			kept = append(kept, w)
		}
		widgets = kept
	}

	for _, w := range dec.Upsert {
		if !validWidget(w) {
			continue
		}
		if idx := indexByID(widgets, w.ID); idx >= 0 {
			if !widgetEqual(widgets[idx], w) {
				widgets[idx] = w
				changed = true
			}
			continue
		}
		widgets = append(widgets, w)
		changed = true
	}

	if !changed {
		return spec, false
	}
	return Spec{Version: spec.Version + 1, Widgets: widgets, UpdatedSeq: spec.UpdatedSeq}, true
}

func validWidget(w Widget) bool {
	return strings.TrimSpace(w.ID) != "" && knownKinds[w.Kind]
}

func indexByID(widgets []Widget, id string) int {
	for i, w := range widgets {
		if w.ID == id {
			return i
		}
	}
	return -1
}

// widgetEqual reports whether two widgets are identical for change
// detection. Title/kind are compared directly; Props is compared
// structurally so an upsert that re-states the same values is a no-op.
func widgetEqual(a, b Widget) bool {
	if a.ID != b.ID || a.Kind != b.Kind || a.Title != b.Title {
		return false
	}
	return propsEqual(a.Props, b.Props)
}
