package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// registerRunRoutes wires the /api/runs surface onto the server's
// mux. Called from routes() after the editor endpoints so the run
// console is opt-in: a server constructed without a store dir
// (s.runs == nil) silently skips registration and behaves exactly
// like the editor-only build.
func (s *Server) registerRunRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.HandleFunc("GET /api/runs", s.handleListRuns)
	s.mux.HandleFunc("POST /api/runs", s.handleLaunchRun)
	s.mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("GET /api/runs/{id}/events", s.handleGetRunEvents)
	s.mux.HandleFunc("GET /api/runs/{id}/workflow", s.handleGetRunWorkflow)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts/{node}", s.handleListArtifacts)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts/{node}/{version}", s.handleGetArtifact)
	s.mux.HandleFunc("GET /api/runs/{id}/files", s.handleListRunFiles)
	s.mux.HandleFunc("GET /api/runs/{id}/files/diff", s.handleGetRunFileDiff)
	s.mux.HandleFunc("GET /api/runs/{id}/commits", s.handleListRunCommits)
	s.mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)
	s.mux.HandleFunc("POST /api/runs/{id}/resume", s.handleResumeRun)
	s.mux.HandleFunc("POST /api/runs/{id}/merge", s.handleMergeRun)
	s.mux.HandleFunc("GET /api/ws/runs/{id}", s.handleRunWebSocket)
}

// --- Request / response shapes ---

type launchRunRequest struct {
	FilePath string `json:"file_path"`
	// Source is the .iter contents uploaded inline. In cloud mode the
	// editor SPA sends this so the server pod doesn't need a shared
	// filesystem; FilePath is then advisory (used for display + as the
	// AST parserPath). When both are set, Source wins.
	Source string            `json:"source,omitempty"`
	RunID  string            `json:"run_id,omitempty"`
	Vars   map[string]string `json:"vars,omitempty"`
	// Timeout is a Go-style duration string ("30m", "2h"). Empty disables.
	Timeout string `json:"timeout,omitempty"`
	// MergeInto is the worktree-finalization merge target. See
	// runview.LaunchSpec.MergeInto.
	MergeInto string `json:"merge_into,omitempty"`
	// BranchName overrides the storage branch name created on the
	// worktree's HEAD. See runview.LaunchSpec.BranchName.
	BranchName string `json:"branch_name,omitempty"`
	// MergeStrategy is "squash" (default) or "merge". See
	// runview.LaunchSpec.MergeStrategy.
	MergeStrategy string `json:"merge_strategy,omitempty"`
	// AutoMerge: when true, the engine performs the merge at end of
	// run; when false (default), merge is deferred to a UI action.
	AutoMerge bool `json:"auto_merge,omitempty"`
}

type launchRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type resumeRunRequest struct {
	FilePath string `json:"file_path,omitempty"` // optional; falls back to run.FilePath
	// Source carries the .iter contents inline. Used in cloud mode
	// when the resumer (editor SPA) wants to push a possibly-modified
	// workflow without depending on the server pod's filesystem.
	Source  string                 `json:"source,omitempty"`
	Answers map[string]interface{} `json:"answers,omitempty"`
	Force   bool                   `json:"force,omitempty"`
	Timeout string                 `json:"timeout,omitempty"`
}

type cancelRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

// --- Handlers ---

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := runview.ListFilter{
		Workflow: q.Get("workflow"),
		Node:     q.Get("node"),
	}
	if status := q.Get("status"); status != "" {
		filter.Status = store.RunStatus(status)
	}
	if since := q.Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid since (want RFC3339): %v", err)
			return
		}
		filter.Since = t
	}
	if limit := q.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		filter.Limit = n
	}
	out, err := s.runs.List(filter)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list runs: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]interface{}{"runs": out})
}

