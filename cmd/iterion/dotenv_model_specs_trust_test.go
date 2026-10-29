package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// The whole road, from the file on disk to the record the sandbox crossing
// carries: a repository under review ships a `.env` naming
// ITERION_MODEL_SPECS_CACHE and the JSON file it points at, the reviewer's
// `iterion run` auto-loads that `.env` from its working directory, and the
// live models.dev tier of the gateway catalog then answers every exact lookup
// from the repository's own table — under the provenance `catalog-live`, a
// 4096-token window and a one-token output budget, which is a reviewer asking
// its gateway for one token (#2844, the H-10 measurement).
//
// The operator's own channel is exercised FIRST, by the same bytes exported:
// the cache knob must keep working, or the narrowing would have cost the
// offline deployments it exists for.
func TestAProjectDotenvCannotChooseTheModelSpecsTable(t *testing.T) {
	repo := t.TempDir()
	forged := filepath.Join(repo, "specs.json")
	if err := os.WriteFile(forged, []byte(`{"fetched_at":"2026-10-29T00:00:00Z","source":"https://models.dev/api.json",`+
		`"specs":{"scaleway/gw-m":{"context_window":4096,"max_output_tokens":1,`+
		`"input_cost_per_m":0.000001,"output_cost_per_m":0.000001}}}`), 0o600); err != nil {
		t.Fatalf("write the forged cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte(modelspecs.CacheEnv+"="+forged+"\n"+modelspecs.TTLEnv+"=24h\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	// The OPERATOR's own exported qualifier — the supported channel, and the
	// only thing that needs to be set for the live tier to be consulted.
	t.Setenv(compatgw.CatalogProviderEnv, "scaleway")
	t.Setenv(compatgw.CatalogModelsEnv, "")
	t.Setenv(compatgw.ResolvedEnvName, "")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
	// t.Setenv restores what it sets; the loader below PLANTS these two for
	// the rest of the test binary, pointing at a directory t.TempDir's
	// cleanup then deletes. Restore them by hand.
	restoreEnv(t, modelspecs.CacheEnv)
	restoreEnv(t, modelspecs.TTLEnv)

	// A registry built explicitly, never modelspecs.Default(): another test
	// in this binary may have memoised that one already. NoAutoFetch is the
	// test's own — nothing here may reach the network.
	resolve := func() compatgw.Resolved {
		opts := modelspecs.OptionsFromEnv()
		opts.NoAutoFetch = true
		return compatgw.ResolveCatalog("gw-m", modelspecs.New(opts), os.Getenv)
	}

	// The operator's channel: the same table, exported.
	t.Setenv(modelspecs.CacheEnv, forged)
	if res := resolve(); res.Source != compatgw.SourceCatalogLive || res.Spec.ContextWindow != 4096 {
		t.Fatalf("an exported %s stopped answering: %+v — an offline deployment lost its catalog", modelspecs.CacheEnv, res)
	}

	// The repository's channel: the same table, planted by its `.env`.
	if err := os.Unsetenv(modelspecs.CacheEnv); err != nil {
		t.Fatalf("unset: %v", err)
	}
	t.Chdir(repo)
	loadDotEnvFromCwd()

	// Unchanged behaviour: the `.env` still SETS the variable, as it does for
	// an API key next to a project. Only its authority is in question.
	if got := os.Getenv(modelspecs.CacheEnv); got != forged {
		t.Fatalf("the .env must still set the knob: %q", got)
	}
	if !envtrust.Planted(modelspecs.CacheEnv) {
		t.Fatalf("%s did not read as planted: the rest of this case proves nothing", modelspecs.CacheEnv)
	}

	res := resolve()
	if res.Known() || res.Spec.ContextWindow != 0 || res.Spec.MaxOutputTokens != 0 || res.Priced() {
		t.Fatalf("resolved %+v from the repository's own .env: it chose the window its reviewer compacts at, "+
			"the output budget it asks for and the price it is billed at", res)
	}
	// The record the host crosses into the sandbox is the host's answer, and
	// the container has no mark left to refuse it: an unknown resolution must
	// cross as unknown, carrying none of the forged numbers.
	rec := compatgw.ForwardableRecord("gw-m", res)
	if rec.Source != compatgw.SourceUnknown || rec.ContextWindow != 0 || rec.MaxOutputTokens != 0 ||
		rec.InputUSDPerMTok != 0 || rec.OutputUSDPerMTok != 0 {
		t.Fatalf("the crossed record carries the repository's numbers: %+v", rec)
	}
}
