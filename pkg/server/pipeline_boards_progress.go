package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// finalOutputCacheMax bounds the memo so a long-lived studio can't leak: past
// this many distinct finished runs the whole map is dropped (a cheap
// recompute), which is fine — entries are ≤ pipelineOutputMaxLen strings.
const finalOutputCacheMax = 5000

// finalOutputCache memoizes finished runs' resolved board output, keyed by run
// id. Finished runs are terminal, so the value is immutable once computed —
// the pipeline-board poll then serves the DONE card's output from memory
// instead of re-probing artifacts on every 3s tick (PR #193 M1). Safe for
// concurrent poll handlers.
type finalOutputCache struct {
	mu sync.RWMutex
	m  map[string]string
}

func (c *finalOutputCache) get(runID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[runID]
	return v, ok
}

func (c *finalOutputCache) put(runID, out string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]string)
	}
	if len(c.m) >= finalOutputCacheMax {
		c.m = make(map[string]string)
	}
	c.m[runID] = out
}

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
	return b.scanRunEvents(runID).executedNodes
}

// runEventScan is the result of ONE walk over a run's event log.
//
// The projection is rebuilt from scratch on every poll (the studio polls
// the board every 3s) and a paused run's log is among the longest the
// store holds — FilesystemRunStore json.Unmarshals every line. Both
// consumers, the node-progress count and a paused gate's instructions,
// therefore share a single pass instead of walking the file twice.
type runEventScan struct {
	executedNodes int
	// instructions holds the LAST human_input_requested text per key
	// (see instructionScanKey). Last-wins is OUTRIGHT: a turn that
	// carries no instructions stores "", so a stale question can never
	// remain displayed above a live form asking something else. Two
	// pause paths legitimately emit no text — a recovery pause
	// (pauseForRecovery) and a turn whose instructions template renders
	// empty (humanInstructionsExtra returns nil).
	instructions map[string]string
}

// instructionScanKey namespaces interaction ids apart from node ids so the
// two key spaces can share one map without colliding.
func instructionScanKey(kind, id string) string {
	return kind + "\x00" + id
}

