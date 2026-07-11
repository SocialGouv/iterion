package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

// handlePluginsList answers GET /api/v1/plugins — the loaded plugin registry
// (embedded builtins + ~/.iterion/plugins) with each plugin's enable state and
// contribution kinds. Backs the studio Plugins management view.
func (s *Server) handlePluginsList(w http.ResponseWriter, r *http.Request) {
	reg, err := plugin.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"plugins": reg.Views()})
}

// handlePluginEnable answers POST /api/v1/plugins/{name}/enable and
// .../disable, persisting the decision to ~/.iterion/plugins.yaml.
func (s *Server) handlePluginEnable(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "plugin name required", http.StatusBadRequest)
			return
		}
		reg, err := plugin.Load()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := reg.SetEnabled(name, enabled); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.writeJSONFor(w, r, map[string]any{"name": name, "enabled": enabled})
	}
}

// handlePluginInstall answers POST /api/v1/plugins/install — installs a plugin
// from a git URL or local path into ~/.iterion/plugins/. Super-admin only (the
// route is wrapped in requireSuperAdmin), because it clones an arbitrary source
// server-side and mutates the shared plugin tree. Returns the installed
// plugin's view so the studio can update its list without a full refetch.
func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	src := strings.TrimSpace(body.Source)
	if src == "" {
		http.Error(w, "plugin source (git URL or path) required", http.StatusBadRequest)
		return
	}
	name, err := plugin.Install(r.Context(), src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Reload so the freshly-installed plugin is in the registry, then return its
	// view (enabled state resolved, kinds populated).
	reg, err := plugin.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{"name": name}
	if p, ok := reg.Get(name); ok {
		resp["plugin"] = p.View()
	}
	s.writeJSONFor(w, r, resp)
}

// handlePluginUninstall answers DELETE /api/v1/plugins/{name} — removes an
// installed plugin. Super-admin only. Builtins cannot be removed (plugin.Uninstall
// rejects them with a 400-worthy error; disable them instead).
func (s *Server) handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "plugin name required", http.StatusBadRequest)
		return
	}
	if err := plugin.Uninstall(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"name": name, "removed": true})
}

// handlePluginConfig answers PUT /api/v1/plugins/{name}/config — persists the
// operator's config values for a plugin (like saving a Firefox add-on's
// preferences). Super-admin only. Only fields declared in the manifest are
// accepted; a secret field submitted empty keeps its prior value ("leave blank
// to keep"). Returns the refreshed view (with masked secret values).
func (s *Server) handlePluginConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "plugin name required", http.StatusBadRequest)
		return
	}
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	reg, err := plugin.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, ok := reg.Get(name); !ok {
		http.Error(w, "plugin not found", http.StatusNotFound)
		return
	}
	// ApplyConfig accepts only declared fields and keeps a secret left blank.
	if err := reg.ApplyConfig(name, body.Values); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	view, _ := reg.ViewFor(name)
	s.writeJSONFor(w, r, view)
}

// handlePluginDetail answers GET /api/v1/plugins/{name} — the full detail
// projection of one plugin (README, rewriters, MCP servers, mirrored files,
// hook commands, lifecycle). Read-only, same open access as the list route.
func (s *Server) handlePluginDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "plugin name required")
		return
	}
	reg, err := plugin.Load()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	if _, ok := reg.Get(name); !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "plugin %q not found", name)
		return
	}
	detail, err := reg.DetailFor(name)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	s.writeJSONFor(w, r, detail)
}

// lifecycleOutputCap bounds the subprocess output echoed back to the studio —
// a runaway indexer must not stream unbounded bytes to the client.
const lifecycleOutputCap = 64 << 10

// lifecycleStreamWriter turns subprocess output into flushed NDJSON
// {"output": …} lines so the studio renders progress live, capped at
// lifecycleOutputCap (beyond: discarded, flagged on the trailer). It is
// passed as BOTH Stdout and Stderr of the lifecycle command; os/exec
// serializes Writes when the two are the same comparable writer, so no
// lock is needed. It never returns a write error, so a client that
// disconnects mid-stream never kills the subprocess.
type lifecycleStreamWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	written   int
	truncated bool
}

func (b *lifecycleStreamWriter) Write(p []byte) (int, error) {
	if remaining := lifecycleOutputCap - b.written; remaining > 0 {
		chunk := p
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
			b.truncated = true
		}
		b.written += len(chunk)
		b.writeLine(map[string]any{"output": string(chunk)})
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

// writeLine marshals one NDJSON event and flushes it to the client.
func (b *lifecycleStreamWriter) writeLine(v map[string]any) {
	line, err := json.Marshal(v)
	if err != nil {
		// Maps of strings/bools cannot fail to marshal; guard anyway.
		return
	}
	_, _ = b.w.Write(append(line, '\n'))
	if b.flusher != nil {
		b.flusher.Flush()
	}
}

// handlePluginLifecycle answers POST /api/v1/plugins/{name}/lifecycle/{phase}
// (phase: index|refresh) — runs the plugin's manifest lifecycle command in the
// server's workspace. Local-mode only (the command is arbitrary manifest shell
// executed server-side) and super-admin gated via the route wrapper. Setup
// problems (unknown plugin, no lifecycle block, empty phase command) are 4xx;
// a command that RAN and failed is a 200 with ok:false so the studio can show
// the captured output either way.
func (s *Server) handlePluginLifecycle(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "plugin lifecycle is not available in cloud mode")
		return
	}
	if s.cfg.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "no workspace configured: the lifecycle command executes in the workspace")
		return
	}
	phase := r.PathValue("phase")
	if phase != "index" && phase != "refresh" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "unknown lifecycle phase %q (want index|refresh)", phase)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "plugin name required")
		return
	}
	reg, err := plugin.Load()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	p, ok := reg.Get(name)
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "plugin %q not found", name)
		return
	}
	// Validate the manifest declares this phase BEFORE running, so setup
	// errors surface as 4xx instead of a false "command failed" result.
	lc := p.Manifest.Contributes.Lifecycle
	if lc == nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "plugin %q has no lifecycle commands", name)
		return
	}
	cmd := lc.Index
	if phase == "refresh" {
		cmd = lc.Refresh
	}
	if strings.TrimSpace(cmd) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "plugin %q has no %q command", name, phase)
		return
	}
	// Stream: each output chunk is an NDJSON {"output":…} line flushed as
	// it arrives, closed by a {"done":true,…} trailer — the studio renders
	// progress live instead of waiting on a one-shot blob. Setup errors
	// above stay plain-JSON 4xx (the client checks res.ok before reading
	// the stream).
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	out := &lifecycleStreamWriter{w: w, flusher: flusher}
	runErr := plugin.RunLifecycle(r.Context(), reg, name, phase, s.cfg.WorkDir, out, out)
	trailer := map[string]any{
		"done":  true,
		"name":  name,
		"phase": phase,
		"ok":    runErr == nil,
	}
	if out.truncated {
		trailer["truncated"] = true
	}
	if runErr != nil {
		trailer["error"] = runErr.Error()
	}
	out.writeLine(trailer)
}
