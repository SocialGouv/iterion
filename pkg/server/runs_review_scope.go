package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
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
	// BaseRef/HeadRef bracket the range. Base is the previous gate, or the
	// run's base commit for the first one.
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
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
	s.writeJSONFor(w, r, buildReviewScope(run, gate))
}

// handleGetRunReviewDiff serves GET /api/runs/{id}/review/diff.
//
// The refs are resolved server-side from the gate number, never taken
// from the caller: they end up as arguments to git, and a client-supplied
// ref is an injection surface for no benefit.
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
	scope := buildReviewScope(run, gate)
	if !scope.Available {
		s.httpErrorFor(w, r, http.StatusConflict, "no review range: %s", scope.Reason)
		return
	}
	payload, derr := gitlib.DiffBetween(run.WorkDir, scope.BaseRef, scope.HeadRef, path)
	if derr != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git diff: %v", derr)
		return
	}
	s.writeJSONFor(w, r, payload)
}

// buildReviewScope resolves the gate range and partitions it by node.
func buildReviewScope(run *store.Run, gate int) *reviewScopeResponse {
	out := &reviewScopeResponse{RunID: run.ID, Groups: []reviewScopeGroup{}}
	if run.WorkDir == "" || !dirExists(run.WorkDir) {
		out.Reason = "this run has no workspace on this host — review ranges are recorded in the run's own git worktree"
		return out
	}
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
	out.Groups = groupByNode(run, files)
	return out
}

// groupByNode attributes each file in the range to the node that last
// changed it, using per-node boundary refs.
//
// Attribution is best-effort by construction and that is deliberate: the
// authoritative answer is the range itself, computed by git over the whole
// workspace. Grouping only decides which heading a file appears under.
func groupByNode(run *store.Run, files []gitlib.FileStatus) []reviewScopeGroup {
	type nodeRange struct {
		node     string
		loopIter int
		when     int64
		set      map[string]bool
	}
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
