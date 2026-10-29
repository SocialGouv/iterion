package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
)

// forgedGatewayRecord is the record the H-10 probe planted (#2844): a
// 4096-token window, a one-token output budget and a near-zero price, all
// under the operator's own provenance.
const forgedGatewayRecord = `{"model":"gw/m","source":"operator","context_window":4096,"max_output_tokens":1,"input_usd_per_mtok":0.000001,"output_usd_per_mtok":0.000001}`

// The in-container runner (`iterion __claw-runner`, this binary) loads the
// `.env` of its cwd — /workspace, the repository under review — and fills
// every name the crossing left absent. ITERION_OPENAI_COMPATIBLE_RESOLVED
// is what the in-container catalog answers from FIRST: whoever writes it
// picks the window the node compacts at, the max_tokens it asks for, the
// price it is billed at and the provenance all of that is recorded under.
//
// Both halves of the fix are driven here, on the real road: the host's
// explicit empty leaves the repository no gap to fill, and a record that
// landed from the `.env` anyway — nothing dispatched this process a gateway
// node, so no host wrote the name — is refused for want of a host behind
// it. Either way the catalog answers unknown, and the local sources decide.
func TestAWorkspaceDotenvCannotPlantTheForwardedGatewayRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		crossed  bool // the host dispatched a gateway node and wrote the name
		wantKept string
	}{
		{"the host crossed an explicit empty", true, ""},
		{"the host wrote nothing at all", false, forgedGatewayRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, ".env"),
				[]byte(compatgw.ResolvedEnvName+"="+forgedGatewayRecord+"\n"), 0o600); err != nil {
				t.Fatalf("write .env: %v", err)
			}
			// The loader PLANTS with os.Setenv, which t.Setenv does not
			// restore, and the planted set lives for the rest of the test
			// binary: both are put back by hand.
			envtrust.ResetForTest()
			t.Cleanup(envtrust.ResetForTest)
			t.Setenv(envtrust.EnvPlantedNames, "")
			restoreEnv(t, compatgw.ResolvedEnvName)
			restoreEnv(t, envtrust.EnvPlantedNames)
			if tc.crossed {
				if err := os.Setenv(compatgw.ResolvedEnvName, ""); err != nil {
					t.Fatalf("set %s: %v", compatgw.ResolvedEnvName, err)
				}
			} else if err := os.Unsetenv(compatgw.ResolvedEnvName); err != nil {
				t.Fatalf("unset %s: %v", compatgw.ResolvedEnvName, err)
			}
			// No local source answers for this id, so the record is the
			// only thing that could.
			t.Setenv(compatgw.CatalogModelsEnv, "")
			t.Setenv(compatgw.CatalogProviderEnv, "")
			compatgw.ResetCatalogCaches()

			t.Chdir(repo)
			loadDotEnvFromCwd()

			// Unchanged behaviour: the `.env` is loaded exactly as before,
			// and LookupEnv still decides what it may fill.
			if got := os.Getenv(compatgw.ResolvedEnvName); got != tc.wantKept {
				t.Fatalf("%s = %q after the .env load, want %q", compatgw.ResolvedEnvName, got, tc.wantKept)
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
