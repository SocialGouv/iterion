package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrRewindNotRewindable is returned when the run's status forbids an
// in-place rewind. Callers map it to 409.
var ErrRewindNotRewindable = errors.New("runview: rewind: run is not in a rewindable status")

// ErrRewindNodeNotReached is returned when the requested pivot node has
// no recorded output and is not the current checkpoint anchor — i.e. the
// run never got there, so there is nothing to rewind to. Callers map it
// to 400.
var ErrRewindNodeNotReached = errors.New("runview: rewind: node was never reached by this run")

// ArtifactLabelRewound marks the marker version appended over a
// published artifact whose producing node was invalidated by a rewind.
const ArtifactLabelRewound = "rewound"

// RewindSpec describes an IN-PLACE rewind — "I misconfigured the bot,
// back up and replay from here" — as opposed to Fork's
// "create-an-alternative-future", which mints a new run id.
//
// The run keeps its id, name, inputs, lineage, and budget accounting.
// Only its checkpoint anchor and the state produced at or after NodeID
// are invalidated, and it is parked in a resumable status. The caller
// issues Resume separately (with --force when the .bot source was edited
// in between, which is the whole point of the operation).
type RewindSpec struct {
	// RunID is the run to rewind. Required.
	RunID string
	// NodeID is the pivot: the already-executed node the run will
	// re-execute from. Required unless Auto is set.
	NodeID string
	// Auto derives the pivot by diffing the workflow source this run
	// executed against the source on disk now, and picking the earliest
	// executed node the edit affects.
	//
	// This is the bot-development loop in one step: edit the .bot, rewind
	// --auto, resume. Without it the operator has to translate "I changed
	// implement's prompt" into "--node implement" on every iteration, and
	// an edited edge or shared prompt has no obvious answer at all.
	//
	// Requires the run to carry Run.WorkflowSource (captured at launch).
	// An explicit NodeID always wins.
	Auto bool
	// KeepFiles opts OUT of restoring the workspace.
	//
	// By default a rewind also undoes what the dropped nodes PRODUCED,
	// not merely what they declared: the workspace is reverted to the
	// state the pivot started from. For a bot whose real product is files
	// — docs, code — the output map is only a summary, so rewinding the
	// checkpoint alone would leave the half-written tree in place and the
	// replayed node would build on top of itself.
	//
	// The revert is additive (the prior tree is committed on top of HEAD,
	// and uncommitted work is banked first), so opting out is about
	// intent — "replay this node against the current files" — never about
	// safety.
	KeepFiles bool
	// SourcePath overrides the workflow source to compute the graph
	// from. Empty resolves the run's persisted FilePath (via the bot
	// catalog when needed), exactly like the studio's workflow view.
	//
	// The graph is needed to decide what "downstream of NodeID" means.
	// Note this compiles the source AS IT IS NOW — deliberately: the
	// operator rewinds precisely because they edited it, and the resume
	// that follows executes the new graph too.
	SourcePath string
}

// RewindResult is the response shape returned to HTTP / CLI callers.
type RewindResult struct {
	RunID string `json:"run_id"`
	// FromNode is the checkpoint anchor the run carried before the
	// rewind ("" when it had no checkpoint).
	FromNode string `json:"from_node,omitempty"`
	// NodeID is the new checkpoint anchor — the node resume re-executes.
	NodeID string `json:"node_id"`
	// DroppedNodes lists the nodes whose outputs were invalidated,
	// sorted. Always contains NodeID.
	DroppedNodes []string `json:"dropped_nodes"`
	// TombstonedArtifacts lists the dropped nodes whose published
	// artifact was superseded by a `rewound` marker version, sorted.
	TombstonedArtifacts []string `json:"tombstoned_artifacts,omitempty"`
	// OrphanedChildRuns lists child run ids the dropped subbot nodes had
	// spawned and were still pointing at. The pointer is released so the
	// replay launches a fresh child; the runs themselves are left alone
	// (an in-flight one keeps going — cancel it if it is burning budget).
	OrphanedChildRuns []string `json:"orphaned_child_runs,omitempty"`
	// Files reports the workspace half of the rewind: whether it was
	// restored, to which snapshot, the revert commit, the backup ref
	// banking the pre-revert state, and — importantly — why it was
	// skipped when it was.
	Files *fileRevertResult `json:"files,omitempty"`
	// Status is the run's status after the rewind (always resumable).
	Status string `json:"status"`
	// AutoTargeted reports that NodeID was derived from the source diff
	// rather than supplied by the caller.
	AutoTargeted bool `json:"auto_targeted,omitempty"`
	// PromotedFrom is set when the requested pivot sat inside a fan-out
	// body and NodeID was promoted to the router orchestrating it, so
	// every parallel execution of that node replays instead of one.
	PromotedFrom string `json:"promoted_from,omitempty"`
	// Changes lists the workflow-source differences that selected the
	// pivot. Populated only for an --auto rewind — it is what lets the
	// operator confirm the tool understood the edit.
	Changes []DeclChange `json:"changes,omitempty"`
}

