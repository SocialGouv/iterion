package server

import (
	"encoding/json"
	"path/filepath"
	"strings"

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
		// Waiting for a local concurrency slot — not yet executing.
		return pipelineColumnTodo
	case store.RunStatusFinished:
		return pipelineColumnDone
	case store.RunStatusRunning, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		// An operator soft-pause is a RESUMABLE mid-flight state (the run
		// console offers Resume), not a failure — it stays In progress with
		// its "paused" status chip rather than landing in Failed with a
		// Retry-from-zero affordance.
		return pipelineColumnInProgress
	case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		// A failed/cancelled run lands in the FAILED lane (with its error
		// as the reason) until the operator retries it back to Todo.
		return pipelineColumnFailed
	default:
		return pipelineColumnInProgress
	}
}

// pipelineRunFailed reports whether a run status marks a card failed (as
// opposed to a not-yet-ready backlog ticket).
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
