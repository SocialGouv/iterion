package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// reviewScopeResponse is what a human gate shows the operator: every file
// changed since the previous gate, grouped by the node that changed it.
//
// The GROUP is presentation; the FILE LIST is the contract. Grouping uses
// per-node boundary refs, which only some node kinds record — a subbot, a
// fan-out branch or a compute node has none. So anything that cannot be
// attributed lands in the trailing group with an empty NodeID rather than
// being dropped: a reviewer approving this must never be shown less than
// what changed.
type reviewScopeResponse struct {
	RunID   string `json:"run_id"`
	GateSeq int    `json:"gate_seq"`
	// BaseRef/HeadRef bracket the range. For a worktree run they are git
	// refs; for an in-place run they are workspacetrack snapshot ids
	// (resolved server-side — never taken from the client).
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
	// Backend is "git" or "workspace" so the UI/diff path know which
	// loader to use. Omitted on unavailable responses.
	Backend string `json:"backend,omitempty"`
	// Available is false when no range could be resolved; Reason then says
	// why, in the operator's terms.
	Available bool               `json:"available"`
	Reason    string             `json:"reason,omitempty"`
	Groups    []reviewScopeGroup `json:"groups"`
	// TotalFiles is the size of the whole range, so the UI can show the
	// count without summing groups (they partition it).
	TotalFiles int `json:"total_files"`
}

// reviewScopeGroup is one node's contribution to the range.
type reviewScopeGroup struct {
	// NodeID is empty for the catch-all group.
	NodeID    string              `json:"node_id"`
	Label     string              `json:"label"`
	Iteration int                 `json:"iteration,omitempty"`
	Files     []gitlib.FileStatus `json:"files"`
}

// handleGetRunReviewScope serves GET /api/runs/{id}/review/scope.
//
// Optional ?gate=<n> selects a specific gate; the default is the latest,
// which is the one the operator is paused on.
func (s *Server) handleGetRunReviewScope(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	gate := -1
	if raw := r.URL.Query().Get("gate"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid gate: %q", raw)
			return
		}
		gate = n
	}
	s.writeJSONFor(w, r, buildReviewScope(run, gate, s.workspaceTracker()))
}

// handleGetRunReviewDiff serves GET /api/runs/{id}/review/diff.
//
// The refs are resolved server-side from the gate number, never taken
// from the caller: they end up as arguments to git (or as snapshot ids),
// and a client-supplied ref is an injection surface for no benefit.
func (s *Server) handleGetRunReviewDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	path := r.URL.Query().Get("path")
	if err := gitlib.ValidateRelPath(path); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	gate := -1
	if raw := r.URL.Query().Get("gate"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid gate: %q", raw)
			return
		}
		gate = n
	}
	tracker := s.workspaceTracker()
	scope := buildReviewScope(run, gate, tracker)
	if !scope.Available {
		s.httpErrorFor(w, r, http.StatusConflict, "no review range: %s", scope.Reason)
		return
	}
	if scope.Backend == "workspace" {
		payload, derr := diffReviewScopeWorkspace(tracker, run.ID, scope.BaseRef, scope.HeadRef, path)
		if derr != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "workspace diff: %v", derr)
			return
		}
		s.writeJSONFor(w, r, payload)
		return
	}
	payload, derr := gitlib.DiffBetween(run.WorkDir, scope.BaseRef, scope.HeadRef, path)
	if derr != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git diff: %v", derr)
		return
	}
	s.writeJSONFor(w, r, payload)
}