// rewindableStatuses are the statuses an in-place rewind accepts. A
// `running` run is deliberately excluded: its engine owns the checkpoint
// and would overwrite the rewind at its next node boundary. Cancel or
// pause it first.
var rewindableStatuses = []store.RunStatus{
	store.RunStatusFailedResumable,
	store.RunStatusCancelled,
	store.RunStatusPausedOperator,
	store.RunStatusPausedWaitingHuman,
	store.RunStatusQueued,
}

// Rewind re-anchors an existing run's checkpoint on an already-executed
// node and invalidates every output downstream of it, so the next Resume
// re-executes from there without replaying the upstream nodes that were
// already paid for.
//
// It is the iteration counterpart of Fork: same "restart at node N"
// effect, but on the SAME run id, because the operator is fixing a
// misconfiguration rather than exploring an alternative. See
// docs/resume.md § Rewind.
//
// The run is parked in `cancelled` — the one resumable status the cloud
// runner treats as "explicit resume required" (see pkg/runner/loop.go:
// failed_resumable and paused_operator are auto-resumed on queue
// redelivery, which would race the operator's .bot edit and execute the
// stale workflow).
//
// SCOPE: this rewinds ENGINE state (checkpoint outputs), the store
// artifacts those nodes published, their subbot child pointers, and — for
// a run with an isolated worktree — the workspace files, reverted to the
// state the pivot started from. It does NOT undo external effects (board
// cards, forge comments, pushed commits, already-launched child runs).
//
// A run WITHOUT a worktree keeps its files: its workspace is the
// operator's live tree, which iterion deliberately does not snapshot
// (doing so would stage the operator's own work through `git add -A`).
// Those runs get a populated Files.SkipReason rather than silence.
func (s *Service) Rewind(ctx context.Context, spec RewindSpec) (*RewindResult, error) {
	if spec.RunID == "" {
		return nil, errors.New("runview: rewind: run_id is required")
	}
	if spec.NodeID == "" && !spec.Auto {
		return nil, errors.New("runview: rewind: node_id is required (or set auto to derive it from the source diff)")
	}
	run, err := s.store.LoadRun(ctx, spec.RunID)
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	if !isRewindableStatus(run.Status) {
		return nil, fmt.Errorf("%w: %s (rewindable: %s)",
			ErrRewindNotRewindable, run.Status, joinStatuses(rewindableStatuses))
	}
	cp := run.Checkpoint
	if cp == nil {
		return nil, fmt.Errorf("runview: rewind: run %s has no checkpoint — nothing to rewind", spec.RunID)
	}
	// Compile the CURRENT source to learn the topology. Deliberately no
	// workflow-hash check: rewinding after editing the .bot is the
	// primary use case, and the hash guard belongs to resume (--force),
	// which is where the operator asserts the edit is compatible.
	sourcePath := spec.SourcePath
	if sourcePath == "" {
		sourcePath = resolveWorkflowPath(run)
	}
	if sourcePath == "" {
		return nil, fmt.Errorf("runview: rewind: run %s has no workflow source path — pass one explicitly", spec.RunID)
	}
	wf, err := CompileWorkflow(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("compile workflow %s (needed to resolve what is downstream of %q): %w",
			sourcePath, spec.NodeID, err)
	}
	// Nodes this run actually executed — the search space for --auto and
	// the validity domain for an explicit --node.
	executed := map[string]bool{}
	for id := range cp.Outputs {
		executed[id] = true
	}
	if cp.NodeID != "" {
		executed[cp.NodeID] = true
	}

	pivot := spec.NodeID
	var changes []DeclChange
	autoTargeted := false
	if pivot == "" {
		current, rerr := os.ReadFile(sourcePath)
		if rerr != nil {
			return nil, fmt.Errorf("read current workflow source %s: %w", sourcePath, rerr)
		}
		pivot, changes, err = resolveAutoPivot(run.WorkflowSource, string(current), wf, executed)
		if err != nil {
			return nil, err
		}
		autoTargeted = true
	}

	if _, ok := wf.Nodes[pivot]; !ok {
		return nil, fmt.Errorf("runview: rewind: node %q is not in workflow %s", pivot, sourcePath)
	}
	// The pivot must be a node the run actually reached. Without this a
	// typo silently parks the run on a node that never ran, and the
	// resume then fails deep in the engine with a much worse message.
	//
	// Checked BEFORE the fan-out promotion below: a router does not always
	// record an output of its own, so validating the promoted anchor would
	// reject a perfectly good rewind. The body node having executed is
	// proof enough that its router did.
	if !executed[pivot] {
		return nil, fmt.Errorf("%w: %q (reached: %s)",
			ErrRewindNodeNotReached, pivot, joinSorted(mapKeys(cp.Outputs)))
	}
	// A pivot inside a fan-out body is promoted to the router that
	// orchestrates it. The checkpoint keeps one output per node id, so N
	// parallel executions of a body node collapse to a single entry;
	// anchoring on the node would replay it once, linearly, with no `each`
	// context — testing one iteration instead of all of them, silently.
	// Re-running every instance is only expressible as "re-run the
	// fan-out".
	promotedFrom := ""
	if router := fanOutRouterFor(wf, pivot); router != "" && router != pivot {
		promotedFrom, pivot = pivot, router
		if s.logger != nil {
			s.logger.Info("rewind: %q is inside the fan-out of %q — anchoring on the router so every parallel execution replays",
				promotedFrom, pivot)
		}
	}

	dropped := downstreamOf(wf, pivot, cp.Outputs)
	fromNode := cp.NodeID

	// Workspace first: a git failure then leaves the run's engine state
	// untouched, so the operator can fix the tree and retry the whole
	// rewind. The reverse order could leave a rewound checkpoint pointing
	// at un-rewound files.
	files := &fileRevertResult{SkipReason: "keep_files requested"}
	if !spec.KeepFiles {
		files, err = revertWorkspace(run, wf, cp, pivot)
		if err != nil {
			return nil, err
		}
	}

	// Claim the run before mutating. UpdateRunStatusIf is a CAS against
	// the expected statuses, so a concurrent resume that already flipped
	// this run to `running` loses the race instead of being rewound out
	// from under itself. `cancelled` preserves the checkpoint through
	// applyStatusTransition (unlike running/finished/failed, which nil
	// it) — so this must run BEFORE the checkpoint write.
	claimed, err := s.store.UpdateRunStatusIf(ctx, run.ID, store.RunStatusCancelled, "", rewindableStatuses)
	if err != nil {
		return nil, fmt.Errorf("claim run for rewind: %w", err)
	}
	if !claimed {
		return nil, fmt.Errorf("%w: status changed under us — reload and retry", ErrRewindNotRewindable)
	}

	// Reserve the tombstone versions BEFORE persisting the checkpoint,
	// but write the artifacts AFTER. WriteArtifact updates
	// Run.ArtifactIndex inside run.json, so writing first and saving the
	// run second would clobber that index with our stale in-memory copy.
	tombstones := s.planArtifactTombstones(ctx, run.ID, cp, dropped)

	// Release the subbot child pointers of the dropped nodes. Without
	// this the rewind is silently a no-op for subbots: ReattachSubbotChild
	// consults ONLY run.SubbotChildren and the child's status — never the
	// parent's checkpoint (ADR-084 rejected the checkpoint as its home,
	// since a parked subbot node has not finished) — and it runs before
	// the child .bot is even compiled. So a re-executed subbot node would
	// adopt the pre-rewind child and its output, and the edited child
	// workflow would never run.
	//
	// The happy path already cleared the pointer when the child finished;
	// what survives is exactly the interesting case — a child still
	// parked on a human gate, or in flight.
	orphaned := detachSubbotChildren(run, dropped)

	applyRewind(cp, pivot, dropped)
	for _, ts := range tombstones {
		// The engine writes a node's next artifact at
		// ArtifactVersions[node] and then increments
		// (pkg/runtime/engine_exec.go), so the tombstone must reserve its
		// slot — otherwise re-execution overwrites the marker and the
		// "supersede" silently becomes a delete.
		cp.ArtifactVersions[ts.nodeID] = ts.version + 1
	}

	run.Status = store.RunStatusCancelled
	// Clear the stale failure message: the run is no longer "failed at
	// verify", it is parked at the pivot awaiting a fresh execution.
	run.Error = ""
	run.Checkpoint = cp
	run.UpdatedAt = time.Now().UTC()
	if err := s.store.SaveRun(ctx, run); err != nil {
		return nil, fmt.Errorf("save rewound run: %w", err)
	}
	if err := s.store.SaveCheckpoint(ctx, run.ID, cp); err != nil {
		return nil, fmt.Errorf("save rewound checkpoint: %w", err)
	}

	tombstoned := s.writeArtifactTombstones(ctx, run.ID, pivot, tombstones)

	// Append-only audit. events.jsonl is never truncated — the dropped
	// nodes' original records stay, and this marker explains why they
	// are about to appear a second time.
	if _, err := s.store.AppendEvent(ctx, run.ID, store.Event{
		Type:   store.EventRunRewound,
		RunID:  run.ID,
		NodeID: pivot,
		Data: map[string]any{
			"from_node":            fromNode,
			"to_node":              pivot,
			"dropped_nodes":        dropped,
			"tombstoned_artifacts": tombstoned,
			"orphaned_child_runs":  orphaned,
			"promoted_from":        promotedFrom,
			"files_reverted":       files.Reverted,
			"files_ref":            files.Ref,
			"files_revert_commit":  files.RevertCommit,
			"files_backup_ref":     files.BackupRef,
			"files_skip_reason":    files.SkipReason,
		},
	}); err != nil && s.logger != nil {
		// Best-effort: the state mutation already landed and is the
		// authority. Losing the marker degrades the audit trail, it
		// does not corrupt the run.
		s.logger.Warn("rewind: append run_rewound event for %s: %v", run.ID, err)
	}
	if s.logger != nil {
		s.logger.Info("rewind: run %s re-anchored %q → %q, dropped %d node output(s), superseded %d artifact(s), released %d child run(s)",
			run.ID, fromNode, pivot, len(dropped), len(tombstoned), len(orphaned))
	}

	return &RewindResult{
		RunID:               run.ID,
		FromNode:            fromNode,
		NodeID:              pivot,
		DroppedNodes:        dropped,
		TombstonedArtifacts: tombstoned,
		OrphanedChildRuns:   orphaned,
		Files:               files,
		Status:              string(store.RunStatusCancelled),
		AutoTargeted:        autoTargeted,
		PromotedFrom:        promotedFrom,
		Changes:             changes,
	}, nil
}

