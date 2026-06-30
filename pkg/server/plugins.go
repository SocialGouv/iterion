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
	p, ok := reg.Get(name)
	if !ok {
		http.Error(w, "plugin not found", http.StatusNotFound)
		return
	}
	// Merge submitted values over the stored ones, accepting only declared
	// fields and keeping a secret whose submission is blank.
	merged := reg.StoredConfig(name)
	for _, f := range p.Manifest.Config {
		v, sent := body.Values[f.Key]
		if !sent {
			continue
		}
		if f.Type == "secret" && strings.TrimSpace(v) == "" {
			continue
		}
		merged[f.Key] = v
	}
	if err := reg.SetConfig(name, merged); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	view, _ := reg.ViewFor(name)
	s.writeJSONFor(w, r, view)
}
