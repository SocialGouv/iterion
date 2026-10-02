package ir

// Per-node CLI-command diagnostics.
const (
	DiagCommandIgnored DiagCode = "C174" // `command:` set on a backend that does not consume it (warning)
)

// commandIgnoringBackends are the explicitly-named backends that do NOT
// honor the per-node `command:` CLI-binary override. Only claude_code
// swaps its CLI binary (default `claude`) for the given command (an
// alternate claude-code-compatible CLI): it is the sole reader of
// delegate.Task.Command. claw makes a direct API call (no CLI); codex
// resolves its own binary; and every CLI-agent backend resolves argv[0]
// from the binary its REGISTRY entry was constructed with, which the
// default registry builds empty — so a `command:` on any of them is inert.
var commandIgnoringBackends = map[string]bool{
	"claw":     true,
	"codex":    true,
	"kimi":     true,
	"grok":     true,
	"pi":       true,
	"opencode": true,
}

// validateCommand walks every LLM-capable node (agent, judge) and warns
// when the per-node `command:` CLI-binary override is set on a backend
// that cannot consume it:
//
//   - C174 (warning) when `command:` is non-empty AND the effective
//     backend is one of commandIgnoringBackends above. Only claude_code
//     honors the override.
//
// The effective backend mirrors the runtime precedence knowable at compile
// time: the node's own `backend:`, falling back to the workflow-level
// `default_backend:` when the node leaves it empty/`auto` (see
// ClawExecutor.resolveBackendName). It is deliberately conservative about
// what it CANNOT know: an env-ref backend (${VAR}) and a backend that stays
// empty after the default_backend fallback (deferred to
// ITERION_DEFAULT_BACKEND / host credential auto-detection) are skipped —
// the literal text isn't the resolved backend, so warning there would
// misfire. The check is a warning, never an error: the run still proceeds
// and the override is simply ignored downstream.
func (c *compiler) validateCommand(w *Workflow) {
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		f := nn.GetLLMFields()
		if f.Command == "" {
			continue
		}
		// The effective backend, read as the run reads it — the node's own
		// `backend:` with its `${VAR:-default}` resolved, falling back to
		// the workflow default when it answers nothing. The one reading
		// every backend screen shares (effectiveNodeBackend); "" is the
		// run's decision to make.
		backend := effectiveNodeBackend(f.Backend, w.DefaultBackend)
		if backend == "" {
			continue
		}
		if commandIgnoringBackends[backend] {
			c.warnfAt(DiagCommandIgnored, nn.NodeID(), "",
				"%s %q: command %q has no effect on backend=%q (only claude_code honors the per-node CLI-binary override); it will be ignored",
				nn.NodeKind().String(), nn.NodeID(), f.Command, backend)
		}
	}
}
