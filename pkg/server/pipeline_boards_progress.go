package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runProgress returns (executed, total) node counts for one run. Finished
// runs clamp to 100% with no event scan; queued runs report 0/total; other
// statuses count distinct node_started events (clamped to total).
func (b *pipelineProjectionBuilder) runProgress(run *store.Run) (executed, total int) {
	total = b.totalNodes(run.FilePath)
	switch run.Status {
	case store.RunStatusFinished:
		return total, total
	case store.RunStatusQueued:
		return 0, total
	default:
		exec := b.executedNodes(run.ID)
		if total > 0 && exec > total {
			exec = total
		}
		return exec, total
	}
}

// executedNodes counts distinct nodes that started for a run (node_started
// fires once per loop iteration, so dedup on node id).
func (b *pipelineProjectionBuilder) executedNodes(runID string) int {
	if b.rs == nil {
		return 0
	}
	seen := map[string]struct{}{}
	_ = b.rs.ScanEvents(b.ctx, runID, func(e *store.Event) bool {
		if e.Type == store.EventNodeStarted && e.NodeID != "" {
			seen[e.NodeID] = struct{}{}
		}
		return true
	})
	return len(seen)
}

// totalNodes compiles the run's workflow (memoized by file path) and
// returns its node count; 0 when the file is absent or fails to compile.
func (b *pipelineProjectionBuilder) totalNodes(filePath string) int {
	if filePath == "" {
		return 0
	}
	if n, ok := b.nodeCountCache[filePath]; ok {
		return n
	}
	var (
		wf  *ir.Workflow
		err error
	)
	if bundle := runview.ResolveBundleFromFilePath(filePath); bundle != nil {
		wf, _, err = runview.CompileBundleWorkflow(filePath, bundle)
	} else {
		wf, _, err = runview.CompileWorkflowWithHash(filePath)
	}
	n := 0
	if err == nil && wf != nil {
		n = len(wf.Nodes)
	}
	b.nodeCountCache[filePath] = n
	return n
}

// finalOutput resolves a finished run's user-facing output: the
// final_answer artifact field (pinned node first, then any artifact node),
// falling back to a compact rendering of the latest-written artifact.
func (b *pipelineProjectionBuilder) finalOutput(run *store.Run) string {
	if b.rs == nil {
		return ""
	}
	if run.CallbackAnswerNode != "" {
		if s := b.answerField(run.ID, run.CallbackAnswerNode); s != "" {
			return s
		}
	}
	for nodeID := range run.ArtifactIndex {
		if s := b.answerField(run.ID, nodeID); s != "" {
			return s
		}
	}
	return b.latestArtifactSummary(run)
}