// handleGetRunWorkspaceFile streams one path from the run's live workspace
// so the review panel can play audio/video and show images without going
// through the text-only /files/content or the 5 MiB review/diff cap.
//
// When the live path is missing (deleted file, finalized worktree), it
// falls back to the workspacetrack object for the HEAD of the current
// review gate so a paused review still has a player for versioned media.
func (s *Server) handleGetRunWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	relPath := r.PathValue("path")
	if id == "" || relPath == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or file path")
		return
	}
	if err := gitlib.ValidateRelPath(relPath); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}

	// Prefer the live workspace — the operator is reviewing the state
	// they can still open on disk.
	if run.WorkDir != "" {
		abs, joinErr := safeJoinUnder(run.WorkDir, relPath)
		if joinErr == nil {
			f, openErr := os.Open(abs)
			if openErr == nil {
				defer f.Close()
				info, statErr := f.Stat()
				if statErr == nil && !info.IsDir() {
					serveWorkspaceFile(w, r, relPath, info, f)
					return
				}
				_ = f.Close()
			}
		}
	}

	// Fallback: content-addressed object from the latest (or requested)
	// review gate's head snapshot.
	tracker := s.workspaceTracker()
	if tracker == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	gate := -1
	if raw := r.URL.Query().Get("gate"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid gate: %q", raw)
			return
		}
		gate = n
	}
	scope := buildReviewScope(run, gate, tracker)
	if !scope.Available || scope.HeadRef == "" {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	headSnap, loadErr := tracker.Load(run.ID, scope.HeadRef)
	if loadErr != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	var hash string
	var size int64
	for _, e := range headSnap.Entries {
		if e.Path == relPath {
			hash, size = e.Hash, e.Size
			break
		}
	}
	if hash == "" {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	body, objErr := tracker.Object(hash)
	if objErr != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	w.Header().Set("Content-Type", artifactFileContentType(relPath))
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(body)), 10))
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disposition, filepath.Base(relPath)))
	// size recorded in the snapshot may differ if the object was truncated;
	// prefer the actual body length for the header.
	_ = size
	_, _ = w.Write(body)
}

func serveWorkspaceFile(w http.ResponseWriter, r *http.Request, relPath string, info os.FileInfo, f *os.File) {
	w.Header().Set("Content-Type", artifactFileContentType(relPath))
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disposition, filepath.Base(relPath)))
	// ServeContent handles Range requests so large media can seek.
	http.ServeContent(w, r, filepath.Base(relPath), info.ModTime(), f)
}

// safeJoinUnder resolves rel under root and rejects any escape.
func safeJoinUnder(root, rel string) (string, error) {
	if err := gitlib.ValidateRelPath(rel); err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(absRoot, filepath.FromSlash(rel))
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	sep := string(filepath.Separator)
	if abs != absRoot && !strings.HasPrefix(abs, absRoot+sep) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return abs, nil
}

// workspaceTracker returns the runview service's workspace tracker, or a
// freshly constructed one from the store dir when the service has none
// (tests, CLI). nil when versioning is disabled.
func (s *Server) workspaceTracker() workspacetrack.Tracker {
	if s == nil || s.runs == nil {
		return nil
	}
	if tr := s.runs.WorkspaceTracker(); tr != nil {
		return tr
	}
	return runviewWorkspaceTracker(s.runs.StoreDir())
}

// runviewWorkspaceTracker is a thin alias so tests can inject without
// importing runview's package-level helper through a cycle. The real
// construction lives in runview.WorkspaceTrackerFor.
var runviewWorkspaceTracker = func(storeDir string) workspacetrack.Tracker {
	if storeDir == "" {
		return nil
	}
	return workspacetrack.NewNative(storeDir)
}

// buildReviewScope resolves the gate range and partitions it by node.
//
// tracker is required for in-place runs (and is ignored for worktree
// runs, which use git refs). Passing nil for an in-place run yields an
// unavailable scope with a reason.
func buildReviewScope(run *store.Run, gate int, tracker workspacetrack.Tracker) *reviewScopeResponse {
	out := &reviewScopeResponse{RunID: run.ID, Groups: []reviewScopeGroup{}}
	if run.WorkDir == "" || !dirExists(run.WorkDir) {
		out.Reason = "this run has no workspace on this host — review ranges are recorded next to the run"
		return out
	}
	if run.Worktree {
		return buildReviewScopeGit(run, gate, out)
	}
	return buildReviewScopeWorkspace(run, gate, tracker, out)
}

