package model

import (
	"maps"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// The in-container runner rebuilds its registry from the env that crossed
// and, for every name the host left unset, from the container's own env,
// which a found devcontainer.json seeds from the reviewed repository:
// forwardableProviderEnv copies a host value only when the host has one. So
// every name the runner is handed — forwarded, set by a branch of the
// crossing, the variable the run's own key crosses as, and every name this
// package WRITES in an env it hands on (the crossing, a tool node's command,
// a settings hook's, including the permission gate's payload) — is the
// operator's, and the devcontainer deny class must refuse it, or a planted
// value answers wherever the host said nothing.
// denyClassOwed names what the deny class does not refuse YET, with the
// reason it is safe for the node the host dispatches and the processes it is
// still owed for. The entry is self-expiring: the probe below fails as soon
// as the deny class does refuse the name, so it cannot outlive its subject.
//
// The gateway catalog's two local sources (#2844): the crossing writes both
// as explicit empties for every gateway node, and ExecOpts.Env outranks the
// environment the container was started with, so a devcontainer.json value
// does not reach a gateway node's runner. It does reach the OTHER processes
// in that container — a tool node's command, an `iterion run` one of them
// launches — which resolve a gateway id from the container's own env.
// pkg/sandbox/devcontainer's tables are being rewritten in the same round
// (the repo-authored image env), so the entry lands on top of that rewrite
// rather than conflicting with it.
var denyClassOwed = map[string]string{
	"OPENAI_COMPATIBLE_MODELS":           "#2844: crossed as an explicit empty for every gateway node, still owed for the container's other processes",
	"OPENAI_COMPATIBLE_CATALOG_PROVIDER": "#2844: crossed as an explicit empty for every gateway node, still owed for the container's other processes",
}

func TestDevcontainerDenyClassCoversEveryNameTheRunnerIsHanded(t *testing.T) {
	t.Parallel()
	for name, why := range denyClassOwed {
		if devcontainer.DeniedEnvKey(name) {
			t.Errorf("denyClassOwed lists %s (%s), which the deny class now refuses: drop the entry", name, why)
		}
	}
	sources := map[string][]string{
		"providerCredentialEnvVars": providerCredentialEnvVars,
		"providerRoutingEnvVars":    providerRoutingEnvVars,
		"byokEnvVar":                slices.Sorted(maps.Values(byokEnvVar)),
		"crossedByItsOwnBranch":     slices.Sorted(maps.Keys(crossedByItsOwnBranch)),
	}
	sources["envWrites"] = slices.Sorted(maps.Keys(envWrites))
	if len(sources["envWrites"]) == 0 {
		t.Fatal("envWrites names no variable: the probe lost its subject")
	}
	for _, source := range slices.Sorted(maps.Keys(sources)) {
		for _, name := range sources[source] {
			if _, owed := denyClassOwed[name]; owed {
				continue
			}
			if !devcontainer.DeniedEnvKey(name) {
				t.Errorf("%s names %s, which a repository's devcontainer.json may still set for the in-container runner: devcontainer.DeniedEnvKey must refuse it, or name the gap and its reason in denyClassOwed", source, name)
			}
		}
	}
}
