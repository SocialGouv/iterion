package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// ---------------------------------------------------------------------------
// Dependency graph + bulk ops (Vague 3)
// ---------------------------------------------------------------------------

// DependencyGraphNode is one ticket in a limited-depth dependency walk.
type DependencyGraphNode struct {
	ID        string                `json:"id"`
	Title     string                `json:"title,omitempty"`
	State     string                `json:"state,omitempty"`
	Bot       string                `json:"bot,omitempty"`
	Satisfied bool                  `json:"satisfied,omitempty"`
	Blockers  []native.BlockerInfo  `json:"blockers,omitempty"`
	Blocking  []native.BlockingInfo `json:"blocking,omitempty"`
	Depth     int                   `json:"depth"`
}

// DependencyGraphResponse is the GET …/dependency-graph payload.
type DependencyGraphResponse struct {
	Root  DependencyGraphNode   `json:"root"`
	Nodes []DependencyGraphNode `json:"nodes"`
	// Edges is a flat list of from→to (dependent → blocker) for simple UIs.
	Edges []DependencyGraphEdge `json:"edges,omitempty"`
}

// DependencyGraphEdge is one directed hard-dep edge.
type DependencyGraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

const dependencyGraphMaxDepth = 4
const dependencyGraphMaxNodes = 64

// handlePipelineBoardDependencyGraph walks hard blockers (and reverse
// blocking one hop) for a ticket. Depth is capped so a large production
// board cannot fan out unbounded.
func (s *Server) handlePipelineBoardDependencyGraph(w http.ResponseWriter, r *http.Request) {
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "dependency graph: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "dependency graph: native tracker is not available")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "dependency graph: missing id")
		return
	}
	// Accept short prefixes via Resolve when available.
	if resolved, rerr := boardStore.Resolve(id); rerr == nil && resolved != "" {
		id = resolved
	}
	rootIss, err := boardStore.Get(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "dependency graph: %v", err)
		return
	}
	all, err := boardStore.List(native.ListFilter{})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "dependency graph: list: %v", err)
		return
	}

	seen := map[string]struct{}{rootIss.ID: {}}
	var nodes []DependencyGraphNode
	var edges []DependencyGraphEdge

	rootNode := DependencyGraphNode{
		ID:       rootIss.ID,
		Title:    rootIss.Title,
		State:    rootIss.State,
		Bot:      rootIss.Bot,
		Blockers: native.ResolveBlockersForIssue(boardStore, rootIss),
		Blocking: native.ReverseBlockers(all, rootIss.ID),
		Depth:    0,
	}
	// Walk blockers outward.
	type item struct {
		id    string
		depth int
	}
	queue := []item{{id: rootIss.ID, depth: 0}}
	for len(queue) > 0 && len(nodes)+1 < dependencyGraphMaxNodes {
		cur := queue[0]
		queue = queue[1:]
		iss, gerr := boardStore.Get(cur.id)
		if gerr != nil || iss == nil {
			continue
		}
		if cur.depth > 0 {
			nodes = append(nodes, DependencyGraphNode{
				ID:       iss.ID,
				Title:    iss.Title,
				State:    iss.State,
				Bot:      iss.Bot,
				Blockers: native.ResolveBlockersForIssue(boardStore, iss),
				Blocking: native.ReverseBlockers(all, iss.ID),
				Depth:    cur.depth,
			})
		}
		if cur.depth >= dependencyGraphMaxDepth {
			continue
		}
		for _, bid := range native.NormalizeBlockers(iss.Blockers) {
			edges = append(edges, DependencyGraphEdge{From: iss.ID, To: bid})
			if _, ok := seen[bid]; ok {
				continue
			}
			seen[bid] = struct{}{}
			queue = append(queue, item{id: bid, depth: cur.depth + 1})
		}
	}
	// One hop reverse: tickets this root blocks (already on root.Blocking).
	for _, b := range rootNode.Blocking {
		if _, ok := seen[b.ID]; ok {
			continue
		}
		if len(nodes)+1 >= dependencyGraphMaxNodes {
			break
		}
		seen[b.ID] = struct{}{}
		if dep, gerr := boardStore.Get(b.ID); gerr == nil && dep != nil {
			nodes = append(nodes, DependencyGraphNode{
				ID:       dep.ID,
				Title:    dep.Title,
				State:    dep.State,
				Bot:      dep.Bot,
				Blockers: native.ResolveBlockersForIssue(boardStore, dep),
				Depth:    -1, // reverse hop
			})
			edges = append(edges, DependencyGraphEdge{From: dep.ID, To: rootIss.ID})
		}
	}

	resp := DependencyGraphResponse{Root: rootNode, Nodes: nodes, Edges: edges}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(resp)
}

