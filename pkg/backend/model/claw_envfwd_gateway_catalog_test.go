package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
)

// The forged table the repository under review writes: a 4096-token window,
// a one-token output budget and a near-zero price, the numbers the H-10
// probe measured in the container (#2844).
const forgedOperatorTable = `{"gw/m":{"context_window":4096,"max_output_tokens":1,"input_usd_per_mtok":0.000001,"output_usd_per_mtok":0.000001}}`

// hostGatewayEnv is a host serving a gateway whose id it resolves from
// nothing: the endpoint and the key it has, neither local catalog source
// set. This is the state the ticket is filed on — the host's answer is
// unknown, so its record crosses empty.
func hostGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv(compatgw.CatalogModelsEnv, "")
	t.Setenv(compatgw.CatalogProviderEnv, "")
	t.Setenv(compatgw.BaseURLEnv, "https://198.51.100.7")
	t.Setenv(compatgw.APIKeyEnv, "sk-host")
	t.Setenv(compatgw.ResolvedEnvName, "")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
}

// The record is not the only road to the resolution it carries: the two
// LOCAL sources of the very same answer — the operator table and the
// models.dev qualifier — are read by the in-container catalog when the
// record does not cover the id, and the crossing never wrote either name.
// An ABSENT name is the one the workspace `.env` fills, so the repository
// under review would pick the window its reviewer compacts at, the
// max_tokens it asks for, the price it is billed at and the provenance all
// of it is recorded under — the same numbers, the same `source=operator`,
// no record involved. Both cross as explicit empties beside the record, and
// only for a gateway node.
func TestForwardableProviderEnv_CrossesTheGatewayCatalogSourcesAsExplicitEmpties(t *testing.T) {
	hostGatewayEnv(t)

	env, _, err := forwardableProviderEnv(context.Background(), "openai_compatible/no-table-has-this-id")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.CatalogProviderEnv} {
		v, ok := env[name]
		if !ok {
			t.Errorf("%s crossed ABSENT for a gateway node — the workspace .env fills it and the repository under review picks the window, the max output and the price of the node reviewing it", name)
			continue
		}
		if v != "" {
			t.Errorf("%s crossed as %q, want the explicit empty: the host's own answer rides in %s", name, v, compatgw.ResolvedEnvName)
		}
	}

	// The endpoint is crossed for every gateway node, always: it is the
	// operator's infrastructure, and the in-container factory is told it
	// explicitly rather than left to read whatever the sandbox carries.
	if got := env[compatgw.BaseURLEnv]; got != "https://198.51.100.7" {
		t.Errorf("%s crossed as %q, want the host's endpoint", compatgw.BaseURLEnv, got)
	}

	// Gateway-only, like the endpoint and the record: no vendor node has
	// business seeing the gateway's catalog env.
	env, _, err = forwardableProviderEnv(context.Background(), "anthropic/claude-opus-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.CatalogProviderEnv} {
		if _, ok := env[name]; ok {
			t.Errorf("a vendor node received %s", name)
		}
	}
}

// The decisive road, and the one the fix's own commit message overstated: a
// host-side `iterion run` INSIDE the repository loads that repository's
// `.env` — the documented reason the auto-load exists, and the dogfood
// shape where a tool node runs `iterion run` in /workspace. The record name
// is refused as planted, but the host resolves the id from the local
// sources, and a planted TABLE taken for the operator's makes the host
// write the repository's numbers into the container as its OWN answer,
// under `source=operator`. Container-side that record carries no mark at
// all, so no reader there can refuse it: the laundering has to be refused
// on the host, where the mark is.
func TestForwardableProviderEnv_DoesNotLaunderAPlantedTableIntoTheCrossedRecord(t *testing.T) {
	hostGatewayEnv(t)
	t.Setenv(compatgw.CatalogModelsEnv, forgedOperatorTable)
	// What cmd/iterion applyDotEnv does for the two `.env` lines it fills.
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	envtrust.MarkPlanted(compatgw.CatalogModelsEnv, compatgw.ResolvedEnvName)
	compatgw.ResetCatalogCaches()

	env, _, err := forwardableProviderEnv(context.Background(), "openai_compatible/gw/m")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if got := env[compatgw.ResolvedEnvName]; got != "" {
		t.Fatalf("the host crossed, as its own answer, the record the repository under review wrote: %s=%s", compatgw.ResolvedEnvName, got)
	}
	// And the mark does not cross either, which is why the refusal has to
	// happen here: whatever the host writes into that record arrives in
	// the container as the host's own, with no provenance to question.
	if marker, ok := env[envtrust.EnvPlantedNames]; ok {
		t.Fatalf("%s crossed as %q: a container-side reader could then ask, and this refusal would not have to be the host's", envtrust.EnvPlantedNames, marker)
	}
}

