package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/server/projects"
	runstore "github.com/SocialGouv/iterion/pkg/store"
)

// WorkspaceRuntimeFactory builds one complete project server. The host
// acquires the project's store lease before calling it, preventing two local
// runtimes from constructing dispatchers over the same state.
type WorkspaceRuntimeFactory func(projects.Project) (*Server, error)

type workspaceRuntime struct {
	project    projects.Project
	generation string
	server     *Server
	lease      *flock.Flock
	state      string
	err        string
}

// WorkspaceHost multiplexes durable project runtimes behind one HTTP
// listener. Switching the current project changes only a registry pointer;
// every runtime and WebSocket scope remains alive.
type WorkspaceHost struct {
	mu       sync.RWMutex
	registry *projects.Config
	factory  WorkspaceRuntimeFactory
	logger   *iterlog.Logger
	runtimes map[string]*workspaceRuntime
	handoffs map[string]*workspaceHandoff
	// handoffTickets maps a ticket digest to its durable handoff id. Raw
	// bearer tickets are never written to disk.
	handoffTickets map[string]string
	handoffWake    chan struct{}
	handoffCancel  context.CancelFunc
	handoffWG      sync.WaitGroup
	static         fs.FS
}

type WorkspaceRuntimeStatus struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Dir          string `json:"dir"`
	StoreDir     string `json:"store_dir"`
	Generation   string `json:"generation,omitempty"`
	State        string `json:"state"`
	RuntimeReady bool   `json:"runtime_ready"`
	Error        string `json:"error,omitempty"`
	ScopedURL    string `json:"scoped_url"`
	LastOpened   string `json:"last_opened"`
	Kind         string `json:"kind"`
}

func NewWorkspaceHost(registry *projects.Config, factory WorkspaceRuntimeFactory, logger *iterlog.Logger) (*WorkspaceHost, error) {
	if registry == nil {
		return nil, fmt.Errorf("workspace: project registry is required")
	}
	if factory == nil {
		return nil, fmt.Errorf("workspace: runtime factory is required")
	}
	if logger == nil {
		logger = iterlog.Nop()
	}
	staticSub, err := fs.Sub(StaticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("workspace: SPA assets: %w", err)
	}
	h := &WorkspaceHost{
		registry:       registry,
		factory:        factory,
		logger:         logger,
		runtimes:       make(map[string]*workspaceRuntime),
		handoffs:       make(map[string]*workspaceHandoff),
		handoffTickets: make(map[string]string),
		handoffWake:    make(chan struct{}, 1),
		static:         staticSub,
	}
	h.loadWorkspaceHandoffs()
	for _, project := range registry.RecentProjects {
		h.startRuntime(project)
	}
	h.startHandoffDelivery()
	return h, nil
}

func (h *WorkspaceHost) startRuntime(project projects.Project) {
	rt := &workspaceRuntime{
		project:    project,
		generation: workspaceGeneration(),
		state:      "starting",
	}
	h.mu.Lock()
	h.runtimes[project.ID] = rt
	h.mu.Unlock()

	if _, err := os.Stat(project.Dir); err != nil {
		h.degrade(rt, fmt.Errorf("project root unavailable: %w", err))
		return
	}
	if _, err := os.Stat(project.StoreDir); err != nil {
		h.degrade(rt, fmt.Errorf("project store unavailable: %w", err))
		return
	}
	lease := flock.New(filepath.Join(project.StoreDir, ".workspace-owner.lock"))
	locked, err := lease.TryLock()
	if err != nil {
		h.degrade(rt, fmt.Errorf("acquire store lease: %w", err))
		return
	}
	if !locked {
		h.degrade(rt, fmt.Errorf("store is already owned by another workspace process"))
		return
	}
	rt.lease = lease
	srv, err := h.factory(project)
	if err != nil {
		_ = lease.Unlock()
		rt.lease = nil
		h.degrade(rt, err)
		return
	}
	// Every embedded server represents exactly one stable project scope. Seed
	// its server-info cache with that ID so a hidden pane still identifies
	// itself after the workspace host's global current pointer changes.
	srv.cacheCurrentProjectID(project.ID)
	if err := srv.StartEmbedded(); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		cancel()
		_ = lease.Unlock()
		rt.lease = nil
		h.degrade(rt, err)
		return
	}
	h.mu.Lock()
	rt.server = srv
	rt.state = "ready"
	h.mu.Unlock()
	h.logger.Info("workspace: project %s ready at /x/%s/", project.Name, project.ID)
}

func (h *WorkspaceHost) degrade(rt *workspaceRuntime, err error) {
	h.mu.Lock()
	rt.state = "degraded"
	rt.err = err.Error()
	h.mu.Unlock()
	h.logger.Warn("workspace: project %s degraded: %v", rt.project.Name, err)
}

func workspaceGeneration() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (h *WorkspaceHost) Handler() http.Handler {
	return http.HandlerFunc(h.ServeHTTP)
}

