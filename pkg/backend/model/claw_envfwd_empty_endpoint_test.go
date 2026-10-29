package model

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
)

// The two lists must not drift apart: every endpoint-shaped name on the
// forwarding list must appear in the explicit-empty list, or the next
// endpoint added to providerCredentialEnvVars silently reopens the .env
// road this fix closes. The routing names that are not endpoint-shaped —
// a deployment, a project, a region, an API version — are held to the same
// rule by their own derivation (TestEveryVendoredProviderEnvNameIsDecided).
func TestProviderRoutingEnvVarsCoversEveryEndpointOnTheForwardList(t *testing.T) {
	for _, name := range providerCredentialEnvVars {
		if !strings.HasSuffix(name, "_BASE_URL") && !strings.HasSuffix(name, "_ENDPOINT") {
			continue
		}
		if !slices.Contains(providerRoutingEnvVars, name) {
			t.Errorf("%s is on the forwarding list and endpoint-shaped, but not in providerRoutingEnvVars — a workspace .env picks where it dials", name)
		}
	}
}

// The runner auto-loads the workspace .env, which only fills names MISSING
// from the process env (applyDotEnv honours LookupEnv). An endpoint name the
// host leaves unset must therefore cross as an explicit empty — never as an
// absent name, or the repository under review picks which infrastructure a
// forwarded credential dials.
func TestForwardableProviderEnv_ForwardsEmptyEndpoints(t *testing.T) {
	for _, name := range providerRoutingEnvVars {
		t.Setenv(name, "")
	}
	// Keys keep the skip: an empty key reads as "auth attempted, invalid",
	// not "no auth" — absent stays the honest shape for them.
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	env, _, err := forwardableProviderEnv(context.Background(), "openai/gpt-6")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	for _, name := range providerRoutingEnvVars {
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
	for _, name := range providerRoutingEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("XAI_BASE_URL", "https://operator-proxy.example/v1")
	t.Setenv("XAI_API_KEY", "sk-x")

	env, _, err := forwardableProviderEnv(context.Background(), "xai/grok-4")
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

// The forwarded catalog record is the endpoint empties' twin: the host
// writes ITERION_OPENAI_COMPATIBLE_RESOLVED for every gateway node it
// dispatches, an EMPTY record when its own answer is unknown. Absent, the
// name is filled by the workspace .env of the repository under review —
// and that record is what the in-container catalog answers from FIRST, so
// the repository would pick the window its reviewer compacts at, the
// max_tokens it asks for, the price it is billed at and the provenance all
// of it is recorded under.
func TestForwardableProviderEnv_ForwardsAnEmptyGatewayRecordWhenTheHostDoesNotKnow(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_MODELS", "")
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	t.Setenv(compatgw.ResolvedEnvName, "")
	compatgw.ResetCatalogCaches()

	env, _, err := forwardableProviderEnv(context.Background(), "openai_compatible/no-table-has-this-id")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	raw, ok := env[compatgw.ResolvedEnvName]
	if !ok {
		t.Fatalf("%s crossed ABSENT for an id the host does not know — the workspace .env fills it and the repository under review writes the record its reviewer's node runs on", compatgw.ResolvedEnvName)
	}
	if raw != "" {
		t.Errorf("%s crossed as %q, want the explicit empty: the host resolved nothing for this id", compatgw.ResolvedEnvName, raw)
	}
}
