package tool

import "testing"

func TestResolveWebSearchEnabled(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"default off, no backend", map[string]string{}, false},
		{"auto on with searxng", map[string]string{"SEARXNG_URL": "http://searx:8080"}, true},
		{"auto on with searxng endpoint alias", map[string]string{"SEARXNG_ENDPOINT": "http://searx:8080"}, true},
		{"auto on with brave key", map[string]string{"BRAVE_API_KEY": "k"}, true},
		{"explicit on with no backend (ddg allowed)", map[string]string{"ITERION_WEB_SEARCH": "on"}, true},
		{"explicit off masks configured backend", map[string]string{"ITERION_WEB_SEARCH": "off", "SEARXNG_URL": "http://searx:8080"}, false},
		{"auto keyword with backend", map[string]string{"ITERION_WEB_SEARCH": "auto", "BRAVE_API_KEY": "k"}, true},
		{"auto keyword without backend", map[string]string{"ITERION_WEB_SEARCH": "auto"}, false},
		{"empty env value ignored", map[string]string{"SEARXNG_URL": "  "}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear all relevant envs, then set the case's.
			for _, k := range []string{"ITERION_WEB_SEARCH", "SEARXNG_URL", "SEARXNG_ENDPOINT", "BRAVE_API_KEY"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := ResolveWebSearchEnabled(); got != tt.want {
				t.Errorf("ResolveWebSearchEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
