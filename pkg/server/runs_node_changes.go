package server

import (
	"net/http"
	"strconv"
)

// handleGetRunNodeChanges serves GET /api/runs/{id}/nodes/{node}/changes —
// what one node execution did to the workspace, the "node as a commit"
// view.
//
// Node scoping is a path segment, matching /artifacts/{node}. Optional
// ?iteration=N selects a loop iteration; the default is the latest
// recorded, which is NOT the same as the checkpoint's loop counter (that
// records where the run stopped, routinely past the last iteration this
// node ran).
func (s *Server) handleGetRunNodeChanges(w http.ResponseWriter, r *http.Request) {
	id, node := r.PathValue("id"), r.PathValue("node")
	if id == "" || node == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or node id")
		return
	}
	iteration, ok := parseIterationParam(s, w, r)
	if !ok {
		return
	}
	// LoadRunCtx happens inside the service, before any filesystem or git
	// access — the tenant filter lives there and the helpers underneath
	// have no tenant awareness at all.
	set, err := s.runs.NodeChanges(r.Context(), id, node, iteration)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "node changes: %v", err)
		return
	}
	s.writeJSONFor(w, r, set)
}

// handleGetRunNodeFileDiff serves GET /api/runs/{id}/nodes/{node}/diff.
//
// Refs and snapshot ids are resolved server-side from (run, node,
// iteration); none of them is caller-supplied, because they become git
// arguments and filesystem lookups.
func (s *Server) handleGetRunNodeFileDiff(w http.ResponseWriter, r *http.Request) {
	id, node := r.PathValue("id"), r.PathValue("node")
	if id == "" || node == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or node id")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "path is required")
		return
	}
	iteration, ok := parseIterationParam(s, w, r)
	if !ok {
		return
	}
	payload, err := s.runs.NodeFileDiff(r.Context(), id, node, iteration, path)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "node diff: %v", err)
		return
	}
	s.writeJSONFor(w, r, payload)
}

// parseIterationParam reads ?iteration=N. Absent means "latest recorded".
// maxIterationParam bounds ?iteration=N.
//
// Defence in depth: the resolvers no longer probe downwards for an
// explicit iteration, so a large N costs one lookup — but the bound keeps
// a malformed request an obvious 400 instead of a plausible-looking miss,
// and it is the guard that holds if the probe ever walks again. Far above
// any real loop (max_iterations is a workflow budget, not a page size).
const maxIterationParam = 1024

func parseIterationParam(s *Server, w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("iteration")
	if raw == "" {
		return -1, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxIterationParam {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid iteration: %q", raw)
		return 0, false
	}
	return n, true
}
