package compatgw

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The gateway catalog (ADR-118): what a `openai_compatible/<id>` model
// costs and how big its window is, resolved with WHOLE-ENTRY precedence —
//
//  1. OPENAI_COMPATIBLE_MODELS, the operator's JSON table (id → window,
//     max output, the two prices; prices arrive as a pair or not at all);
//  2. OPENAI_COMPATIBLE_CATALOG_PROVIDER=<q> — an exact `q/<id>` lookup in
//     the live models.dev table when it is loaded, else the embedded
//     snapshot;
//  3. unknown, said out loud.
//
// There is no bare-id, suffix, vendor or static fallback: a gateway id is
// whatever the gateway serves, and matching it against a vendor's table by
// name or prefix would hand an alias another model's window or price.

// CatalogModelsEnv holds the operator's JSON table for the ids its gateway
// serves. `{"scaleway/gpt-oss-120b": {"context_window": 131072, ...}}` —
// unknown fields refused (a typo'd price silently ignored is worse than a
// refusal), prices as a pair or nothing.
const CatalogModelsEnv = "OPENAI_COMPATIBLE_MODELS"

// ResolvedEnvName carries the HOST's own catalog resolution into the
// sandbox (forwardableProviderEnv writes it for gateway nodes only). Its
// record answers FIRST, container-side: the image's snapshot can be older
// than the host's, and a window or price quietly differing across the IPC
// is one side lying to the other. Compact JSON, one record, the model id
// inside.
const ResolvedEnvName = "ITERION_OPENAI_COMPATIBLE_RESOLVED"

// CatalogProviderEnv names the models.dev provider whose EXACT keys answer
// the gateway's ids — the gateway serves BARE ids, and <q> is the upstream
// provider key in models.dev ("scaleway" in the measured deployment).
// Empty means the operator table (or nothing) decides.
const CatalogProviderEnv = "OPENAI_COMPATIBLE_CATALOG_PROVIDER"

// Provenance of a resolved record — recorded on the node output
// (`_gateway_spec`) and in the events, so an operator can tell WHICH table
// answered.
const (
	SourceOperator        = "operator"
	SourceCatalogLive     = "catalog-live"
	SourceCatalogSnapshot = "catalog-snapshot"
	SourceUnknown         = "unknown"
)

// snapshotStaleAfter is the age at which a snapshot-sourced answer earns a
// warning: the models.dev table drifts, and a window or price quietly
// frozen at generation time should say so.
const snapshotStaleAfter = 180 * 24 * time.Hour

// Resolved is one gateway id's catalog record. It is IMMUTABLE
// per-invocation data (ADR-118 §resolution): consumers copy it, they never
// write back to a process-global "current record".
type Resolved struct {
	Spec   modelspecs.Spec
	Source string
}

// Known reports whether ANY table answered — an unknown record carries no
// window and no price, and every consumer says so rather than guessing.
func (r Resolved) Known() bool { return r.Source != SourceUnknown }

// Priced reports whether the record can reach max_cost_usd: the operator's
// declared 0/0 means unpriced, not free.
func (r Resolved) Priced() bool { return r.Spec.InputCostPerM > 0 && r.Spec.OutputCostPerM > 0 }

var (
	operatorTableOnce sync.Once
	operatorTable     map[string]modelspecs.Spec
	operatorTableErr  error
)

