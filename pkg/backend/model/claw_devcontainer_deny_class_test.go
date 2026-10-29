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
func TestDevcontainerDenyClassCoversEveryNameTheRunnerIsHanded(t *testing.T) {
	t.Parallel()
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
			if !devcontainer.DeniedEnvKey(name) {
				t.Errorf("%s names %s, which a repository's devcontainer.json may still set for the in-container runner: devcontainer.DeniedEnvKey must refuse it", source, name)
			}
		}
	}
}
