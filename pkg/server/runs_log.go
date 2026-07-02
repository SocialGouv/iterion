package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/store"
)

func (s *Server) registerRunLogRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.HandleFunc("GET /api/runs/{id}/log", s.handleGetRunLog)
}

// handleGetRunLog returns the log bytes for a run, served from the
// in-memory buffer for active runs and from <store>/runs/<id>/run.log
// for terminated runs. ?from=N skips bytes; default 0.
func (s *Server) handleGetRunLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant scoping + id validation: load under the caller's context so
	// the mongo tenant filter rejects cross-tenant reads, and the store's
	// path-component sanitiser rejects a traversal-shaped id, before we
	// join it into <store>/runs/<id>/run.log below.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if from < 0 {
		from = 0
	}

	if buf := s.runs.GetLogBuffer(id); buf != nil {
		offset, data, total := buf.Snapshot(from)
		// If the ring has evicted bytes older than `from`, fill the
		// missing prefix [from, offset) from the persisted run.log
		// (authoritative; the ring is just a 1 MiB live tail). Without
		// this, the studio's "copy log" / "download log" buttons on a
		// long-running active run miss everything before the ring's
		// lower bound — same root cause as the WS subscribe path.
		var prefix []byte
		if offset > from {
			if storeDir := s.runs.StoreDir(); storeDir != "" {
				if pre, err := readPersistedLogRange(storeDir, id, from, offset); err == nil {
					prefix = pre
					offset = from
				}
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

	storeDir := s.runs.StoreDir()
	if storeDir == "" {
		// Cloud mode: no filesystem, no in-memory buffer — serve the
		// persisted run_logs chunks through the store's RunLogStore
		// (ADR-053 parity for the REST log endpoint).
		if ls := store.AsRunLogStore(s.runs.RunStore()); ls != nil {
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
			return
		}
		s.httpErrorFor(w, r, http.StatusNotFound, "no log buffer for run %q", id)
		return
	}
	logPath := filepath.Join(storeDir, "runs", id, "run.log")
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.httpErrorFor(w, r, http.StatusNotFound, "no log captured for run %q", id)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "open log: %v", err)
		return
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, 0); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "seek log: %v", err)
			return
		}
	}
	stat, _ := f.Stat()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Iterion-Log-Offset", strconv.FormatInt(from, 10))
	if stat != nil {
		w.Header().Set("X-Iterion-Log-Total", strconv.FormatInt(stat.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

// readPersistedLogRange reads bytes [from, until) from <store>/runs/<id>/run.log.
// Returns the slice (possibly short if EOF hit). Caller treats any error
// as "file unavailable" and falls back to the ring-only response.
func readPersistedLogRange(storeDir, runID string, from, until int64) ([]byte, error) {
	if from >= until {
		return nil, nil
	}
	logPath := filepath.Join(storeDir, "runs", runID, "run.log")
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, until-from)
	n, _ := io.ReadFull(f, buf)
	return buf[:n], nil
}
