package supervise

import (
	"fmt"
	"os"
	"strings"
)

// EnabledEnv is the machine-wide toggle for spawning DSL-declared
// supervisors (`supervisor NAME:` blocks). It sits below the run-level
// override in the precedence chain and above the built-in default (on).
const EnabledEnv = "ITERION_SUPERVISORS"

// DeclaredEnabled resolves whether DSL-declared supervisors spawn for a
// run: run-level override (CLI --supervisors) → ITERION_SUPERVISORS →
// on. The returned source names the layer that decided, so a skip can be
// logged with its cause instead of silently dropping a declared
// capability.
//
// It defaults ON: a bot declares a supervisor because the run benefits
// from it; turning it off is the operator's escape hatch (cost control,
// or isolating a supervisor suspected of steering a run astray).
func DeclaredEnabled(override string) (enabled bool, source string) {
	if v, ok := parseOnOff(override); ok {
		// The override arrives from --supervisors, the launch API field,
		// or the queue envelope — name the level, not one surface.
		return v, "run-level override"
	}
	if env := strings.TrimSpace(os.Getenv(EnabledEnv)); env != "" {
		if v, ok := parseOnOff(env); ok {
			return v, EnabledEnv
		}
		// An unreadable env value falls through to the default — flag it
		// rather than silently spawning what the operator meant to
		// disable (the failure direction here spends LLM budget). %q so
		// a value carrying newlines cannot forge log lines.
		return true, fmt.Sprintf("%s unreadable (%q) — default on", EnabledEnv, env)
	}
	return true, "default"
}

// parseOnOff reads one layer of the chain. ok=false means the layer is
// unset and the next one decides. The extra spellings are accepted from
// the env, where operators reach for 0/1.
func parseOnOff(v string) (enabled, ok bool) {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "off", "0", "false", "no":
		return false, true
	case "on", "1", "true", "yes":
		return true, true
	}
	return false, false
}

// ValidateSupervisorsMode rejects a --supervisors value that neither is
// empty (inherit) nor parses as a boolean spelling. A typo would
// otherwise read as "inherit" and silently keep supervisors an operator
// asked to disable. The accepted set is exactly parseOnOff's — the same
// grammar ITERION_SUPERVISORS reads, so a wrapper forwarding the env
// value as the flag cannot break.
func ValidateSupervisorsMode(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if _, ok := parseOnOff(v); ok {
		return nil
	}
	return fmt.Errorf("invalid supervisors mode %q: expected on or off", v)
}
