// Package isolationenv is the ONE reader of every environment variable that
// can LOWER the isolation of the operator's run.
//
// iterion fills unset variables from the nearest `.env` walking up from the
// working directory (cmd/iterion applyDotEnv, no denylist). That file sits in
// the repository — including the one under review, which the sandboxed agent
// of a previous run had as its workspace. So a name that decides how isolated
// the operator's next run is must be read as INHERITED, never live: a planted
// value disarms the control from inside its own blast radius.
//
// [Names] is the inventory, and membership is decided by ONE question asked of
// the real read: can a value a project `.env` plants make the operator's run
// LESS isolated than it would be with the variable unset? That is why the
// family's other variables are not here, each for a stated reason:
//
//   - ITERION_SANDBOX_HOST_STATE — `none` drops every host bind, which is MORE
//     isolation, and `auto` is already the default, so a plant cannot lower it;
//   - ITERION_SANDBOX_TLS_INSPECT — `off` makes the proxy a transparent tunnel
//     AND makes the egress secret grant refuse (pkg/runtime/secret_grants.go),
//     so the run carries fewer secrets, not more;
//   - ITERION_SANDBOX_INSPECT_MAX_BODY — a smaller bound REFUSES the oversized
//     body with 413 rather than letting it through un-inspected;
//   - ITERION_SANDBOX_PREFLIGHT — default off, so a plant can only add checks;
//   - ITERION_SANDBOX_PERSIST_NIX — default on, so a plant can only drop the
//     shared nix store volume;
//   - ITERION_SANDBOX_REAP_INTERVAL, the K8S_* knobs and every timeout — they
//     move scheduling, resources and patience, never what a run can reach;
//   - ITERION_SANDBOX_PROXY_ENDPOINT — read INSIDE the sandbox by the gateway
//     client, about its own egress, not about the host's exposure.
//
// Two members of the class are already read inherited-only by the code that
// owns them, and are named here so the inventory is complete rather than
// re-plumbed: ITERION_HOME (store.InheritedIterionDataDir, which decides which
// home the sandbox mounts) and HOME (resolveHostHomeDir, which decides whose
// state it mounts).
package isolationenv

import "github.com/SocialGouv/iterion/internal/envtrust"

// The names, each a tier or a switch of the isolation a run resolves to.
const (
	// SandboxDefault is the GLOBAL default mode. "none" resolves the run
	// unsandboxed — and it is the tier the #1959 floor falls through TO once
	// it has removed a bot's own opt-out, so a plant here turns the floor
	// into a no-op that still fires its event.
	SandboxDefault = "ITERION_SANDBOX_DEFAULT"
	// SandboxOverride is the CLI-strength override. "none" resolves the run
	// unsandboxed from above the workflow, and it also makes ApplySandboxFloor
	// return early (an operator's explicit choice is never replaced).
	SandboxOverride = "ITERION_SANDBOX_OVERRIDE"
	// SandboxDefaultImage is the image `sandbox: auto` starts when the
	// repository ships no devcontainer. The image is the code that runs with
	// the operator's host state mounted: choosing it is choosing the agent.
	SandboxDefaultImage = "ITERION_SANDBOX_DEFAULT_IMAGE"
	// SandboxFloor=off honours every bot source's `sandbox: none` again.
	SandboxFloor = "ITERION_SANDBOX_FLOOR"
	// HostStateGuard=off restores the pre-#2069 posture where every host-state
	// bind is read-write.
	HostStateGuard = "ITERION_SANDBOX_HOST_STATE_GUARD"
	// ModelHosts decides which hosts the inspecting proxy treats as model
	// APIs — whose bodies keep their placeholders. An entry, or a `!`
	// exclusion, therefore decides whether a host receives the run's REAL
	// secrets substituted into its request body.
	ModelHosts = "ITERION_SANDBOX_MODEL_HOSTS"
)

// Names is the inventory the guard test holds the tree to: every name here is
// read through [Value] and nowhere else.
var Names = []string{
	SandboxDefault,
	SandboxDefaultImage,
	SandboxFloor,
	HostStateGuard,
	ModelHosts,
	SandboxOverride,
}

// Covers reports whether name is in the inventory.
func Covers(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}

// Value returns the variable as the process INHERITED it — the empty string
// when a project `.env` is what set it — and whether it was planted.
//
// The empty string means "the operator said nothing about this", which every
// caller must read as the closed answer rather than as a default. planted is
// returned rather than logged here: the warn belongs where a logger exists and
// where the ignored value would have CHANGED the outcome, so the line names a
// consequence instead of a setting.
func Value(name string) (value string, planted bool) {
	return envtrust.Inherited(name), envtrust.Planted(name)
}

// PlantedWarning is the one sentence every member of the class says when a
// `.env` planted it: what was ignored, and where the only place it is read
// from is.
func PlantedWarning(name string) string {
	return name + " was set by a project .env, not by the operator — the value is IGNORED and the isolation control stays at the operator's own answer; export it in the operator's own environment for it to count"
}