type pipelineBulkReadyRequest struct {
	// IDs is an explicit list of ticket ids to stage. When empty, FamilyID
	// and/or PipelineKind filter the board.
	IDs          []string `json:"ids,omitempty"`
	FamilyID     string   `json:"family_id,omitempty"`
	PipelineKind string   `json:"pipeline_kind,omitempty"`
	// OnlySatisfied skips tickets whose hard blockers are still open
	// (default true — only mark Ready when CanLaunch-adjacent deps OK).
	OnlySatisfied *bool `json:"only_satisfied,omitempty"`
}

type pipelineBulkReadyResponse struct {
	Ready       []string          `json:"ready"`
	WaitingDeps []string          `json:"waiting_deps,omitempty"`
	Skipped     []string          `json:"skipped,omitempty"`
	SkippedWhy  map[string]string `json:"skipped_why,omitempty"`
}

type pipelineBulkDeleteRequest struct {
	IDs []string `json:"ids"`
}

type pipelineBulkDeleteResponse struct {
	Deleted    []string          `json:"deleted"`
	Skipped    []string          `json:"skipped,omitempty"`
	SkippedWhy map[string]string `json:"skipped_why,omitempty"`
}

// handlePipelineBoardBulkDelete removes many Opened tickets in one call —
// the multi-select "Delete selected" affordance. Same guards as single
// DELETE: issue only (never runs); skip tickets with any non-terminal run
// in their tree (active_run). Missing ids are skipped, not 404.
func (s *Server) handlePipelineBoardBulkDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bulk delete: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "bulk delete: native tracker is not available")
		return
	}
	var req pipelineBulkDeleteRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bulk delete: invalid request: %v", err)
		return
	}
	if len(req.IDs) == 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bulk delete: ids required")
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	runIndex, err := loadRunIndex(r.Context(), runs)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bulk delete: list runs: %v", err)
		return
	}
	out := pipelineBulkDeleteResponse{SkippedWhy: map[string]string{}}
	seen := map[string]struct{}{}
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		issue, gerr := boardStore.Get(id)
		if gerr != nil || issue == nil {
			out.Skipped = append(out.Skipped, id)
			out.SkippedWhy[id] = "not_found"
			continue
		}
		if strings.TrimSpace(issue.Bot) == "" {
			out.Skipped = append(out.Skipped, id)
			out.SkippedWhy[id] = "not_pipeline_ticket"
			continue
		}
		// Refuse while any run in the tree is still live (same as single delete).
		blocked := false
		for _, run := range issueTreeRuns(issue, runIndex) {
			if run != nil && !run.Status.IsTerminal() {
				out.Skipped = append(out.Skipped, id)
				out.SkippedWhy[id] = "active_run:" + string(run.Status)
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		if derr := boardStore.Delete(id); derr != nil {
			out.Skipped = append(out.Skipped, id)
			out.SkippedWhy[id] = derr.Error()
			continue
		}
		out.Deleted = append(out.Deleted, id)
	}
	if len(out.SkippedWhy) == 0 {
		out.SkippedWhy = nil
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(out)
}

// handlePipelineBoardBulkReady stages many tickets Ready (or waiting_deps)
// in one call — "Ready all tickets of family X whose deps are satisfied".
func (s *Server) handlePipelineBoardBulkReady(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bulk ready: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "bulk ready: native tracker is not available")
		return
	}
	var req pipelineBulkReadyRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bulk ready: invalid request: %v", err)
		return
	}
	onlySatisfied := true
	if req.OnlySatisfied != nil {
		onlySatisfied = *req.OnlySatisfied
	}
	candidates, err := s.collectBulkTargets(boardStore, req)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bulk ready: %v", err)
		return
	}
	board := boardStore.Board()
	out := pipelineBulkReadyResponse{SkippedWhy: map[string]string{}}
	for _, iss := range candidates {
		if iss == nil || strings.TrimSpace(iss.Bot) == "" {
			continue
		}
		if isPipelineTerminalOrActive(iss.State) {
			out.Skipped = append(out.Skipped, iss.ID)
			out.SkippedWhy[iss.ID] = "active_or_terminal"
			continue
		}
		ok, _ := native.BlockersSatisfiedForIssue(boardStore, iss)
		target := native.StateReady
		if !ok {
			if onlySatisfied {
				out.Skipped = append(out.Skipped, iss.ID)
				out.SkippedWhy[iss.ID] = "open_blockers"
				continue
			}
			if board != nil && board.StateByName(native.StateWaitingDeps) != nil {
				target = native.StateWaitingDeps
			} else {
				out.Skipped = append(out.Skipped, iss.ID)
				out.SkippedWhy[iss.ID] = "open_blockers"
				continue
			}
		}
		if _, err := native.SetStateOrReopen(boardStore, iss.ID, target); err != nil {
			out.Skipped = append(out.Skipped, iss.ID)
			out.SkippedWhy[iss.ID] = err.Error()
			continue
		}
		if target == native.StateReady {
			out.Ready = append(out.Ready, iss.ID)
		} else {
			out.WaitingDeps = append(out.WaitingDeps, iss.ID)
		}
	}
	if len(out.SkippedWhy) == 0 {
		out.SkippedWhy = nil
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) collectBulkTargets(boardStore native.BoardStore, req pipelineBulkReadyRequest) ([]*native.Issue, error) {
	if len(req.IDs) > 0 {
		out := make([]*native.Issue, 0, len(req.IDs))
		for _, id := range req.IDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if resolved, err := boardStore.Resolve(id); err == nil && resolved != "" {
				id = resolved
			}
			iss, err := boardStore.Get(id)
			if err != nil {
				continue
			}
			out = append(out, iss)
		}
		return out, nil
	}
	family := strings.TrimSpace(req.FamilyID)
	kind := strings.TrimSpace(req.PipelineKind)
	if family == "" && kind == "" {
		return nil, errBulkFilterRequired
	}
	all, err := boardStore.List(native.ListFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]*native.Issue, 0)
	for _, iss := range all {
		if iss == nil || strings.TrimSpace(iss.Bot) == "" {
			continue
		}
		if family != "" {
			if iss.BotArgs == nil || strings.TrimSpace(iss.BotArgs[native.BotArgFamilyID]) != family {
				continue
			}
		}
		if kind != "" {
			if iss.BotArgs == nil || strings.TrimSpace(iss.BotArgs[native.BotArgPipelineKind]) != kind {
				continue
			}
		}
		out = append(out, iss)
	}
	return out, nil
}