// applyRewind mutates cp in place: re-anchor on nodeID and invalidate
// the dropped nodes' state.
//
// What is deliberately NOT reset, and why:
//   - Budget counters (tokens / cost / iterations / elapsed) and
//     CostUSDTotal: resume never resets them either. A rewind that
//     refunded spend would make "rewind + resume" an unbounded way
//     around max_cost_usd. Raise the cap on the resume instead.
//   - LoopCounters: same argument for max_iterations. An exhausted loop
//     stays exhausted; grant more with resume's --max-iterations.
//   - LoopPreviousOutput / LoopCurrentOutput: the pivot may legitimately
//     read {{loop.<name>.previous_output}} on its first re-execution.
func applyRewind(cp *store.Checkpoint, nodeID string, dropped []string) {
	cp.NodeID = nodeID
	if cp.ArtifactVersions == nil {
		cp.ArtifactVersions = map[string]int{}
	}
	for _, id := range dropped {
		delete(cp.Outputs, id)
		// A re-executed node gets a fresh recovery budget: it is being
		// replayed because the WORKFLOW changed, so attempts spent
		// against the old definition should not count against it.
		delete(cp.NodeAttempts, id)
	}
	// Any pending interaction belonged to the node the run was parked
	// on, which is at or downstream of the pivot. Carrying it over would
	// make the resume try to answer a question the rewound run never
	// asked.
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	// Drop backend rehydration so the pivot restarts from a clean
	// conversation. Fork pins these from a turn checkpoint because it
	// resumes mid-turn; a rewind wants the node re-run from scratch
	// against the edited prompt — replaying the old conversation would
	// carry the very context the operator is trying to change.
	cp.BackendName = ""
	cp.BackendSessionID = ""
	cp.BackendConversation = nil
	cp.BackendPendingToolUseID = ""
}

