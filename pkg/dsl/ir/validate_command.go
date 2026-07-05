package ir

import "strings"

// Per-node CLI-command diagnostics.
const (
	DiagCommandIgnored DiagCode = "C174" // `command:` set on a backend that does not consume it (warning)
)

// commandIgnoringBackends are the explicitly-named backends that do NOT
// honor the per-node `command:` CLI-binary override. Only claude_code
// swaps its CLI binary (default `claude`) for the given command (e.g.
// `kimi`); claw makes a direct API call (no CLI) and codex resolves its
// own binary, so a `command:` there is inert.
var commandIgnoringBackends = map[string]bool{
	"claw":  true,
	"codex": true,
}

// validateCommand walks every LLM-capable node (agent, judge) and warns
// when the per-node `command:` CLI-binary override is set on a backend
// that cannot consume it:
//
//   - C174 (warning) when `command:` is non-empty AND the backend is
//     explicitly `claw` or `codex`. Only claude_code honors the override.
//
// It is deliberately conservative: an empty/`auto` backend (deferred to
// runtime auto-resolution) and any env-ref backend (${VAR}, resolved only
// at run time) are skipped — the literal text isn't the resolved backend,
// so warning there would misfire. The check is a warning, never an error:
// the run still proceeds and the override is simply ignored downstream.
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
		backend := f.Backend
		// Empty/auto backend and env-ref forms resolve at run time; the
		// literal text isn't the resolved backend, so defer to runtime.
		if backend == "" || backend == "auto" || strings.Contains(backend, "${") {
			continue
		}
		if commandIgnoringBackends[backend] {
			c.warnfAt(DiagCommandIgnored, nn.NodeID(), "",
				"%s %q: command %q has no effect on backend=%q (only claude_code honors the per-node CLI-binary override); it will be ignored",
				nn.NodeKind().String(), nn.NodeID(), f.Command, backend)
		}
	}
}
