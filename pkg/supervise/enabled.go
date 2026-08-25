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
		return v, "--supervisors"
	}
	if v, ok := parseOnOff(os.Getenv(EnabledEnv)); ok {
		return v, EnabledEnv
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

// ValidateSupervisorsMode rejects a --supervisors value that is neither
// empty (inherit) nor on|off. A typo would otherwise read as "inherit"
// and silently keep supervisors an operator asked to disable.
func ValidateSupervisorsMode(v string) error {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "", "on", "off":
		return nil
	}
	return fmt.Errorf("invalid supervisors mode %q: expected on or off", v)
}