// scanRunEvents walks a run's events once and memoizes the result for this
// projection build.
func (b *pipelineProjectionBuilder) scanRunEvents(runID string) *runEventScan {
	if scan, ok := b.eventScans[runID]; ok {
		return scan
	}
	scan := &runEventScan{instructions: map[string]string{}}
	if b.rs != nil {
		seen := map[string]struct{}{}
		_ = b.rs.ScanEvents(b.ctx, runID, func(e *store.Event) bool {
			if e == nil {
				return true
			}
			switch e.Type {
			case store.EventNodeStarted:
				if e.NodeID != "" {
					seen[e.NodeID] = struct{}{}
				}
			case store.EventHumanInputRequested:
				if e.NodeID == "" {
					return true
				}
				text, _ := e.Data["instructions"].(string)
				scan.instructions[instructionScanKey("node", e.NodeID)] = text
				if id, ok := e.Data["interaction_id"].(string); ok && id != "" {
					scan.instructions[instructionScanKey("interaction", id)] = text
				}
			}
			return true
		})
		scan.executedNodes = len(seen)
	}
	if b.eventScans == nil {
		b.eventScans = map[string]*runEventScan{}
	}
	b.eventScans[runID] = scan
	return scan
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

// cachedFinalOutput returns the truncated DONE-card output for a finished run,
// memoized across polls (PR #193 M1). A finished run is terminal, so the value
// is stable; the first poll computes it (probing artifacts), every later poll
// serves it from the cache without touching the store.
func (b *pipelineProjectionBuilder) cachedFinalOutput(run *store.Run) string {
	if b.finalOutputMemo != nil {
		if out, ok := b.finalOutputMemo.get(run.ID); ok {
			return out
		}
	}
	out := pipelineTruncate(b.finalOutput(run), pipelineOutputMaxLen)
	if b.finalOutputMemo != nil {
		b.finalOutputMemo.put(run.ID, out)
	}
	return out
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

// pipelineLaneForRoot is the SOLE arbiter of a root card's lane AND of
// whether that card holds a concurrency slot open for its own restart.
//
// Returning both facts from one evaluation is deliberate. A card that
// reserves a slot without rendering in the needs-attention lane is an
// invisible held slot — the operator sees "2/3 running" and no reason why
// nothing starts. That is the worst failure this feature can have, so the
// two answers are not allowed to be computed in two places.
//
// A tree blocked on a human review is IN_PROGRESS (the operator's turn)
// regardless of the root's own transient status, and reserves nothing: the
// run is alive and already holds a real slot.
//
// issue is nil for standalone (non-ticket) roots; terminalStates is the
// board's set of terminal ticket states.
func pipelineLaneForRoot(root *store.Run, issue *native.Issue, terminalStates map[string]struct{}, reviews []PipelineBoardPendingReview) (column string, reserves bool) {
	if len(reviews) > 0 {
		return pipelineColumnInProgress, false
	}
	switch root.Status {
	case store.RunStatusQueued:
		// Waiting for a local concurrency slot — not yet executing, so it
		// stays in Opened (the studio badges it Ready — it is cleared to run).
		return pipelineColumnOpened, false
	case store.RunStatusFinished:
		return pipelineColumnClosed, false
	case store.RunStatusRunning, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		// An operator soft-pause is a RESUMABLE mid-flight state (the run
		// console offers Resume), not a failure — it stays In progress with
		// its "paused" status chip rather than landing in Closed with a
		// Retry-from-zero affordance.
		return pipelineColumnInProgress, false
	case store.RunStatusCancelled:
		// Cancelling is a DECISION, not an anomaly. It goes to Closed and
		// reserves nothing — a cancelled run that held a slot would make the
		// Stop button punish the operator who pressed it, and would make
		// Close (which cancels) retain the very slot it exists to release.
		return pipelineColumnClosed, false
	case store.RunStatusFailed, store.RunStatusFailedResumable:
		if issue == nil {
			// A STANDALONE failure (manual / API / scheduled run, no ticket)
			// has no retry, resume-to-ready or close affordance on this board
			// and reserves nothing. Putting it in the lane would only collect
			// cards nobody can act on — the junkyard that killed the previous
			// Failed lane. It belongs in Closed, badged Failed.
			return pipelineColumnClosed, false
		}
		if _, terminal := terminalStates[issue.State]; terminal {
			if pipelineTicketGaveUp(issue, root) {
				// NOT the operator's filing: the dispatcher wrote this
				// terminal state itself when its retry budget ran out, and
				// an exhausted budget is an anomaly, not a decision. A
				// deterministic failure — a fail-closed pipeline demanding a
				// human — burns every attempt on every run, so treating a
				// give-up as acknowledged history hid exactly the class of
				// failure this lane exists for (issue #494).
				//
				// It renders WITHOUT reserving. A terminal ticket will not
				// relaunch on its own, so holding capacity for it would be a
				// leak with no bound — the same reason a waiting_deps ticket
				// releases (see pipelineTicketHoldsSlot). Retry restages the
				// ticket, and the reservation starts there.
				return pipelineColumnNeedsAttention, false
			}
			// The operator already filed this ticket (typically via Close).
			// The failure is acknowledged history, not open work.
			return pipelineColumnClosed, false
		}
		return pipelineColumnNeedsAttention, pipelineTicketHoldsSlot(issue, root, terminalStates)
	default:
		return pipelineColumnInProgress, false
	}
}

// pipelineTicketGaveUp reports that the ticket in front of us is terminal
// because the DISPATCHER gave up on this very run, with nobody having acted
// on it since — the one case where a terminal ticket state is an unattended
// failure rather than an operator's decision.
//
// The question cannot be answered from the state alone: the dispatcher's
// give-up target (Agent.FailedState, default "blocked") and the board's Close
// target are the same state by construction, so the projection reads the
// stamp the dispatcher leaves instead of guessing. The stamp expires on its
// own — a newer run or any move of the ticket makes it stale (native.GiveUp.
// Current) — so no writer has to remember to clear it, and a stale stamp can
// never pin a card to the lane.
func pipelineTicketGaveUp(issue *native.Issue, root *store.Run) bool {
	if issue == nil || root == nil {
		return false
	}
	return issue.GaveUp.Current(issue.State, root.ID)
}

// pipelineTicketHoldsSlot is the ONE definition of "this ticket is holding a
// concurrency slot", shared by the card projection and the admission gate so
// the badge the operator sees and the arithmetic that refuses launches can
// never disagree.
//
// The restaged case is the subtle one. Retry does not launch: it restages the
// ticket to Ready and the next admission tick starts it, up to two seconds
// later. Releasing the reservation at restage time would open exactly the
// window the feature exists to close — another ready ticket, or a FIFO
// waiter, takes the slot the operator just freed their fix into. So a
// restaged ticket KEEPS its slot, and its relaunch spends it through
// LaunchSpec.PipelineTicketID.
//
// Only while STAGED, though: a Retry that parks in waiting_deps (a blocker
// reopened) is not going to launch soon, and holding capacity for it would
// be a leak with no bound.
func pipelineTicketHoldsSlot(issue *native.Issue, root *store.Run, terminalStates map[string]struct{}) bool {
	if issue == nil || root == nil || strings.TrimSpace(issue.Bot) == "" {
		return false
	}
	if _, terminal := terminalStates[issue.State]; terminal {
		return false
	}
	// A recovery fork EXECUTING for the ticket occupies the slot its dead
	// parent held: forks start via Service.Resume, which never touches
	// pipelineQueue, so nothing else accounts for them — without this the
	// ticket reads 0 active + 0 reserved while the fork runs, and the
	// admission loop over-admits past max-concurrent-pipelines. A
	// terminal fork is out of the race: a parked shell never becomes the
	// root (FinishedAt-gated in the projection) and a finished one
	// closed the card.
	if root.ForkedFrom != "" && !root.Status.IsTerminal() {
		return true
	}
	if root.Status != store.RunStatusFailed && root.Status != store.RunStatusFailedResumable {
		return false
	}
	// Failures iterion caused itself (drain / boot orphan sweep) are not
	// anomalies the operator can fix; reserving for them would wedge the
	// board on every restart.
	if pipelineRunInterrupted(root) {
		return false
	}
	if pipelineIssueRestagedForRelaunch(issue, root) {
		return issue.State == native.StateReady
	}
	return true
}

// pipelineRunInterrupted reports that a run's failure was caused by the
// iterion process itself (a graceful drain or the boot orphan sweep)
// rather than by the workflow. Such a run still shows in the
// needs-attention lane — the operator must know it was cut — but it does
// not reserve a slot. Exact equality against the exported sentinels, so a
// reworded reason breaks the runview test loudly instead of silently
// re-arming the restart wedge.
func pipelineRunInterrupted(run *store.Run) bool {
	if run == nil {
		return false
	}
	return run.Error == runview.ReasonServerDrained || run.Error == runview.ReasonProcessOrphaned
}

// pipelineRunFailed reports whether a run status marks a card failed (as
// opposed to a successfully-finished one). It deliberately still spans
// cancelled: `Failed` is the card's OUTCOME flag, read by the Closed
// lane's success/failed filter and by the studio's retry affordances, and
// it is not the lane test. Lane membership is pipelineLaneForRoot's job.
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

// compactPipelineTitle turns any selected label into a bounded, single-line
// card title. Inputs can legitimately contain entire Markdown briefs; those
// belong in EntryInput, not in the board title or its JSON payload.
func compactPipelineTitle(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s = line
			break
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= pipelineTitleMaxRunes {
		return s
	}
	return strings.TrimSpace(string(runes[:pipelineTitleMaxRunes-1])) + "…"
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
		return compactPipelineTitle(t)
	}
	if issue != nil {
		if t := strings.TrimSpace(issue.Title); t != "" {
			return compactPipelineTitle(t)
		}
	}
	if root != nil {
		if t := strings.TrimSpace(root.BundleDisplayName); t != "" {
			return compactPipelineTitle(t)
		}
		if t := humanizePipelineName(root.WorkflowName); t != "" && t != "Pipeline" {
			return compactPipelineTitle(t)
		}
		if t := strings.TrimSpace(root.Name); t != "" {
			return compactPipelineTitle(t)
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