func (b *pipelineProjectionBuilder) answerField(runID, nodeID string) string {
	art, err := b.rs.LoadLatestArtifact(b.ctx, runID, nodeID)
	if err != nil || art == nil || art.Data == nil {
		return ""
	}
	if raw, ok := art.Data[pipelineFinalAnswerField]; ok {
		if s, ok := raw.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// latestArtifactSummary probes each artifact-bearing node (bounded),
// picks the most-recently-written artifact, and returns a compact JSON of
// its data — the DONE fallback when no final_answer field exists.
func (b *pipelineProjectionBuilder) latestArtifactSummary(run *store.Run) string {
	var (
		best   *store.Artifact
		probed int
	)
	for nodeID := range run.ArtifactIndex {
		if probed >= pipelineArtifactProbeCap {
			break
		}
		probed++
		art, err := b.rs.LoadLatestArtifact(b.ctx, run.ID, nodeID)
		if err != nil || art == nil || len(art.Data) == 0 {
			continue
		}
		if best == nil || art.WrittenAt.After(best.WrittenAt) {
			best = art
		}
	}
	if best == nil {
		return ""
	}
	encoded, err := json.Marshal(best.Data)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// pipelineColumnForRoot maps a root run to a lane. A tree blocked on a
// human review is IN_PROGRESS (the operator's turn) regardless of the
// root's own transient status.
func pipelineColumnForRoot(root *store.Run, reviews []PipelineBoardPendingReview) string {
	if len(reviews) > 0 {
		return pipelineColumnInProgress
	}
	switch root.Status {
	case store.RunStatusQueued:
		// Waiting for a local concurrency slot — not yet executing, so it
		// stays in Opened (the studio badges it Ready — it is cleared to run).
		return pipelineColumnOpened
	case store.RunStatusFinished:
		return pipelineColumnClosed
	case store.RunStatusRunning, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		// An operator soft-pause is a RESUMABLE mid-flight state (the run
		// console offers Resume), not a failure — it stays In progress with
		// its "paused" status chip rather than landing in Closed with a
		// Retry-from-zero affordance.
		return pipelineColumnInProgress
	case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		// A failed/cancelled run lands in the CLOSED lane (with its error as
		// the reason, flagged failed) until the operator retries it to Opened.
		return pipelineColumnClosed
	default:
		return pipelineColumnInProgress
	}
}

// pipelineRunFailed reports whether a run status marks a card failed (as
// opposed to a successfully-finished one — both share the Closed lane).
func pipelineRunFailed(status store.RunStatus) bool {
	switch status {
	case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		return true
	default:
		return false
	}
}

func pipelineRunBotID(run *store.Run) string {
	if run == nil {
		return ""
	}
	if run.BotID != "" {
		return run.BotID
	}
	if run.BundleName != "" {
		return run.BundleName
	}
	if run.BundlePath != "" {
		return strings.TrimSuffix(filepath.Base(strings.TrimRight(run.BundlePath, "/")), ".botz")
	}
	return ""
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func stringMapToAny(in map[string]string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func pipelineTruncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// pipelineDisplayTitle picks the label shown on a pipeline card.
//
// Priority:
//  1. Content-derived title from bot inputs / bot_args (explains WHAT the
//     pipeline is producing — e.g. "Boudicca · ÉP 1/5 — Le Fouet et le Serment")
//  2. Native ticket title (operator-authored)
//  3. Bundle display name / humanized workflow name
//  4. Run codename (GenerateRunName) — last resort; unique but opaque
//
// Run codenames are intentionally NOT preferred: they are branch/ID helpers,
// not content labels. See titleFromContentInputs for the input key heuristics.
func pipelineDisplayTitle(issue *native.Issue, root *store.Run) string {
	var inputs map[string]any
	if root != nil && len(root.Inputs) > 0 {
		inputs = root.Inputs
	} else if issue != nil && len(issue.BotArgs) > 0 {
		inputs = stringMapToAny(issue.BotArgs)
	}
	if t := titleFromContentInputs(inputs); t != "" {
		return t
	}
	if issue != nil {
		if t := strings.TrimSpace(issue.Title); t != "" {
			return t
		}
	}
	if root != nil {
		if t := strings.TrimSpace(root.BundleDisplayName); t != "" {
			return t
		}
		if t := humanizePipelineName(root.WorkflowName); t != "" && t != "Pipeline" {
			return t
		}
		if t := strings.TrimSpace(root.Name); t != "" {
			return t
		}
	}
	return "Pipeline"
}

// titleFromContentInputs builds a human content label from common bot input
// keys. Prefer structured subject + episode framing (shorts / series bots)
// over free-form prose. Returns "" when inputs have nothing usable so callers
// can fall through to ticket title / run name.
//
// Recognised patterns (first match wins on each slot):
//
//	subject:  character | requested_character | subject | family | family_name |
//	          asset_name | collection | series
//	episode:  episode_no (+ episode_total) | ep / episode
//	title:    episode_title | title | topic | feature | name (when not a path)
//
// Example: character=Boudicca, episode_no=1, episode_total=5,
// episode_title="Le Fouet et le Serment"
// → "Boudicca · ÉP 1/5 — Le Fouet et le Serment"
func titleFromContentInputs(inputs map[string]any) string {
	if len(inputs) == 0 {
		return ""
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			v, ok := inputs[k]
			if !ok || v == nil {
				continue
			}
			s := strings.TrimSpace(fmt.Sprint(v))
			if s == "" || s == "<nil>" {
				continue
			}
			return s
		}
		return ""
	}

	subject := get(
		"character", "requested_character", "subject",
		"family", "family_id", "family_name", "asset_name", "collection", "series",
	)
	// Planners often only carry a path — use the file stem as subject
	// (e.g. assets/.../boudicca.json → boudicca).
	if subject == "" {
		if p := get("input_path", "catalog_path", "output_dir"); p != "" {
			base := filepath.Base(strings.TrimRight(p, "/\\"))
			base = strings.TrimSuffix(base, filepath.Ext(base))
			base = strings.TrimSpace(base)
			if base != "" && !looksLikeMachineToken(base) {
				subject = humanizePipelineName(base)
			}
		}
	}
	epNo := get("episode_no", "ep", "episode")
	// episode_index is often 0-based; only use it when episode_no is absent,
	// and prefer the 1-based display the operator expects (index+1 when numeric).
	if epNo == "" {
		if idx := get("episode_index"); idx != "" {
			epNo = episodeIndexAsOneBased(idx)
		}
	}
	epTotal := get("episode_total", "episodes", "episode_count")
	epTitle := get("episode_title", "title", "topic", "feature")
	// "name" is common but often a machine id / path — only use when it looks
	// human (no slash, not a bare uuid-ish token) and we still have nothing.
	if epTitle == "" {
		if n := get("name"); n != "" && !looksLikeMachineToken(n) {
			epTitle = n
		}
	}

	// Full shorts-style frame: Subject · ÉP n/N — Title
	if epNo != "" {
		epLabel := "ÉP " + epNo
		if epTotal != "" {
			epLabel = fmt.Sprintf("ÉP %s/%s", epNo, epTotal)
		}
		switch {
		case subject != "" && epTitle != "":
			return fmt.Sprintf("%s · %s — %s", subject, epLabel, epTitle)
		case subject != "":
			return fmt.Sprintf("%s · %s", subject, epLabel)
		case epTitle != "":
			return fmt.Sprintf("%s — %s", epLabel, epTitle)
		default:
			return epLabel
		}
	}

	if subject != "" && epTitle != "" && subject != epTitle {
		return fmt.Sprintf("%s — %s", subject, epTitle)
	}
	if epTitle != "" {
		return epTitle
	}
	if subject != "" {
		return subject
	}

	// Last-resort content: a short prose field, truncated so it can't blow the card.
	for _, k := range []string{"hook", "angle", "summary", "place", "period"} {
		if s := get(k); s != "" {
			return pipelineTruncate(s, 80)
		}
	}
	return ""
}

// episodeIndexAsOneBased turns a 0-based episode_index into a 1-based display
// number when the value is a plain integer; non-numeric values pass through.
func episodeIndexAsOneBased(idx string) string {
	var n int
	if _, err := fmt.Sscanf(idx, "%d", &n); err == nil {
		// Heuristic: indices start at 0 in many bots; if the value is already
		// ≥1 leave it (some bots store 1-based in episode_index).
		if n == 0 {
			return "1"
		}
		// For n>=1 we can't know base — keep as-is (callers prefer episode_no).
		return fmt.Sprintf("%d", n)
	}
	return idx
}

// looksLikeMachineToken rejects values that are paths, UUIDs, or codenames
// from being used as a human title fragment.
func looksLikeMachineToken(s string) bool {
	if strings.ContainsAny(s, "/\\") {
		return true
	}
	if strings.Count(s, "-") >= 3 && !strings.Contains(s, " ") {
		// e.g. run codenames orbital-plunge-borealroar-707f
		return true
	}
	// Hex-ish uuid fragments
	if len(s) >= 32 {
		hexish := true
		for _, r := range s {
			if r == '-' {
				continue
			}
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				hexish = false
				break
			}
		}
		if hexish {
			return true
		}
	}
	return false
}

func humanizePipelineName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Pipeline"
	}
	var b strings.Builder
	previousSeparator := true
	for _, r := range value {
		if r == '_' || r == '-' || r == '.' || r == '/' || r == ' ' {
			if b.Len() > 0 && !previousSeparator {
				b.WriteByte(' ')
			}
			previousSeparator = true
			continue
		}
		if previousSeparator {
			b.WriteRune(toUpperRune(r))
		} else {
			b.WriteRune(r)
		}
		previousSeparator = false
	}
	return strings.TrimSpace(b.String())
}

func toUpperRune(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 32
	}
	return r
}
