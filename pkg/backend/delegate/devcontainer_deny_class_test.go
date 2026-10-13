package delegate

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A sandboxed agent CLI reads its routing from the container's env, which a
// found devcontainer.json seeds from the reviewed repository. So every name
// this package knows a CLI to route a request or resolve a credential by is
// the operator's, and the devcontainer deny class must refuse it: the claude
// CLI's routing table, the cloud switches and channels a facade route clears
// or sets, pi's credential allowlist, its per-provider key table and the
// channels piResolveEnv writes, the CLI config homes. When a CLI release
// grows one of these tables, this test grows with it.
func TestDevcontainerDenyClassCoversEveryAgentRoutingName(t *testing.T) {
	t.Setenv("ITERION_PI_AGENT_DIR", t.TempDir())
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	sources := map[string][]string{
		"ClaudeCodeRoutingEnv":     ClaudeCodeRoutingEnv,
		"cloudProviderSwitches":    cloudProviderSwitches,
		"clearedAnthropicChannels": slices.Sorted(maps.Keys(clearedAnthropicChannels())),
		"facadeRouteEnv":           slices.Sorted(maps.Keys(facadeRouteEnv(secrets.ProviderZAI, "https://gateway.example", "key"))),
		"suppressAnthropicWireEnv": slices.Sorted(maps.Keys(suppressAnthropicWireEnv())),
		"directCredentialChannels": directCredentialChannels,
		"piCredentialEnvNames":     piCredentialEnvNames,
		"piEnvKeys":                slices.Sorted(maps.Values(piEnvKeys)),
		"piResolveEnv":             slices.Sorted(maps.Keys(piResolveEnv(context.Background()))),
	}
	for _, proto := range []CLIAgentProtocol{kimiProtocol, grokProtocol, piProtocol, opencodeProtocol} {
		// HOME belongs to the runtime's home pin, not to the class (#2318).
		if hook := proto.PermissionHook; hook != nil && hook.HomeEnv != "HOME" {
			sources[proto.Name+" PermissionHook.HomeEnv"] = []string{hook.HomeEnv}
		}
	}
	if len(sources["piResolveEnv"]) == 0 || len(sources[kimiProtocol.Name+" PermissionHook.HomeEnv"]) == 0 {
		t.Fatalf("sources = %v, want piResolveEnv and kimi's config home to name variables: the probe lost its subject", sources)
	}
	for _, source := range slices.Sorted(maps.Keys(sources)) {
		for _, name := range sources[source] {
			if !devcontainer.DeniedEnvKey(name) {
				t.Errorf("%s names %s, which a repository's devcontainer.json may still set in the sandbox: devcontainer.DeniedEnvKey must refuse it", source, name)
			}
		}
	}
}
