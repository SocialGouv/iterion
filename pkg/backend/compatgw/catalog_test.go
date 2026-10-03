package compatgw

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// mapGet builds an env reader over fixed values — the resolution is pure
// over `get`, so precedence cases need no process env at all.
func mapGet(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

const operatorTableJSON = `{"scaleway/gpt-oss-120b":{"context_window":131072,"max_output_tokens":8192,"input_usd_per_mtok":0.1,"output_usd_per_mtok":0.3}}`

// Whole-entry precedence: the operator table answers first, case
// insensitively; the catalog provider is consulted only when it does not.
func TestResolveCatalog_OperatorTableWins(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{CatalogModelsEnv: operatorTableJSON, CatalogProviderEnv: "scaleway"})
	res := ResolveCatalog("SCALEWAY/GPT-OSS-120B", nil, get)
	if res.Source != SourceOperator || !res.Priced() {
		t.Fatalf("resolved %+v, want the operator record, priced", res)
	}
	if res.Spec.ContextWindow != 131072 {
		t.Errorf("window = %d, want the operator's", res.Spec.ContextWindow)
	}
}

// The operator's declared 0/0 is UNPRICED, not free: the record is known,
// the estimator sees no price.
func TestResolveCatalog_ADeclaredZeroIsUnpriced(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{CatalogModelsEnv: `{"gw/m":{"context_window":8192}}`})
	res := ResolveCatalog("gw/m", nil, get)
	if !res.Known() || res.Priced() {
		t.Fatalf("resolved %+v, want known-but-unpriced", res)
	}
}

// An operator table that does not parse — unknown field, unpaired prices —
// is refused and memoised: a typo'd price must never silently re-price a
// model, and nothing resolves while the refusal stands.
func TestOperatorModels_RefusesAMalformedTable(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":  `{"gw/m":{"context_window":8192,"vendor":"x"}}`,
		"unpaired price": `{"gw/m":{"input_usd_per_mtok":0.1}}`,
		"not json":       `nope`,
	} {
		t.Run(name, func(t *testing.T) {
			ResetCatalogCaches()
			if _, err := operatorModels(mapGet(map[string]string{CatalogModelsEnv: raw})); err == nil {
				t.Fatalf("%s accepted", name)
			} else if !strings.Contains(err.Error(), CatalogModelsEnv) {
				t.Errorf("refusal %q does not name the variable", err.Error())
			}
			// The refusal stands until the process restarts: the same
			// reader keeps refusing even with the table gone — a memo of a
			// decision made on evidence, not of whatever comes next.
			if _, err := operatorModels(mapGet(nil)); err == nil {
				t.Fatal("the memoised refusal evaporated")
			}
		})
	}
	ResetCatalogCaches()
}

// The catalog provider answers exactly — live table when seeded, embedded
// snapshot otherwise — with its provenance attached, and an unknown id
// stays unknown.
func TestResolveCatalog_CatalogProviderAnswers(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{CatalogProviderEnv: "scaleway"})
	live := modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"scaleway/gpt-oss-120b": {ContextWindow: 1000, InputCostPerM: 1, OutputCostPerM: 2},
	})
	if res := ResolveCatalog("gpt-oss-120b", live, get); res.Source != SourceCatalogLive || res.Spec.ContextWindow != 1000 {
		t.Fatalf("resolved %+v, want the live record", res)
	}
	if res := ResolveCatalog("gpt-oss-120b", nil, get); res.Source != SourceCatalogSnapshot || res.Spec.ContextWindow == 0 {
		t.Fatalf("resolved %+v, want the snapshot record", res)
	}
	if res := ResolveCatalog("no-such-model-xyz", live, get); res.Known() {
		t.Fatalf("an unknown id resolved %+v", res)
	}
	// The live branch is EXACT: another provider's entry for the same bare
	// id must never answer under the operator-named provider's name. The id
	// is one the embedded snapshot lacks, so only the live table could
	// answer — and only with the operator-named provider's own key.
	foreign := modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"openai/rva2-unique-bare-id": {ContextWindow: 999999, InputCostPerM: 60, OutputCostPerM: 120},
	})
	if res := ResolveCatalog("rva2-unique-bare-id", foreign, get); res.Known() {
		t.Fatalf("a foreign provider's bare-id entry leaked: %+v", res)
	}
}

// The host's forwarded resolution answers FIRST container-side: the
// image's snapshot can be older than the host's, and the two must never
// disagree across the IPC. A record for another id is ignored.
func TestResolveCatalog_TheForwardedRecordAnswersFirst(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{
		CatalogModelsEnv:   operatorTableJSON,
		CatalogProviderEnv: "scaleway",
		ResolvedEnvName:    `{"model":"scaleway/gpt-oss-120b","source":"catalog-live","context_window":999}`,
	})
	res := ResolveCatalog("scaleway/gpt-oss-120b", nil, get)
	if res.Source != "catalog-live" || res.Spec.ContextWindow != 999 {
		t.Fatalf("resolved %+v, want the forwarded record", res)
	}
	ResetCatalogCaches()
	get = mapGet(map[string]string{
		CatalogModelsEnv: operatorTableJSON,
		ResolvedEnvName:  `{"model":"other/m","source":"operator"}`,
	})
	if res := ResolveCatalog("scaleway/gpt-oss-120b", nil, get); res.Source != SourceOperator {
		t.Fatalf("a foreign forwarded record leaked: %+v", res)
	}
}

// The estimator's gateway branch prices from the catalog and treats
// unknown/unpriced as 0-that-is-not-a-measured-zero.
func TestEstimateUSD_TheGatewayBranchPricesFromTheCatalog(t *testing.T) {
	ResetCatalogCaches()
	t.Setenv(CatalogModelsEnv, `{"m1":{"input_usd_per_mtok":3,"output_usd_per_mtok":15},"unpriced":{"context_window":1024}}`)
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(ResolvedEnvName, "")
	if got := cost.EstimateUSD("openai_compatible/m1", 1_000_000, 1_000_000); got != 18 {
		t.Errorf("EstimateUSD = %v, want 18", got)
	}
	if got := cost.EstimateUSD("openai_compatible/no-such", 1_000_000, 1_000_000); got != 0 {
		t.Errorf("an unknown gateway id priced %v", got)
	}
	if got := cost.EstimateUSD("openai_compatible/unpriced", 1_000_000, 1_000_000); got != 0 {
		t.Errorf("a declared-0/0 id priced %v — declared zero is unpriced, not free", got)
	}
	// A PREFIXED gateway id prices from its full wire id: stripping the
	// first slash again would look up "m2" and miss (the double-strip this
	// seam was rebuilt to kill).
	ResetCatalogCaches()
	t.Setenv(CatalogModelsEnv, `{"sc/m2":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`)
	if got := cost.EstimateUSD("openai_compatible/sc/m2", 1_000_000, 1_000_000); got != 3 {
		t.Errorf("EstimateUSD on a prefixed id = %v, want 3 (the full wire id priced)", got)
	}
}