// detachSubbotChildren removes, from the run's in-memory SubbotChildren
// map, every entry belonging to a dropped node, and returns the child
// run ids released (sorted, deduped). The caller persists the run
// afterwards, so the removal rides the same SaveRun as the rest of the
// rewind rather than racing it through ClearSubbotChild.
//
// Key shape is the engine's (pkg/runtime/special_node.go:subbotReattachKey):
//
//	sanitize(<node id>[@<loop=iter;…>][#<branch id>])
//
// so a node's entries are its sanitized id alone, or that id followed by
// the '@' / '#' delimiter. Matching on the delimiter — not a bare prefix
// — is what keeps node "run" from stealing node "run_child"'s entries.
func detachSubbotChildren(run *store.Run, dropped []string) []string {
	if len(run.SubbotChildren) == 0 {
		return nil
	}
	prefixes := make(map[string]bool, len(dropped))
	for _, id := range dropped {
		prefixes[sanitizeSubbotKey(id)] = true
	}
	seen := map[string]bool{}
	var released []string
	for key, childID := range run.SubbotChildren {
		head := key
		if i := strings.IndexAny(key, "@#"); i >= 0 {
			head = key[:i]
		}
		if !prefixes[head] {
			continue
		}
		delete(run.SubbotChildren, key)
		if childID != "" && !seen[childID] {
			seen[childID] = true
			released = append(released, childID)
		}
	}
	if len(run.SubbotChildren) == 0 {
		run.SubbotChildren = nil
	}
	sort.Strings(released)
	return released
}

