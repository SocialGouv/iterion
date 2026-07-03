package server

import (
	"net/http"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/store"
)

func (s *Server) registerRunLogRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.HandleFunc("GET /api/runs/{id}/log", s.handleGetRunLog)
}

// handleGetRunLog returns the log bytes for a run: the in-memory buffer
// for runs live in this process, the store's RunLogStore otherwise
// (runs/<id>/run.log on a filesystem store, the run_logs chunks on the
// Mongo store — one persisted read path for both, ADR-053). ?from=N
// skips bytes; default 0.
func (s *Server) handleGetRunLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant scoping + id validation: load under the caller's context so
	// the mongo tenant filter rejects cross-tenant reads, and the store's
	// path-component sanitiser rejects a traversal-shaped id, before any
	// log bytes are read for it.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if from < 0 {
		from = 0
	}
	ls := store.AsRunLogStore(s.runs.RunStore())

	if buf := s.runs.GetLogBuffer(id); buf != nil {
		offset, data, total := buf.Snapshot(from)
		// If the ring has evicted bytes older than `from`, fill the
		// missing prefix [from, offset) from the persisted log
		// (authoritative; the ring is just a 1 MiB live tail). Without
		// this, the studio's "copy log" / "download log" buttons on a
		// long-running active run miss everything before the ring's
		// lower bound — same root cause as the WS subscribe path.
		var prefix []byte
		if offset > from && ls != nil {
			if pre, err := ls.ReadRunLogRange(r.Context(), id, from, offset); err == nil && len(pre) > 0 {
				prefix = pre
				offset = from
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Iterion-Log-Offset", strconv.FormatInt(offset, 10))
		w.Header().Set("X-Iterion-Log-Total", strconv.FormatInt(total, 10))
		if len(prefix) > 0 {
			_, _ = w.Write(prefix)
		}
		_, _ = w.Write(data)
		return
	}

	if ls == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "no log buffer for run %q", id)
		return
	}
	total, err := ls.RunLogSize(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "log size: %v", err)
		return
	}
	if total == 0 {
		s.httpErrorFor(w, r, http.StatusNotFound, "no log captured for run %q", id)
		return
	}
	data, err := ls.ReadRunLogRange(r.Context(), id, from, 0)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read log: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Iterion-Log-Offset", strconv.FormatInt(from, 10))
	w.Header().Set("X-Iterion-Log-Total", strconv.FormatInt(total, 10))
	_, _ = w.Write(data)
}