func (h *WorkspaceHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/x/") {
		h.serveScoped(w, r)
		return
	}
	switch r.URL.Path {
	case "/healthz":
		h.writeJSON(w, map[string]string{"status": "ok"})
		return
	case "/readyz":
		h.ready(w)
		return
	case "/api/projects":
		switch r.Method {
		case http.MethodGet:
			h.listProjects(w)
		case http.MethodPost:
			if !workspaceSafeOrigin(r) {
				h.writeError(w, http.StatusForbidden, "cross-origin workspace mutation rejected")
				return
			}
			h.addProject(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	case "/api/projects/current":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.currentProject(w)
		return
	case "/api/projects/switch":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !workspaceSafeOrigin(r) {
			h.writeError(w, http.StatusForbidden, "cross-origin workspace mutation rejected")
			return
		}
		h.switchProject(w, r)
		return
	case "/api/workspace/runtimes":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.writeJSON(w, h.statuses())
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/projects/") && r.Method == http.MethodDelete {
		if !workspaceSafeOrigin(r) {
			h.writeError(w, http.StatusForbidden, "cross-origin workspace mutation rejected")
			return
		}
		h.removeProject(w, strings.TrimPrefix(r.URL.Path, "/api/projects/"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		rt := h.currentRuntime()
		if rt == nil || rt.server == nil {
			h.writeError(w, http.StatusServiceUnavailable, "current project runtime is unavailable")
			return
		}
		rt.server.Handler().ServeHTTP(w, r)
		return
	}
	h.serveWorkspaceAsset(w, r)
}

func (h *WorkspaceHost) ready(w http.ResponseWriter) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ready := 0
	degraded := make([]string, 0)
	for _, project := range h.registry.RecentProjects {
		// Legacy registry rows without a pinned store are visible for repair,
		// but they are not workspace runtimes and cannot hold readiness down.
		if project.StoreDir == "" {
			continue
		}
		runtime := h.runtimes[project.ID]
		if runtime != nil && runtime.state == "ready" && runtime.server != nil {
			ready++
			continue
		}
		degraded = append(degraded, project.ID)
	}
	status := http.StatusOK
	if ready == 0 || len(degraded) > 0 {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":                ready,
		"degraded_project_ids": degraded,
	})
}

func (h *WorkspaceHost) serveScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/x/")
	id, sub, found := strings.Cut(rest, "/")
	if id == "" || !found {
		h.writeError(w, http.StatusBadRequest, "missing project scope")
		return
	}
	h.mu.RLock()
	rt := h.runtimes[id]
	h.mu.RUnlock()
	if rt == nil {
		h.writeError(w, http.StatusNotFound, "unknown project scope")
		return
	}
	if sub == "_ws/info" {
		scheme := "ws"
		if r.TLS != nil {
			scheme = "wss"
		}
		h.writeJSON(w, map[string]any{
			"ws_base":      scheme + "://" + r.Host + "/x/" + id,
			"needs_ticket": false,
		})
		return
	}
	if sub == "api/project/identity" {
		h.writeJSON(w, WorkspaceRuntimeStatus{
			ID: id, Name: rt.project.Name, Dir: rt.project.Dir,
			StoreDir: rt.project.StoreDir, Generation: rt.generation,
			State: rt.state, RuntimeReady: rt.state == "ready" && rt.server != nil,
			Error: rt.err, ScopedURL: "/x/" + id + "/",
			LastOpened: rt.project.LastOpened.UTC().Format(time.RFC3339Nano), Kind: "local",
		})
		return
	}
	if sub == "api/workspace/handoffs" && r.Method == http.MethodPost {
		h.createHandoff(w, r, id)
		return
	}
	if sub == "api/workspace/handoffs/redeem" && r.Method == http.MethodPost {
		h.redeemHandoff(w, r, id)
		return
	}
	if sub == "api/workspace/handoffs/bind" && r.Method == http.MethodPost {
		h.bindHandoff(w, r, id)
		return
	}
	if sub == "api/workspace/handoffs/complete" && r.Method == http.MethodPost {
		h.completeHandoff(w, r, id)
		return
	}
	if sub == "api/projects" || strings.HasPrefix(sub, "api/projects/") {
		clone := r.Clone(r.Context())
		clone.URL.Path = "/" + sub
		clone.RequestURI = clone.URL.RequestURI()
		h.ServeHTTP(w, clone)
		return
	}
	if sub == "api" || strings.HasPrefix(sub, "api/") {
		if rt.server == nil {
			h.writeError(w, http.StatusServiceUnavailable, rt.err)
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = "/" + sub
		clone.RequestURI = clone.URL.RequestURI()
		rt.server.Handler().ServeHTTP(w, clone)
		return
	}
	ServeInjectedIndex(w, r, h.static, "/x/"+id, false)
}

func workspaceSafeOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
}

func (h *WorkspaceHost) serveWorkspaceAsset(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean(r.URL.Path)
	if clean != "/" && clean != "." {
		rel := strings.TrimPrefix(clean, "/")
		if f, err := h.static.Open(rel); err == nil {
			_ = f.Close()
			http.FileServer(http.FS(h.static)).ServeHTTP(w, r)
			return
		}
	}
	ServeInjectedIndex(w, r, h.static, "", true)
}

