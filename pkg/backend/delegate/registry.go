package delegate

import (
	"fmt"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Registry maps backend names to Backend implementations.
type Registry struct {
	backends map[string]Backend
}

// NewRegistry creates an empty delegation backend registry.
func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]Backend)}
}

// Register adds a backend under the given name.
func (r *Registry) Register(name string, b Backend) {
	r.backends[name] = b
}

// Resolve looks up a backend by name. Returns an error if not found.
func (r *Registry) Resolve(name string) (Backend, error) {
	b, ok := r.backends[name]
	if !ok {
		return nil, fmt.Errorf("delegate: unknown backend %q", name)
	}
	return b, nil
}

// DefaultRegistry returns a registry pre-loaded with the standard
// claude_code and codex backends.
func DefaultRegistry(logger *iterlog.Logger) *Registry {
	r := NewRegistry()
	r.Register(BackendClaudeCode, &ClaudeCodeBackend{Logger: logger})
	r.Register(BackendCodex, &CodexBackend{Logger: logger})
	return r
}