func buildReviewScopeGit(run *store.Run, gate int, out *reviewScopeResponse) *reviewScopeResponse {
	gates := listReviewGates(run.WorkDir, run.ID)
	if len(gates) == 0 {
		out.Reason = "no review gate has been reached in this run yet"
		return out
	}
	if gate < 0 {
		gate = gates[len(gates)-1]
	}
	if !containsInt(gates, gate) {
		out.Reason = fmt.Sprintf("gate %d was never reached (recorded: %v)", gate, gates)
		return out
	}
	out.GateSeq = gate
	out.Backend = "git"
	out.HeadRef = store.ReviewGateRef(run.ID, gate)
	if gate > 0 {
		out.BaseRef = store.ReviewGateRef(run.ID, gate-1)
	} else {
		// First gate: the range starts where the run did.
		out.BaseRef = run.BaseCommit
	}
	if out.BaseRef == "" {
		out.Reason = "the run recorded no base commit, so the first gate has no range to compare against"
		return out
	}

	files, err := gitlib.StatusBetween(run.WorkDir, out.BaseRef, out.HeadRef)
	if err != nil {
		out.Reason = fmt.Sprintf("could not read the range: %v", err)
		return out
	}
	out.Available = true
	out.TotalFiles = len(files)
	out.Groups = groupByNodeGit(run, files)
	return out
}

func buildReviewScopeWorkspace(run *store.Run, gate int, tracker workspacetrack.Tracker, out *reviewScopeResponse) *reviewScopeResponse {
	if tracker == nil {
		out.Reason = "workspace versioning is disabled on this host, so in-place review ranges cannot be recorded"
		return out
	}
	gates := workspacetrack.ListGates(tracker, run.ID)
	var baseID, headID string
	switch {
	case len(gates) > 0:
		if gate < 0 {
			gate = gates[len(gates)-1]
		}
		if !containsInt(gates, gate) {
			out.Reason = fmt.Sprintf("gate %d was never reached (recorded: %v)", gate, gates)
			return out
		}
		out.GateSeq = gate
		headLabel := workspacetrack.GateLabel(gate)
		var ok bool
		headID, ok = tracker.Resolve(run.ID, headLabel)
		if !ok {
			out.Reason = fmt.Sprintf("gate %d is labelled but its snapshot is missing", gate)
			return out
		}
		if gate > 0 {
			baseID, ok = tracker.Resolve(run.ID, workspacetrack.GateLabel(gate-1))
			if !ok {
				out.Reason = fmt.Sprintf("previous gate %d has no snapshot", gate-1)
				return out
			}
		} else {
			baseID, ok = workspacetrack.Root(tracker, run.ID)
			if !ok {
				out.Reason = "the run has no initial workspace snapshot to compare the first gate against"
				return out
			}
		}
	default:
		// No explicit gate labels yet. For a run that is currently paused
		// waiting on a human — the only caller of this panel — fall back
		// to "everything since the run's first capture". That covers runs
		// that reached a human gate before markReviewGate started writing
		// gate:N labels, without inventing a range for a still-running
		// or finished run that never paused.
		if !run.Status.IsPaused() {
			out.Reason = "no review gate has been reached in this run yet"
			return out
		}
		headID = tracker.Head(run.ID)
		if headID == "" {
			out.Reason = "this run captured no workspace snapshots at all"
			return out
		}
		var ok bool
		baseID, ok = workspacetrack.Root(tracker, run.ID)
		if !ok {
			out.Reason = "this run captured no workspace snapshots at all"
			return out
		}
		out.GateSeq = 0
	}
	out.Backend = "workspace"
	out.BaseRef = baseID
	out.HeadRef = headID

	baseSnap, err := tracker.Load(run.ID, baseID)
	if err != nil {
		out.Reason = fmt.Sprintf("could not load base snapshot: %v", err)
		return out
	}
	headSnap, err := tracker.Load(run.ID, headID)
	if err != nil {
		out.Reason = fmt.Sprintf("could not load head snapshot: %v", err)
		return out
	}
	// When base and head are the same snapshot (first gate taken before
	// any node ran, or fallback with a single capture), the range is empty
	// rather than "every file in the workspace as an addition".
	var changes []workspacetrack.FileChange
	if baseID == headID {
		changes = nil
	} else {
		changes = workspacetrack.StatusBetween(baseSnap, headSnap)
	}
	files := workspaceChangesToFileStatus(changes)
	out.Available = true
	out.TotalFiles = len(files)
	out.Groups = groupByNodeWorkspace(tracker, run.ID, files)
	return out
}

