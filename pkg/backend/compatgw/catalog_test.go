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

// A gateway that names its models provider-first (LiteLLM's
// `scaleway/glm-5.2`) resolves under OPENAI_COMPATIBLE_CATALOG_PROVIDER=scaleway
// exactly like the bare `glm-5.2` does: the `q/<id>` key misses, the id as-is
// is the key. Only the CONFIGURED qualifier is recognised — another provider's
// prefix stays part of the id — and a table that really holds the doubled key
// still wins, `q/<id>` being tried first.
func TestResolveCatalog_AnIdAlreadyCarryingTheQualifierResolves(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{CatalogProviderEnv: "scaleway"})
	live := modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"scaleway/glm-5.2": {ContextWindow: 1234, InputCostPerM: 1, OutputCostPerM: 2},
	})
	if res := ResolveCatalog("scaleway/glm-5.2", live, get); res.Source != SourceCatalogLive || res.Spec.ContextWindow != 1234 {
		t.Fatalf("provider-first id resolved %+v, want the live scaleway/glm-5.2 record", res)
	}
	if res := ResolveCatalog("Scaleway/GLM-5.2", live, get); res.Source != SourceCatalogLive || res.Spec.ContextWindow != 1234 {
		t.Fatalf("case-folded provider-first id resolved %+v, want the live record", res)
	}
	if res := ResolveCatalog("scaleway/gpt-oss-120b", nil, get); res.Source != SourceCatalogSnapshot || res.Spec.ContextWindow == 0 {
		t.Fatalf("provider-first id resolved %+v, want the snapshot record", res)
	}
	// Another provider's prefix is not the qualifier: it stays in the id and
	// the lookup misses (the bare-id test above proves the snapshot holds
	// gpt-oss-120b under scaleway, so only the prefix keeps it unknown).
	if res := ResolveCatalog("openai/gpt-oss-120b", live, get); res.Known() {
		t.Fatalf("a foreign provider's prefix was stripped: %+v", res)
	}
	// One occurrence is stripped, never every one.
	if res := ResolveCatalog("scaleway/scaleway/glm-5.2", live, get); res.Known() {
		t.Fatalf("a doubled prefix was stripped twice: %+v", res)
	}
	// The qualifier and the id are read like every lookup here reads them:
	// lower-cased, trimmed at the ends.
	loud := mapGet(map[string]string{CatalogProviderEnv: " Scaleway "})
	if res := ResolveCatalog("  scaleway/glm-5.2 ", live, loud); res.Source != SourceCatalogLive || res.Spec.ContextWindow != 1234 {
		t.Fatalf("a loud qualifier or a padded id did not resolve: %+v", res)
	}
	// Exact stays exact: a remainder the lookups would have to trim is no
	// id (the lookups trim their arguments, which must not turn
	// `scaleway/ glm-5.2` into glm-5.2's record).
	for _, id := range []string{"scaleway/ glm-5.2", "scaleway/\tglm-5.2", "scaleway/glm-5.2 \n"} {
		if res := ResolveCatalog(id, live, get); id != "scaleway/glm-5.2 \n" && res.Known() {
			t.Fatalf("inner whitespace resolved %q: %+v", id, res)
		}
	}
	if res := ResolveCatalog("scaleway/glm-5.2 \n", live, get); !res.Known() {
		t.Fatalf("trailing whitespace is trimmed at the ends like everywhere: %+v", res)
	}
	// `q/<id>` is tried FIRST: a table that holds the doubled key answers
	// with it, not with the stripped one.
	doubled := modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"scaleway/scaleway/glm-5.2": {ContextWindow: 42, InputCostPerM: 1, OutputCostPerM: 2},
		"scaleway/glm-5.2":          {ContextWindow: 1234, InputCostPerM: 1, OutputCostPerM: 2},
	})
	if res := ResolveCatalog("scaleway/glm-5.2", doubled, get); res.Source != SourceCatalogLive || res.Spec.ContextWindow != 42 {
		t.Fatalf("the doubled key did not win: %+v", res)
	}
	// Across tables the live table keeps its precedence (ADR-122 §1.2): a
	// live table holding only the stripped key answers before a snapshot
	// holding `q/<id>`. nvidia self-namespaces its ids in models.dev, so the
	// snapshot holds `nvidia/nvidia/<id>` and not `nvidia/<id>`.
	nv := mapGet(map[string]string{CatalogProviderEnv: "nvidia"})
	if res := ResolveCatalog("nvidia/nemotron-3-super-120b-a12b", nil, nv); res.Source != SourceCatalogSnapshot {
		t.Fatalf("the snapshot's self-namespaced nvidia key did not answer: %+v", res)
	}
	stripped := modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"nvidia/nemotron-3-super-120b-a12b": {ContextWindow: 7, InputCostPerM: 1, OutputCostPerM: 2},
	})
	if res := ResolveCatalog("nvidia/nemotron-3-super-120b-a12b", stripped, nv); res.Source != SourceCatalogLive || res.Spec.ContextWindow != 7 {
		t.Fatalf("the live table's stripped key must answer before the snapshot's q/<id>: %+v", res)
	}
}

// TestOperatorModels_DeclaresTheFlagsItKnows: the operator's table may state
// reasoning, tool_call and temperature; a flag it leaves out is nil — unknown
// downstream, never false — and an unknown key is still refused.
func TestOperatorModels_DeclaresTheFlagsItKnows(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	get := func(k string) string {
		if k == CatalogModelsEnv {
			return `{"glm-5.2": {"context_window": 200000, "reasoning": true, "temperature": false}}`
		}
		return ""
	}
	r := ResolveCatalog("glm-5.2", nil, get)
	if r.Source != SourceOperator || r.Spec.ContextWindow != 200000 {
		t.Fatalf("resolved = %+v, want the operator's record", r)
	}
	if r.Spec.Reasoning == nil || !*r.Spec.Reasoning || r.Spec.Temperature == nil || *r.Spec.Temperature {
		t.Fatalf("flags = %v/%v, want reasoning true and temperature false as declared", r.Spec.Reasoning, r.Spec.Temperature)
	}
	if r.Spec.ToolCall != nil {
		t.Fatalf("tool_call = %v, want nil: the operator left it out", *r.Spec.ToolCall)
	}
	ResetCatalogCaches()
	bad := func(k string) string {
		if k == CatalogModelsEnv {
			return `{"glm-5.2": {"reasonning": true}}`
		}
		return ""
	}
	if err := CheckOperatorTable(bad); err == nil {
		t.Fatal("a misspelt flag key was accepted")
	}
}