// sanitizeSubbotKey mirrors the engine's subbotKeySanitizer: '.' and '$'
// are illegal in a Mongo field name and the key becomes a map key on the
// parent run doc. Group-instantiated nodes carry dotted ids
// ("pr.check"), so this is load-bearing, not cosmetic.
var subbotKeyReplacer = strings.NewReplacer(".", "_", "$", "_")

func sanitizeSubbotKey(nodeID string) string { return subbotKeyReplacer.Replace(nodeID) }

// artifactTombstone is one reserved marker-version write.
type artifactTombstone struct {
	nodeID     string
	version    int
	supersedes int
}

// planArtifactTombstones decides which dropped nodes have a published
// artifact that must be superseded, and reserves the version each marker
// will occupy.
//
// Why supersede rather than delete: Run.ArtifactIndex is what
// LoadLatestArtifact reads, and three consumers read through it — the
// run-completion webhook payload (pkg/notify), a parent reading a
// subbot's outputs back (pkg/runview/subbot.go), and the pipeline board's
// progress projection (pkg/server). Leaving the index pointing at the
// pre-rewind artifact makes those three serve output the checkpoint has
// already invalidated. Appending a marker version fixes that while
// keeping every earlier version on disk and readable, which is the same
// append-only contract events.jsonl follows.
func (s *Service) planArtifactTombstones(ctx context.Context, runID string, cp *store.Checkpoint, dropped []string) []artifactTombstone {
	var out []artifactTombstone
	for _, id := range dropped {
		latest, err := s.store.LoadLatestArtifact(ctx, runID, id)
		if err != nil || latest == nil {
			// No artifact published by this node — nothing to supersede.
			continue
		}
		if isRewoundArtifact(latest) {
			// Already superseded by an earlier rewind that was never
			// resumed; a second marker would add noise, not information.
			continue
		}
		next := cp.ArtifactVersions[id]
		if next <= latest.Version {
			next = latest.Version + 1
		}
		out = append(out, artifactTombstone{nodeID: id, version: next, supersedes: latest.Version})
	}
	return out
}