func workspaceChangesToFileStatus(changes []workspacetrack.FileChange) []gitlib.FileStatus {
	out := make([]gitlib.FileStatus, 0, len(changes))
	for _, c := range changes {
		fs := gitlib.FileStatus{Path: c.Path, Status: c.Status, Binary: c.Binary}
		if c.Binary {
			fs.Added, fs.Deleted = -1, -1
		}
		out = append(out, fs)
	}
	return out
}

func diffReviewScopeWorkspace(tracker workspacetrack.Tracker, runID, baseID, headID, path string) (gitlib.DiffPayload, error) {
	if tracker == nil {
		return gitlib.DiffPayload{}, fmt.Errorf("no workspace tracker")
	}
	var baseSnap, headSnap *workspacetrack.Snapshot
	var err error
	if baseID != "" {
		baseSnap, err = tracker.Load(runID, baseID)
		if err != nil {
			return gitlib.DiffPayload{}, err
		}
	}
	if headID != "" {
		headSnap, err = tracker.Load(runID, headID)
		if err != nil {
			return gitlib.DiffPayload{}, err
		}
	}
	d, err := workspacetrack.DiffBetween(tracker, baseSnap, headSnap, path)
	if err != nil {
		return gitlib.DiffPayload{}, err
	}
	return gitlib.DiffPayload{
		Path:      d.Path,
		Before:    d.Before,
		After:     d.After,
		Binary:    d.Binary,
		Oversized: d.Oversized,
	}, nil
}

// groupByNodeGit attributes each file in the range to the node that last
// changed it, using per-node git boundary refs.
func groupByNodeGit(run *store.Run, files []gitlib.FileStatus) []reviewScopeGroup {
	var ranges []nodeRange
	for _, b := range listNodeBoundaries(run.WorkDir, run.ID) {
		changed, err := gitlib.StatusBetween(run.WorkDir, b.preRef, b.postRef)
		if err != nil {
			continue
		}
		set := make(map[string]bool, len(changed))
		for _, f := range changed {
			set[f.Path] = true
		}
		ranges = append(ranges, nodeRange{node: b.node, loopIter: b.loopIter, when: b.when, set: set})
	}
	// Latest boundary wins: when two nodes touched the same file, the
	// reviewer cares about who left it in the state under review.
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].when < ranges[j].when })
	return partitionFilesByRanges(files, ranges)
}

// groupByNodeWorkspace attributes files using workspacetrack pre:/post:
// labels. Same partition contract as the git path.
func groupByNodeWorkspace(tracker workspacetrack.Tracker, runID string, files []gitlib.FileStatus) []reviewScopeGroup {
	labels := tracker.Labels(runID)
	// Collect nodes that have BOTH pre and post labels.
	type bound struct {
		node     string
		loopIter int
		preID    string
		postID   string
	}
	var bounds []bound
	for label, postID := range labels {
		// post:<node>:<iter>
		if !strings.HasPrefix(label, workspacetrack.PhasePost+":") {
			continue
		}
		rest := strings.TrimPrefix(label, workspacetrack.PhasePost+":")
		slash := strings.LastIndex(rest, ":")
		if slash < 0 {
			continue
		}
		node := rest[:slash]
		loopIter, err := strconv.Atoi(rest[slash+1:])
		if err != nil {
			continue
		}
		preID, ok := labels[workspacetrack.Label(workspacetrack.PhasePre, node, loopIter)]
		if !ok {
			continue
		}
		bounds = append(bounds, bound{node: node, loopIter: loopIter, preID: preID, postID: postID})
	}
	var ranges []nodeRange
	for _, b := range bounds {
		preSnap, err := tracker.Load(runID, b.preID)
		if err != nil {
			continue
		}
		postSnap, err := tracker.Load(runID, b.postID)
		if err != nil {
			continue
		}
		changed := workspacetrack.StatusBetween(preSnap, postSnap)
		set := make(map[string]bool, len(changed))
		for _, f := range changed {
			set[f.Path] = true
		}
		when := int64(0)
		if !postSnap.CreatedAt.IsZero() {
			when = postSnap.CreatedAt.Unix()
		}
		ranges = append(ranges, nodeRange{node: b.node, loopIter: b.loopIter, when: when, set: set})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].when < ranges[j].when })
	return partitionFilesByRanges(files, ranges)
}