// operatorModels parses OPENAI_COMPATIBLE_MODELS once. A malformed table is
// a REFUSAL at every seam that resolves a gateway route, not a silent
// fallback to the catalog: the operator wrote a price, a typo must not
// price the model differently.
func operatorModels(get func(string) string) (map[string]modelspecs.Spec, error) {
	operatorTableOnce.Do(func() {
		raw := strings.TrimSpace(get(CatalogModelsEnv))
		if raw == "" {
			operatorTable = map[string]modelspecs.Spec{}
			return
		}
		var entries map[string]struct {
			ContextWindow    int     `json:"context_window"`
			MaxOutputTokens  int     `json:"max_output_tokens"`
			InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
			OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&entries); err != nil {
			operatorTableErr = fmt.Errorf("openai_compatible: %s does not parse: %w", CatalogModelsEnv, err)
			return
		}
		operatorTable = map[string]modelspecs.Spec{}
		for id, e := range entries {
			if e.InputUSDPerMTok > 0 != (e.OutputUSDPerMTok > 0) {
				operatorTableErr = fmt.Errorf("openai_compatible: %s[%q]: the prices arrive as a pair or not at all", CatalogModelsEnv, id)
				return
			}
			if e.InputUSDPerMTok < 0 || e.OutputUSDPerMTok < 0 {
				operatorTableErr = fmt.Errorf("openai_compatible: %s[%q]: a price is negative", CatalogModelsEnv, id)
				return
			}
			operatorTable[strings.ToLower(strings.TrimSpace(id))] = modelspecs.Spec{
				ContextWindow:   e.ContextWindow,
				MaxOutputTokens: e.MaxOutputTokens,
				InputCostPerM:   e.InputUSDPerMTok,
				OutputCostPerM:  e.OutputUSDPerMTok,
			}
		}
	})
	return operatorTable, operatorTableErr
}

// CheckOperatorTable parses OPENAI_COMPATIBLE_MODELS and returns its
// refusal — the seam-level probe for "the operator table, if any, is
// well-formed". Memoised like the resolution itself: one parse per
// process, the same refusal every time.
func CheckOperatorTable(get func(string) string) error {
	_, err := operatorModels(get)
	return err
}

// ResetCatalogCaches drops the memoised operator table — tests only; a
// production process reads its env once.
func ResetCatalogCaches() {
	operatorTableOnce = sync.Once{}
	operatorTable, operatorTableErr = nil, nil
}

var staleWarnOnce sync.Once

// warnStaleSnapshot says, once per process, that a snapshot-sourced answer
// is older than the drift window. The operator decides what that is worth;
// the record itself is unchanged.
func warnStaleSnapshot() {
	staleWarnOnce.Do(func() {
		age, ok := modelspecs.SnapshotAge()
		if !ok || age <= snapshotStaleAfter {
			return
		}
		iterlog.NewFromEnv(os.Stderr).Warn("openai_compatible: the embedded models.dev snapshot answered and is %d days old — regenerate with `task models:snapshot`", int(age/(24*time.Hour)))
	})
}

// ResolveCatalog resolves one gateway model id. live is the process
// models.dev registry (modelspecs.Default in production); when it has no
// answer the embedded snapshot does, with its provenance attached.
func ResolveCatalog(modelID string, live *modelspecs.Registry, get func(string) string) Resolved {
	// The host's resolution (sandbox forward) answers before anything
	// local: agreement across the IPC beats any local freshness.
	if rec, ok := resolvedEnvRecord(get); ok && rec.Model == strings.ToLower(strings.TrimSpace(modelID)) {
		return Resolved{
			Spec: modelspecs.Spec{
				ContextWindow:   rec.ContextWindow,
				MaxOutputTokens: rec.MaxOutputTokens,
				InputCostPerM:   rec.InputUSDPerMTok,
				OutputCostPerM:  rec.OutputUSDPerMTok,
			},
			Source: rec.Source,
		}
	}
	table, terr := operatorModels(get)
	if terr != nil {
		// The operator table is malformed: refuse loudly, resolve to
		// nothing. The dispatch seams surface the error; the pricer reads
		// unknown.
		warnOperatorTable(terr)
		return Resolved{Source: SourceUnknown}
	}
	if e, ok := table[strings.ToLower(strings.TrimSpace(modelID))]; ok {
		return Resolved{Spec: e, Source: SourceOperator}
	}
	prov := strings.TrimSpace(get(CatalogProviderEnv))
	if prov == "" {
		return Resolved{Source: SourceUnknown}
	}
	// The live table answers only when it is actually loaded — an offline
	// runner's registry falls through to the snapshot instead of blocking.
	if live != nil {
		if e, ok := live.LookupExact(prov, modelID); ok {
			return Resolved{Spec: e, Source: SourceCatalogLive}
		}
	}
	if e, ok := modelspecs.SnapshotLookupExact(prov, modelID); ok {
		warnStaleSnapshot()
		return Resolved{Spec: e, Source: SourceCatalogSnapshot}
	}
	// Said out loud, once per id per process: the record on the node
	// output is easy to miss, and a mis-set OPENAI_COMPATIBLE_CATALOG_PROVIDER
	// would otherwise be invisible in logs.
	if _, loaded := unknownWarns.LoadOrStore(strings.ToLower(strings.TrimSpace(modelID)), true); !loaded {
		iterlog.NewFromEnv(os.Stderr).Warn("openai_compatible: id %q is in no table (set %s, or %s=<models.dev provider> — currently %q)", modelID, CatalogModelsEnv, CatalogProviderEnv, prov)
	}
	return Resolved{Source: SourceUnknown}
}