// writeArtifactTombstones performs the reserved writes and returns the
// node ids that were successfully superseded, sorted. Best-effort: the
// checkpoint is the authority and has already landed, so a failed marker
// write is logged rather than failing the whole rewind.
func (s *Service) writeArtifactTombstones(ctx context.Context, runID, pivot string, tombstones []artifactTombstone) []string {
	var done []string
	for _, ts := range tombstones {
		a := &store.Artifact{
			RunID:   runID,
			NodeID:  ts.nodeID,
			Version: ts.version,
			Labels:  []string{ArtifactLabelRewound},
			Data: map[string]any{
				"_rewound":     true,
				"_rewound_to":  pivot,
				"_supersedes":  ts.supersedes,
				"_rewound_at":  time.Now().UTC().Format(time.RFC3339),
				"_explanation": fmt.Sprintf("superseded by a rewind to %q; version %d holds the pre-rewind output", pivot, ts.supersedes),
			},
			WrittenAt: time.Now().UTC(),
		}
		if err := s.store.WriteArtifact(ctx, a); err != nil {
			if s.logger != nil {
				s.logger.Warn("rewind: supersede artifact %s/%s v%d: %v", runID, ts.nodeID, ts.version, err)
			}
			continue
		}
		done = append(done, ts.nodeID)
	}
	sort.Strings(done)
	return done
}

// isRewoundArtifact reports whether an artifact is a rewind marker.
func isRewoundArtifact(a *store.Artifact) bool {
	for _, l := range a.Labels {
		if l == ArtifactLabelRewound {
			return true
		}
	}
	return false
}

// downstreamOf computes the set of node ids whose outputs a rewind to
// pivot must invalidate, restricted to nodes that actually have a
// recorded output.
//
// The rule is `{pivot} ∪ (forward-reachable(pivot) \ backward-reachable(pivot))`.
//
// Subtracting the ancestors is what makes this correct in the presence
// of loops. In `implement -> verify -> implement as fix(3)`, `verify` is
// BOTH downstream and upstream of `implement`: it is reachable forward
// (implement→verify) and it can reach implement (the back-edge). Its
// output is exactly what `implement` reads back via
// {{loop.fix.previous_output}} / {{outputs.verify}} on re-entry, so
// dropping it would break the first re-execution with a missing
// reference. A node that can reach the pivot always ran before it and
// must survive.
//
// Over-dropping is as harmful as under-dropping here: a node whose
// output is deleted but which does not re-execute before something
// reads it resolves to nil, not to a re-run.
func downstreamOf(wf *ir.Workflow, pivot string, outputs map[string]map[string]any) []string {
	fwd := reachable(pivot, adjacency(wf, false))
	bwd := reachable(pivot, adjacency(wf, true))

	drop := map[string]bool{pivot: true}
	for id := range fwd {
		if !bwd[id] {
			drop[id] = true
		}
	}
	out := make([]string, 0, len(drop))
	for id := range drop {
		// Only report what was actually invalidated: a downstream node
		// the run never reached has nothing to drop, and listing it
		// would misrepresent the blast radius in the audit event.
		if _, ok := outputs[id]; ok || id == pivot {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// adjacency builds the successor (or, reversed, predecessor) map of the
// workflow graph. Loop and foreach back-edges are included: they are
// real execution paths, and the ancestor subtraction in downstreamOf —
// not edge filtering — is what keeps cycles from over-dropping.
func adjacency(wf *ir.Workflow, reverse bool) map[string][]string {
	adj := make(map[string][]string, len(wf.Nodes))
	for _, e := range wf.Edges {
		if e == nil {
			continue
		}
		from, to := e.From, e.To
		if reverse {
			from, to = to, from
		}
		adj[from] = append(adj[from], to)
	}
	return adj
}

// reachable returns every node reachable from start, EXCLUDING start
// itself unless a cycle leads back to it.
func reachable(start string, adj map[string][]string) map[string]bool {
	seen := map[string]bool{}
	queue := append([]string(nil), adj[start]...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, adj[id]...)
	}
	return seen
}

func isRewindableStatus(s store.RunStatus) bool {
	for _, ok := range rewindableStatuses {
		if s == ok {
			return true
		}
	}
	return false
}

func joinStatuses(ss []store.RunStatus) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s))
	}
	return strings.Join(out, ", ")
}

func mapKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func joinSorted(ss []string) string {
	sort.Strings(ss)
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, ", ")
}