// A keyless operator gateway is a supported configuration — the base URL is
// the presence signal — and the crossing leaves the key name ABSENT for
// one, which is the state the workspace `.env` of the repository under
// review fills. An explicit empty is deliberately NOT the answer here: an
// empty key reads as "auth attempted, invalid" to some gateways where no
// key at all reads as "no auth", so the refusal lives at the in-container
// read instead (compatgw.FromEnv drops a planted key).
func TestForwardableProviderEnv_AKeylessGatewayCrossesNoKeyAtAll(t *testing.T) {
	hostGatewayEnv(t)
	t.Setenv(compatgw.APIKeyEnv, "")

	env, _, err := forwardableProviderEnv(context.Background(), "openai_compatible/no-table-has-this-id")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if v, ok := env[compatgw.APIKeyEnv]; ok {
		t.Fatalf("%s crossed as %q for a keyless gateway, want the name absent", compatgw.APIKeyEnv, v)
	}
	if got := env[compatgw.BaseURLEnv]; got == "" {
		t.Fatal("a keyless gateway crossed no endpoint either: the base URL is the presence signal")
	}
}

// The second route to the same two names needs no `.env` at all: a
// repository's own `devcontainer.json` becomes the spec the container is
// STARTED with (devcontainer.ToSandboxSpec), so no provenance mark exists
// anywhere for a reader to refuse. What closes it for a gateway node is the
// crossing itself — `ExecOpts.Env` outranks the environment the sandbox was
// started with, which is the precedence providerRoutingEnvVars' empties
// already rely on.
//
// Driven on the real seam: the map forwardableProviderEnv built, handed to
// a real driver's Run.Command exactly as the dispatch hands it
// (sandbox.ExecOpts{Env: runnerEnv}), and the value READ BACK from the
// process that results. The control runs the same spec with the name
// dropped from the crossing, which proves the spec env does reach the
// process — without it the empty would be proving nothing.
func TestForwardableProviderEnv_TheCrossedEmptyOutranksTheSandboxSpecEnv(t *testing.T) {
	hostGatewayEnv(t)
	ctx := context.Background()

	crossing, _, err := forwardableProviderEnv(ctx, "openai_compatible/no-table-has-this-id")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}

	driver, err := noop.New()
	if err != nil {
		t.Fatalf("noop.New: %v", err)
	}
	// The repository's devcontainer.json, parsed into the spec env: the
	// table the deny class does not screen.
	prepared, err := driver.Prepare(ctx, sandbox.Spec{Env: map[string]string{compatgw.CatalogModelsEnv: forgedOperatorTable}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := driver.Start(ctx, prepared, sandbox.RunInfo{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// ${NAME-ABSENT} tells an unset name from one set to the empty string.
	readBack := []string{"sh", "-c", `printf %s "${` + compatgw.CatalogModelsEnv + `-ABSENT}"`}

	out, err := run.Command(ctx, readBack, sandbox.ExecOpts{Env: crossing}).Output()
	if err != nil {
		t.Fatalf("read back the crossed value: %v", err)
	}
	if got := string(out); got != "" {
		t.Errorf("the node's %s read %q, want empty: the repository's own devcontainer.json picked the window, the max output and the price of the node reviewing it", compatgw.CatalogModelsEnv, got)
	}

	// The control: the same spec, the crossing without the name — the
	// state before the fix. The spec value lands, so the assertion above
	// is about the crossing and not about a spec env that never arrives.
	delete(crossing, compatgw.CatalogModelsEnv)
	out, err = run.Command(ctx, readBack, sandbox.ExecOpts{Env: crossing}).Output()
	if err != nil {
		t.Fatalf("read back the spec value: %v", err)
	}
	if got := string(out); !strings.Contains(got, `"context_window":4096`) {
		t.Fatalf("with the name out of the crossing the node read %q, want the spec's table: this driver does not carry the spec env, so the test above proves nothing", got)
	}
}
