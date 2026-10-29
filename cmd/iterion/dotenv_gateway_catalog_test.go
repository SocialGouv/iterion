package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
)

// forgedGatewayTable and forgedGatewayQualifier are the record's two
// SIBLINGS — the local sources the in-container catalog answers from when
// the forwarded record does not cover the id. The numbers are the H-10
// probe's (#2844): a 4096-token window, a one-token output budget and a
// near-zero price, resolved as `source=operator`.
const (
	forgedGatewayTable     = `{"gw/m":{"context_window":4096,"max_output_tokens":1,"input_usd_per_mtok":0.000001,"output_usd_per_mtok":0.000001}}`
	forgedGatewayQualifier = "scaleway"
)

// Both halves of the fix on the real road, for the whole class rather than
// the one name: the runner (`iterion __claw-runner`, this binary) loads the
// `.env` of its cwd — /workspace, the repository under review — and fills
// every name the crossing left absent. Whichever of the three catalog names
// it fills picks the window the node compacts at, the max_tokens it asks
// for, the price it is billed at and the provenance all of that is recorded
// under.
//
// The host's explicit empties leave the repository no gap to fill; and a
// value that landed from the `.env` anyway — nothing dispatched this process
// a gateway node, so no host wrote any of the names — is refused for want
// of a host behind it. Either way the catalog answers unknown.
func TestAWorkspaceDotenvCannotPlantTheGatewayCatalogSources(t *testing.T) {
	for _, tc := range []struct {
		name    string
		crossed bool // the host dispatched a gateway node and wrote the names
	}{
		{"the host crossed explicit empties", true},
		{"the host wrote nothing at all", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, ".env"), []byte(
				compatgw.CatalogModelsEnv+"="+forgedGatewayTable+"\n"+
					compatgw.CatalogProviderEnv+"="+forgedGatewayQualifier+"\n"), 0o600); err != nil {
				t.Fatalf("write .env: %v", err)
			}
			// The loader PLANTS with os.Setenv, which t.Setenv does not
			// restore, and the planted set lives for the rest of the test
			// binary: both are put back by hand.
			envtrust.ResetForTest()
			t.Cleanup(envtrust.ResetForTest)
			t.Setenv(envtrust.EnvPlantedNames, "")
			for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.CatalogProviderEnv, compatgw.ResolvedEnvName, envtrust.EnvPlantedNames} {
				restoreEnv(t, name)
			}
			// The host resolved nothing for this id, so its record crosses
			// empty whatever happens to the siblings.
			if err := os.Setenv(compatgw.ResolvedEnvName, ""); err != nil {
				t.Fatalf("set %s: %v", compatgw.ResolvedEnvName, err)
			}
			for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.CatalogProviderEnv} {
				var err error
				if tc.crossed {
					err = os.Setenv(name, "")
				} else {
					err = os.Unsetenv(name)
				}
				if err != nil {
					t.Fatalf("prepare %s: %v", name, err)
				}
			}
			compatgw.ResetCatalogCaches()
			t.Cleanup(compatgw.ResetCatalogCaches)

			t.Chdir(repo)
			loadDotEnvFromCwd()

			// Unchanged behaviour: the `.env` is loaded exactly as before,
			// and LookupEnv still decides what it may fill.
			for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.CatalogProviderEnv} {
				got, planted := os.Getenv(name), envtrust.Planted(name)
				if tc.crossed && (got != "" || planted) {
					t.Fatalf("%s = %q (planted=%v) after the .env load, want the crossed empty kept", name, got, planted)
				}
				if !tc.crossed && !planted {
					t.Fatalf("%s = %q was not recorded as planted: the .env filled an absent name and nothing marked it", name, got)
				}
			}

			res := compatgw.ResolveCatalog("gw/m", nil, os.Getenv)
			if res.Source != compatgw.SourceUnknown {
				t.Fatalf("the container resolved source %q, want %q", res.Source, compatgw.SourceUnknown)
			}
			// The EFFECT, not the label: none of the forged numbers reached
			// the node — not the window it compacts at, not the budget it
			// asks for, not the price it is billed at.
			if res.Spec.ContextWindow != 0 || res.Spec.MaxOutputTokens != 0 || res.Priced() {
				t.Fatalf("the container resolved %+v, want nothing: the repository under review chose the window, the max output and the price of the node reviewing it", res)
			}
		})
	}
}

// The adjacent name in the same crossing: the `.env` of the repository under
// review fills a gateway key the host has none for — and that value is the
// credential the in-container registry's gateway factory presents on every
// call (registry.go, compatgw.FromEnv(os.Getenv)), so what the repository
// would be choosing is which virtual key, hence which budget and tenant, its
// reviewer's calls are billed to.
//
// Refused, not dropped: a Config with no key builds no client (claw's OpenAI
// provider refuses one), so the gateway is named in the refusal instead of
// dying at dispatch on a message about OPENAI_API_KEY and Codex's auth.json.
func TestAWorkspaceDotenvCannotPlantTheGatewayApiKey(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte(compatgw.APIKeyEnv+"=sk-chosen-by-the-repo-under-review\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	restoreEnv(t, compatgw.APIKeyEnv)
	restoreEnv(t, envtrust.EnvPlantedNames)
	// The host crossed the endpoint (always) and no key (it has none).
	t.Setenv(compatgw.BaseURLEnv, "https://198.51.100.7")
	if err := os.Unsetenv(compatgw.APIKeyEnv); err != nil {
		t.Fatalf("unset %s: %v", compatgw.APIKeyEnv, err)
	}

	t.Chdir(repo)
	loadDotEnvFromCwd()

	// The value is set, as a `.env` always sets what it fills — only its
	// authority is in question.
	if got := os.Getenv(compatgw.APIKeyEnv); got == "" {
		t.Fatal("the .env did not set the key at all: the rest of this case proves nothing")
	}
	cfg, err := compatgw.FromEnv(os.Getenv)
	if err == nil {
		t.Fatalf("the repository under review chose the credential its reviewer's gateway calls present: base=%q key=%q", cfg.BaseURL, cfg.APIKey)
	}
	for _, want := range []string{compatgw.APIKeyEnv, "project `.env`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "sk-chosen-by-the-repo-under-review") {
		t.Error("the refusal printed the credential")
	}
}
