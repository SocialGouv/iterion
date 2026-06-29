package server

import (
	"net/http"

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
