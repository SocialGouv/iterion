package model

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The two lists must not drift apart: every endpoint-shaped name on the
// forwarding list must appear in the explicit-empty list, or the next
// endpoint added to providerCredentialEnvVars silently reopens the .env
// road this fix closes.
func TestProviderEndpointEnvVarsCoversEveryEndpointOnTheForwardList(t *testing.T) {
	for _, name := range providerCredentialEnvVars {
		if !strings.HasSuffix(name, "_BASE_URL") && !strings.HasSuffix(name, "_ENDPOINT") {
			continue
		}
		if !slices.Contains(providerEndpointEnvVars, name) {
			t.Errorf("%s is on the forwarding list and endpoint-shaped, but not in providerEndpointEnvVars — a workspace .env picks where it dials", name)
		}
	}
}

// The runner auto-loads the workspace .env, which only fills names MISSING
// from the process env (applyDotEnv honours LookupEnv). An endpoint name the
// host leaves unset must therefore cross as an explicit empty — never as an
// absent name, or the repository under review picks which infrastructure a
// forwarded credential dials.
func TestForwardableProviderEnv_ForwardsEmptyEndpoints(t *testing.T) {
	for _, name := range providerEndpointEnvVars {
		t.Setenv(name, "")
	}
	// Keys keep the skip: an empty key reads as "auth attempted, invalid",
	// not "no auth" — absent stays the honest shape for them.
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	env, err := forwardableProviderEnv(context.Background(), "openai/gpt-6")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	for _, name := range providerEndpointEnvVars {
		v, ok := env[name]
		if !ok {
			t.Errorf("%s crossed ABSENT — the workspace .env fills it and picks the endpoint a forwarded credential dials", name)
			continue
		}
		if v != "" {
			t.Errorf("%s crossed as %q, want the explicit empty", name, v)
		}
	}
	for _, name := range []string{"XAI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if _, ok := env[name]; ok {
			t.Errorf("%s crossed empty — providers read that as an invalid auth attempt, not as no auth", name)
		}
	}
}

// A host-POSED endpoint crosses with its value: the explicit-empty path is
// only for names the host left unset.
func TestForwardableProviderEnv_HostSetEndpointStillCrosses(t *testing.T) {
	for _, name := range providerEndpointEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("XAI_BASE_URL", "https://operator-proxy.example/v1")
	t.Setenv("XAI_API_KEY", "sk-x")

	env, err := forwardableProviderEnv(context.Background(), "xai/grok-4")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if env["XAI_BASE_URL"] != "https://operator-proxy.example/v1" {
		t.Errorf("XAI_BASE_URL crossed as %q, want the host's pose", env["XAI_BASE_URL"])
	}
	if env["XAI_API_KEY"] != "sk-x" {
		t.Error("XAI_API_KEY did not cross")
	}
}
