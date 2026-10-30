package model

import "os"

// forwardableToolEnv builds the env claw's vendored TOOL packages read inside
// the container for a sandboxed node — the sibling of forwardableProviderEnv
// for the names the tool packages spell, not the provider ones (#2802). The
// derivation over the vendored tool sources and the verdict of every name
// they read live in claw_vendored_tool_env_test.go, which turns red the day
// claw's tools read a name that nothing decides.
//
// Only names classified "forwarded" appear, and only when the launcher sets
// them: an absent name leaves the operator's own sandbox env (a bot's
// `sandbox.env:`, a file secret's `env:`) its authority, which an explicit
// empty would silently replace. What a sandbox-placed tool reads in the
// container when the launcher never set the name is claw's default, the same
// default the in-process node would read on an unset launcher — no second
// contract.
//
//   - CLAW_BASH_TIMEOUT crosses as-is: a duration dial, not a path — the
//     deployment's bash budget, spent where bash runs, which for a sandboxed
//     node is the container (bash is tool.PlacementSandbox and the runner
//     registers it locally). Same shape as the ITERION_CLAW_STREAM_* knobs
//     in providerCredentialEnvVars.
//   - CLAW_CONFIG_HOME crosses as-is: the skill tool's lookup reads it in the
//     container (skill is sandbox-placed, registered by the runner), and no
//     iterion-seeded mount serves a claw config home, so there is nothing to
//     rewrite to. Where the sandbox binds the host (host_state) the path
//     resolves and the in-container skill lookup sees the same roots as the
//     host's; where it does not, claw skips a root that does not exist.
//
// The credential-bearing config dirs the same lookup also reads — CODEX_HOME,
// CLAUDE_CONFIG_DIR — are NOT here: they cross only rewritten to the seeded
// forfait mounts (applyForfaitAcrossSandbox, deliverHostCodexForfait), never
// as the launcher's path. DISPLAY / WAYLAND_DISPLAY are undecided (the
// Jo-gated question on #2802) and cross nowhere.
func forwardableToolEnv() map[string]string {
	env := map[string]string{}
	// Spelled literally at each read: the package's env-read guard
	// (claw_sandbox_env_class_test.go) classifies names it can SEE.
	if v := os.Getenv("CLAW_BASH_TIMEOUT"); v != "" {
		env["CLAW_BASH_TIMEOUT"] = v
	}
	if v := os.Getenv("CLAW_CONFIG_HOME"); v != "" {
		env["CLAW_CONFIG_HOME"] = v
	}
	return env
}
