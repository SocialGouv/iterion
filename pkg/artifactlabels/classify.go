// Package artifactlabels derives semantic labels for a published
// artifact from the SHAPE of its output data — so an artifact groups
// under "Plans", "Verdicts", etc. in the studio with no authoring effort.
//
// The rules MUST stay in sync with the studio's render-time detection in
// studio/src/components/Runs/ArtifactDiff.tsx (isVerdictShaped /
// isPlanShaped). The studio renders a VerdictCard and a PlanCard
// INDEPENDENTLY (an artifact can be both), so Classify likewise returns
// each matching label independently rather than picking one.
package artifactlabels

const (
	// LabelPlan marks a plan-shaped artifact (a `plan` or `text` prose body).
	LabelPlan = "plan"
	// LabelVerdict marks a reviewer/judge verdict-shaped artifact.
	LabelVerdict = "verdict"
)

// verdictKeys mirrors isVerdictShaped in ArtifactDiff.tsx: an artifact is
// verdict-shaped when it carries ANY of these decision fields.
var verdictKeys = []string{
	"approved", "blockers", "fix_plan", "verdict",
	"rationale", "confidence", "passed", "decision",
}

// Classify returns the shape-derived labels for an artifact's output data.
// Returns nil when no known shape matches. Order is stable (verdict before
// plan) for deterministic output.
func Classify(data map[string]any) []string {
	if len(data) == 0 {
		return nil
	}
	var out []string
	if isVerdictShaped(data) {
		out = append(out, LabelVerdict)
	}
	if isPlanShaped(data) {
		out = append(out, LabelPlan)
	}
	return out
}

// isVerdictShaped reports whether data carries any recognised decision
// field (presence-only, matching the TS `k in d` check).
func isVerdictShaped(data map[string]any) bool {
	for _, k := range verdictKeys {
		if _, ok := data[k]; ok {
			return true
		}
	}
	return false
}

// isPlanShaped reports whether data carries a non-empty `plan` or `text`
// string body (matching the TS firstString([\"plan\",\"text\"]) != "").
func isPlanShaped(data map[string]any) bool {
	for _, k := range []string{"plan", "text"} {
		if s, ok := data[k].(string); ok && s != "" {
			return true
		}
	}
	return false
}
