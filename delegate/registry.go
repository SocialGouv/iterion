package delegate

import "fmt"

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
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("claude_code", &ClaudeCodeBackend{})
	r.Register("codex", &CodexBackend{})
	// NOTE: ClawBackend requires a ModelFactory and is not registered by default.
	// Callers should register it explicitly: r.Register("claw", &ClawBackend{ModelFactory: ...})
	return r
}
