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
// skips bytes (default 0); ?until=M bounds the read to [from, until)
// so the studio's replay scrubber can fetch exactly the prefix its
// in-memory window evicted instead of re-downloading the whole log.
// until <= 0 (or absent) means "to the current end".
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
	until, _ := strconv.ParseInt(r.URL.Query().Get("until"), 10, 64)
	if until < 0 {
		until = 0
	}
	ls := store.AsRunLogStore(s.runs.RunStore())

	if buf := s.runs.GetLogBuffer(id); buf != nil {
		snapOffset, data, total := buf.Snapshot(from)
		respOffset := snapOffset
		// If the ring has evicted bytes older than `from`, fill the
		// missing prefix [from, snapOffset) from the persisted log
		// (authoritative; the ring is just a 1 MiB live tail). Without
		// this, the studio's "copy log" / "download log" buttons on a
		// long-running active run miss everything before the ring's
		// lower bound — same root cause as the WS subscribe path.
		var prefix []byte
		if snapOffset > from && ls != nil {
			prefixEnd := snapOffset
			if until > 0 && until < prefixEnd {
				prefixEnd = until
			}
			if pre, err := ls.ReadRunLogRange(r.Context(), id, from, prefixEnd); err == nil && len(pre) > 0 {
				prefix = pre
				respOffset = from
			}
		}
		data = trimLogWindow(snapOffset, data, until)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Iterion-Log-Offset", strconv.FormatInt(respOffset, 10))
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
	data, err := ls.ReadRunLogRange(r.Context(), id, from, until)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read log: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Iterion-Log-Offset", strconv.FormatInt(from, 10))
	w.Header().Set("X-Iterion-Log-Total", strconv.FormatInt(total, 10))
	_, _ = w.Write(data)
}

// trimLogWindow bounds a live-buffer snapshot to [.., until): data
// covers [snapOffset, snapOffset+len(data)) in the run's byte stream;
// until <= 0 means unbounded.
func trimLogWindow(snapOffset int64, data []byte, until int64) []byte {
	if until <= 0 {
		return data
	}
	if until <= snapOffset {
		return nil
	}
	if until < snapOffset+int64(len(data)) {
		return data[:until-snapOffset]
	}
	return data
}