// unknownWarns bounds the per-id warning to one line each, whatever a
// workflow's templated model strings throw at the catalog.
var unknownWarns sync.Map

// resolvedEnvRecord parses ResolvedEnvName's single record, tolerating a
// wrong-shaped value by ignoring it (the host wrote it; a garbage value
// degrades to the local sources, it never invents one).
func resolvedEnvRecord(get func(string) string) (struct {
	Model            string  `json:"model"`
	Source           string  `json:"source"`
	ContextWindow    int     `json:"context_window,omitempty"`
	MaxOutputTokens  int     `json:"max_output_tokens,omitempty"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
}, bool) {
	var rec struct {
		Model            string  `json:"model"`
		Source           string  `json:"source"`
		ContextWindow    int     `json:"context_window,omitempty"`
		MaxOutputTokens  int     `json:"max_output_tokens,omitempty"`
		InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
		OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
	}
	raw := strings.TrimSpace(get(ResolvedEnvName))
	if raw == "" || json.Unmarshal([]byte(raw), &rec) != nil || rec.Model == "" {
		return rec, false
	}
	// Provenance is one of the four constants — a stale or forged value
	// with a fabricated source must not pass as a record.
	switch rec.Source {
	case SourceOperator, SourceCatalogLive, SourceCatalogSnapshot:
	default:
		return rec, false
	}
	rec.Model = strings.ToLower(strings.TrimSpace(rec.Model))
	return rec, true
}

var operatorTableWarnOnce sync.Once

func warnOperatorTable(err error) {
	operatorTableWarnOnce.Do(func() {
		iterlog.NewFromEnv(os.Stderr).Warn("%v", err)
	})
}

func init() {
	// The estimator's gateway branch reads the same static sources this
	// package owns (operator table + snapshot — never the live registry,
	// whose background refresh would make a price depend on fetch timing).
	cost.SetGatewayPricer(StaticPricer)
}

// StaticPricer prices one gateway WIRE id (the caller in cost.go has
// already stripped the openai_compatible/ prefix — stripping here too would
// cut a prefixed gateway id ("scaleway/<id>") at its own slash). Unknown
// (or an unpriced operator entry) is ok=false — cost 0 is never a measured
// zero.
func StaticPricer(wireModelID string) (inputUSDPerMTok, outputUSDPerMTok float64, known bool) {
	// Live-aware, like every other catalog consumer: the stamp and the
	// estimator must read the SAME sources, or one claims what the other
	// declined (round-2's HIGH). The registry's refresh is background-only;
	// this never blocks on a fetch.
	res := ResolveCatalog(wireModelID, modelspecs.Default(), os.Getenv)
	if !res.Priced() {
		return 0, 0, false
	}
	return res.Spec.InputCostPerM, res.Spec.OutputCostPerM, true
}