type nodeRange struct {
	node     string
	loopIter int
	when     int64
	set      map[string]bool
}

func partitionFilesByRanges(files []gitlib.FileStatus, ranges []nodeRange) []reviewScopeGroup {
	byNode := map[string]*reviewScopeGroup{}
	order := []string{}
	var unattributed []gitlib.FileStatus
	for _, f := range files {
		owner, loopIter := "", 0
		for _, rg := range ranges {
			if rg.set[f.Path] {
				owner, loopIter = rg.node, rg.loopIter
			}
		}
		if owner == "" {
			unattributed = append(unattributed, f)
			continue
		}
		key := fmt.Sprintf("%s@%d", owner, loopIter)
		g, ok := byNode[key]
		if !ok {
			g = &reviewScopeGroup{NodeID: owner, Label: owner, Iteration: loopIter}
			byNode[key] = g
			order = append(order, key)
		}
		g.Files = append(g.Files, f)
	}

	groups := make([]reviewScopeGroup, 0, len(order)+1)
	for _, k := range order {
		groups = append(groups, *byNode[k])
	}
	if len(unattributed) > 0 {
		// Never dropped. A subbot, a fan-out branch or a compute node
		// records no boundary, so its work can only appear here — and a
		// reviewer approving the range must see it.
		groups = append(groups, reviewScopeGroup{
			Label: "Other changes (no per-node boundary recorded)",
			Files: unattributed,
		})
	}
	return groups
}

type nodeBoundary struct {
	node     string
	loopIter int
	preRef   string
	postRef  string
	when     int64
}

// listNodeBoundaries enumerates the nodes that recorded BOTH boundaries,
// with the commit time of the post ref for ordering.
func listNodeBoundaries(workDir, runID string) []nodeBoundary {
	prefix := fmt.Sprintf("refs/iterion/runs/%s/nodes/", runID)
	out, err := gitlib.ForEachRef(workDir, "%(refname) %(committerdate:unix)", prefix)
	if err != nil {
		return nil
	}
	var boundaries []nodeBoundary
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		rest := strings.TrimPrefix(fields[0], prefix)
		slash := strings.LastIndex(rest, "/")
		if slash < 0 {
			continue
		}
		node := rest[:slash]
		loopIter, cerr := strconv.Atoi(rest[slash+1:])
		if cerr != nil {
			continue
		}
		when, _ := strconv.ParseInt(fields[1], 10, 64)
		boundaries = append(boundaries, nodeBoundary{
			node:     node,
			loopIter: loopIter,
			preRef:   store.NodePreSnapshotRef(runID, node, loopIter),
			postRef:  store.NodeSnapshotRef(runID, node, loopIter),
			when:     when,
		})
	}
	return boundaries
}

// listReviewGates returns the gate numbers recorded for a run, ascending.
func listReviewGates(workDir, runID string) []int {
	prefix := strings.TrimSuffix(store.ReviewGateRef(runID, 0), "0")
	out, err := gitlib.ForEachRef(workDir, "%(refname)", prefix)
	if err != nil {
		return nil
	}
	var seqs []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n, cerr := strconv.Atoi(line[strings.LastIndex(line, "/")+1:]); cerr == nil {
			seqs = append(seqs, n)
		}
	}
	sort.Ints(seqs)
	return seqs
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