var errBulkFilterRequired = errString("bulk ready: provide ids, family_id, or pipeline_kind")

type errString string

func (e errString) Error() string { return string(e) }

type pipelineRecomputeDepsRequest struct {
	// ClosedID, when set, only re-evaluates dependents of that ticket.
	// Empty = scan every waiting_deps ticket.
	ClosedID string `json:"closed_id,omitempty"`
}

type pipelineRecomputeDepsResponse struct {
	Promoted []string          `json:"promoted"`
	Targets  map[string]string `json:"targets,omitempty"` // id → new state
}

// handlePipelineBoardRecomputeDeps re-runs the unblock promotion for
// waiting_deps tickets (after bulk closes or external edits).
func (s *Server) handlePipelineBoardRecomputeDeps(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "recompute deps: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "recompute deps: native tracker is not available")
		return
	}
	var req pipelineRecomputeDepsRequest
	_ = readJSON(r, &req) // empty body OK

	out := pipelineRecomputeDepsResponse{Targets: map[string]string{}}
	if id := strings.TrimSpace(req.ClosedID); id != "" {
		if resolved, rerr := boardStore.Resolve(id); rerr == nil && resolved != "" {
			id = resolved
		}
		if err := native.PromoteUnblockedDependents(boardStore, id); err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "recompute deps: %v", err)
			return
		}
		// PromoteUnblockedDependents does not return ids — re-list for response.
		// Best-effort empty promoted list is fine for the closed-id path.
	} else {
		// Full scan: every waiting_deps ticket whose blockers are now OK.
		waiting, err := boardStore.List(native.ListFilter{States: []string{native.StateWaitingDeps}})
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "recompute deps: list: %v", err)
			return
		}
		board := boardStore.Board()
		for _, iss := range waiting {
			if iss == nil {
				continue
			}
			ok, _ := native.BlockersSatisfiedForIssue(boardStore, iss)
			if !ok {
				continue
			}
			target := native.UnblockTarget(board, iss)
			if target == "" || target == iss.State {
				continue
			}
			if _, err := boardStore.SetState(iss.ID, target); err != nil {
				continue
			}
			out.Promoted = append(out.Promoted, iss.ID)
			out.Targets[iss.ID] = target
		}
	}
	if len(out.Targets) == 0 {
		out.Targets = nil
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(out)
}