func (s *Server) handleLaunchRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req launchRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if req.FilePath == "" && req.Source == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "file_path or source is required")
		return
	}
	// FilePath is treated as a logical name when Source is supplied
	// (no disk read). When Source is empty we fall back to safePath
	// resolution against the server's WorkDir — the legacy local-mode
	// flow.
	var absPath string
	if req.Source == "" {
		var pathErr error
		absPath, pathErr = s.safePath(req.FilePath)
		if pathErr != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid file_path: %v", pathErr)
			return
		}
	} else {
		absPath = req.FilePath
	}
	timeout, err := parseTimeout(req.Timeout)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid timeout: %v", err)
		return
	}

	// Detach from the request context so cancellation of the HTTP
	// request (client disconnect, timeout) does not abort the
	// workflow goroutine. The run is its own lifecycle managed by
	// the service's manager; clients cancel via POST /cancel.
	ctx := context.Background()

	res, err := s.runs.Launch(ctx, runview.LaunchSpec{
		FilePath:      absPath,
		Source:        req.Source,
		RunID:         req.RunID,
		Vars:          req.Vars,
		Timeout:       timeout,
		MergeInto:     req.MergeInto,
		BranchName:    req.BranchName,
		MergeStrategy: store.MergeStrategy(req.MergeStrategy),
		AutoMerge:     req.AutoMerge,
	})
	if err != nil {
		if errors.Is(err, runtime.ErrServerDraining) {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "server is draining: %v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "launch: %v", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, launchRunResponse{RunID: res.RunID, Status: string(store.RunStatusRunning)})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	snap, err := s.runs.Snapshot(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	s.writeJSONFor(w, r, snap)
}

func (s *Server) handleGetRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	q := r.URL.Query()
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	events, err := s.runs.LoadEvents(id, from, to)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load events: %v", err)
		return
	}
	if events == nil {
		events = []*store.Event{}
	}
	s.writeJSONFor(w, r, map[string]interface{}{"events": events})
}

func (s *Server) handleGetRunWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	wf, err := s.runs.LoadWireWorkflow(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "load workflow: %v", err)
		return
	}
	s.writeJSONFor(w, r, wf)
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node := r.PathValue("node")
	if id == "" || node == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id or node")
		return
	}
	out, err := s.runs.ListArtifacts(id, node)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list artifacts: %v", err)
		return
	}
	if out == nil {
		out = []runview.ArtifactSummary{}
	}
	s.writeJSONFor(w, r, map[string]interface{}{"artifacts": out})
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node := r.PathValue("node")
	versionStr := r.PathValue("version")
	if id == "" || node == "" || versionStr == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id, node, or version")
		return
	}
	version, err := strconv.Atoi(versionStr)
	if err != nil || version < 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid version")
		return
	}
	a, err := s.runs.LoadArtifact(id, node, version)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "artifact not found: %v", err)
		return
	}
	s.writeJSONFor(w, r, a)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Log cancel intent with source attribution. Mystery context-canceled
	// failures during a run mid-flight typically trace back to either this
	// HTTP endpoint or the WS `cancel` envelope (handleCancel in runs_ws.go);
	// emitting a line per call site lets us tell the two apart without
	// instrumenting the runtime itself.
	if s.logger != nil {
		s.logger.Info("server: cancel run %q via HTTP from %s", id, r.RemoteAddr)
	}
	if err := s.runs.Cancel(id); err != nil {
		// If the run is not currently active in this process, surface
		// the current persisted status so the client can still get a
		// useful response (e.g. already-finished runs).
		if errors.Is(err, runview.ErrRunNotActive) {
			r2, loadErr := s.runs.LoadRun(id)
			if loadErr != nil {
				s.httpErrorFor(w, r, http.StatusNotFound, "run not active and not on disk: %v", loadErr)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: string(r2.Status)})
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "cancel: %v", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: "cancelling"})
}

func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	var req resumeRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	// Resolve file path: explicit body wins, falling back to the
	// FilePath persisted at launch.
	filePath := req.FilePath
	if filePath == "" {
		runMeta, err := s.runs.LoadRun(id)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
			return
		}
		filePath = runMeta.FilePath
		if filePath == "" && req.Source == "" {
			s.httpErrorFor(w, r, http.StatusBadRequest, "file_path or source is required (run has no persisted FilePath)")
			return
		}
	}
	// In cloud mode (Source supplied) skip the safePath disk check —
	// FilePath is purely a label. Local mode keeps the legacy guard.
	var absPath string
	if req.Source == "" {
		var pathErr error
		absPath, pathErr = s.safePath(filePath)
		if pathErr != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid file_path: %v", pathErr)
			return
		}
	} else {
		absPath = filePath
	}
	timeout, err := parseTimeout(req.Timeout)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid timeout: %v", err)
		return
	}

	ctx := context.Background()
	res, err := s.runs.Resume(ctx, runview.ResumeSpec{
		RunID:    id,
		FilePath: absPath,
		Source:   req.Source,
		Answers:  req.Answers,
		Force:    req.Force,
		Timeout:  timeout,
	})
	if err != nil {
		if errors.Is(err, runtime.ErrServerDraining) {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "server is draining: %v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "resume: %v", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, launchRunResponse{RunID: res.RunID, Status: string(store.RunStatusRunning)})
}

// parseTimeout accepts an empty string (no timeout) or a Go duration
// string. Negative values are rejected.
func parseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("timeout must not be negative")
	}
	return d, nil
}