func (h *WorkspaceHost) statuses() []WorkspaceRuntimeStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]WorkspaceRuntimeStatus, 0, len(h.registry.RecentProjects))
	for _, project := range h.registry.RecentProjects {
		rt := h.runtimes[project.ID]
		status := WorkspaceRuntimeStatus{
			ID: project.ID, Name: project.Name, Dir: project.Dir,
			StoreDir: project.StoreDir, State: "degraded",
			ScopedURL:  "/x/" + project.ID + "/",
			LastOpened: project.LastOpened.UTC().Format(time.RFC3339Nano), Kind: "local",
		}
		if rt != nil {
			status.Generation = rt.generation
			status.State = rt.state
			status.RuntimeReady = rt.state == "ready" && rt.server != nil
			status.Error = rt.err
		}
		out = append(out, status)
	}
	return out
}

func (h *WorkspaceHost) listProjects(w http.ResponseWriter) {
	h.writeJSON(w, h.statuses())
}

func (h *WorkspaceHost) currentProject(w http.ResponseWriter) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	project := h.registry.Current()
	if project == nil {
		h.writeJSON(w, nil)
		return
	}
	rt := h.runtimes[project.ID]
	status := WorkspaceRuntimeStatus{
		ID: project.ID, Name: project.Name, Dir: project.Dir,
		StoreDir: project.StoreDir, State: "degraded",
		ScopedURL:  "/x/" + project.ID + "/",
		LastOpened: project.LastOpened.UTC().Format(time.RFC3339Nano), Kind: "local",
	}
	if rt != nil {
		status.Generation, status.State, status.Error = rt.generation, rt.state, rt.err
		status.RuntimeReady = rt.state == "ready" && rt.server != nil
	}
	h.writeJSON(w, status)
}

func (h *WorkspaceHost) currentRuntime() *workspaceRuntime {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if current := h.registry.Current(); current != nil {
		return h.runtimes[current.ID]
	}
	return nil
}

func (h *WorkspaceHost) switchProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	h.mu.Lock()
	if !h.registry.SetCurrent(req.ID) {
		h.mu.Unlock()
		h.writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if err := h.registry.Save(); err != nil {
		h.mu.Unlock()
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.mu.Unlock()
	h.currentProject(w)
}

func (h *WorkspaceHost) addProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	root, err := filepath.Abs(strings.TrimSpace(req.Dir))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if resolved, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
		root = resolved
	}
	storeDir := runstore.ResolveStoreDir(root, "")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	envFile := ""
	if candidate := filepath.Join(root, ".env"); regularFile(candidate) {
		envFile = candidate
	}
	h.mu.Lock()
	project, _, err := h.registry.RegisterWithStore(root, storeDir, botregistry.DefaultPaths(root), envFile)
	if err == nil {
		err = h.registry.Save()
	}
	h.mu.Unlock()
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if existing := h.runtime(project.ID); existing == nil {
		h.startRuntime(project)
	}
	h.switchCurrent(project.ID)
	h.currentProject(w)
}

func regularFile(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.Mode().IsRegular()
}

func (h *WorkspaceHost) runtime(id string) *workspaceRuntime {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.runtimes[id]
}

func (h *WorkspaceHost) switchCurrent(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.registry.SetCurrent(id) {
		_ = h.registry.Save()
	}
}

func (h *WorkspaceHost) removeProject(w http.ResponseWriter, id string) {
	h.mu.Lock()
	rt := h.runtimes[id]
	if !h.registry.Remove(id) {
		h.mu.Unlock()
		h.writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if err := h.registry.Save(); err != nil {
		h.mu.Unlock()
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delete(h.runtimes, id)
	h.mu.Unlock()
	if rt != nil {
		h.stopRuntime(rt)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *WorkspaceHost) stopRuntime(rt *workspaceRuntime) {
	if rt.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_ = rt.server.Shutdown(ctx)
		cancel()
	}
	if rt.lease != nil {
		_ = rt.lease.Unlock()
	}
}

func (h *WorkspaceHost) Shutdown(ctx context.Context) error {
	if h.handoffCancel != nil {
		h.handoffCancel()
		h.handoffWG.Wait()
	}
	h.mu.Lock()
	runtimes := make([]*workspaceRuntime, 0, len(h.runtimes))
	for _, rt := range h.runtimes {
		runtimes = append(runtimes, rt)
	}
	h.runtimes = make(map[string]*workspaceRuntime)
	h.mu.Unlock()
	var first error
	for _, rt := range runtimes {
		if rt.server != nil {
			if err := rt.server.Shutdown(ctx); err != nil && first == nil {
				first = err
			}
		}
		if rt.lease != nil {
			if err := rt.lease.Unlock(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (h *WorkspaceHost) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (h *WorkspaceHost) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
