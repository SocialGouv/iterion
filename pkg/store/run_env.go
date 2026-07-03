package store

import (
	"os"
	"strings"
)

// launchEnvPrefixes is the allowlist of env-var name prefixes whose
// values get captured in Run.LaunchEnv at run creation. Anything
// outside this list is excluded — operators don't want their PATH /
// HOME / SHELL leaking into run records, and the iterion-specific
// + secrets-related vars are the ones that actually drive run
// behaviour (model choice, reasoning effort, rescue provider, etc.).
//
// "ITERION_" covers every recipe knob (ITERION_RENOVACY_MODEL_*,
// ITERION_RENOVACY_EFFORT_*, ITERION_LOG_LEVEL, ITERION_DETACHED, …).
// "RESCUE_" covers RESCUE_PROVIDER (used by the secured-renovacy
// recipe to route claude_code through zai vs. anthropic-direct).
//
// Add prefixes here when a new recipe declares its own env-tunable
// knob; the corresponding values will then start appearing in
// Run.LaunchEnv automatically.
var launchEnvPrefixes = []string{
	"ITERION_",
	"RESCUE_",
}

// secretValuePatterns are name substrings that mark a variable as
// secret. Names containing any of these get the VALUE redacted in
// LaunchEnv ("[redacted]"); the KEY is still recorded so the operator
// can see WHICH credentials the run used (auditability) without
// leaking the secret itself onto disk. Substring matching is
// case-insensitive.
var secretValuePatterns = []string{
	"TOKEN",
	"KEY",
	"SECRET",
	"PASSWORD",
	"CREDENTIAL",
	"AUTH",
}

// CaptureLaunchEnv snapshots the iterion-relevant env vars active at
// run creation. Returns a map suitable for Run.LaunchEnv. Values are
// redacted when the variable name suggests it's a secret (see
// secretValuePatterns) — name kept, value replaced with "[redacted]"
// so audits remain useful without leaking credentials onto disk.
//
// Returns nil when no captured vars are set, keeping legacy run
// records identical (no extra empty map).
func CaptureLaunchEnv() map[string]string {
	out := make(map[string]string)
	for _, raw := range os.Environ() {
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 {
			continue
		}
		name := raw[:eq]
		value := raw[eq+1:]
		if value == "" {
			continue // empty == unset for our purposes; noise otherwise
		}
		if !hasCapturedPrefix(name) {
			continue
		}
		if IsSecretEnvName(name) {
			out[name] = "[redacted]"
		} else {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func hasCapturedPrefix(name string) bool {
	for _, p := range launchEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// IsSecretEnvName reports whether an environment variable name looks
// like it holds a secret (case-insensitive substring match against
// secretValuePatterns). Exported so pkg/backend/secretguard can seed
// its value-taint set from the host's sensitive env vars using the
// same definition this package uses for LaunchEnv redaction.
func IsSecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range secretValuePatterns {
		if strings.Contains(upper, p) {
			return true
		}
	}
	return false
}
