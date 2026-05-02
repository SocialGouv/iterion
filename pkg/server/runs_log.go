package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if from < 0 {
		from = 0
	}

	if buf := s.runs.GetLogBuffer(id); buf != nil {
		offset, data, total := buf.Snapshot(from)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Iterion-Log-Offset", strconv.FormatInt(offset, 10))
		w.Header().Set("X-Iterion-Log-Total", strconv.FormatInt(total, 10))
		_, _ = w.Write(data)
		return
	}

	storeDir := s.runs.StoreDir()
	if storeDir == "" {
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
