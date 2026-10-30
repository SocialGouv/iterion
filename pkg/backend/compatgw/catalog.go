package compatgw

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
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
//     snapshot (an id already carrying `q/` as its first segment is also
//     tried as-is: see qualifiedIDs);
//  3. unknown, said out loud.
//
// There is no bare-id, suffix, vendor or static fallback: a gateway id is
// whatever the gateway serves, and matching it against a vendor's table by
// name or prefix would hand an alias another model's window or price.
//
// All three env names are read through catalogEnv, the one place the
// provenance question is asked for the class.

// CatalogModelsEnv holds the operator's JSON table for the ids its gateway
// serves. `{"scaleway/gpt-oss-120b": {"context_window": 131072, ...}}` —
// unknown fields refused (a typo'd price silently ignored is worse than a
// refusal), prices as a pair or nothing, and the capability flags
// (`reasoning`, `tool_call`, `temperature`) each optional: a flag the
// operator leaves out stays UNKNOWN downstream, never false (#2103).
const CatalogModelsEnv = "OPENAI_COMPATIBLE_MODELS"

// ResolvedEnvName carries the HOST's own catalog resolution into the
// sandbox (forwardableProviderEnv writes it for gateway nodes only). Its
// record answers FIRST, container-side: the image's snapshot can be older
// than the host's, and a window or price quietly differing across the IPC
// is one side lying to the other. Compact JSON, one record, the model id
// inside.
//
// The host writes the name for every gateway node it dispatches, ALWAYS —
// an empty value when its own answer is unknown — because an absent name
// is the one the workspace .env fills inside the container. The two halves
// of that are one contract: the explicit empty leaves no gap for the
// repository under review to fill, and catalogEnv refuses a value this
// process's own .env planted, which is a record no host wrote. The same
// pair holds for the two local sources this record answers before.
const ResolvedEnvName = "ITERION_OPENAI_COMPATIBLE_RESOLVED"

// CatalogProviderEnv names the models.dev provider whose EXACT keys answer
// the gateway's ids — <q> is the upstream provider key in models.dev
// ("scaleway" in the measured deployment). The gateway serves bare ids
// (`glm-5.2`, key `q/glm-5.2`) or provider-first ids (`scaleway/glm-5.2`,
// LiteLLM's naming): an id already carrying <q> as its first segment is
// also looked up as-is — within each table `q/<id>` first, and the live
// table (both keys) before the snapshot. Empty means the operator table (or
// nothing) decides.
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

// operatorTableEntry is one parsed operator table, memoised by the RAW
// value it was parsed from.
type operatorTableEntry struct {
	table map[string]modelspecs.Spec
	err   error
}

var (
	operatorTableMu sync.Mutex
	// Keyed by the raw value READ, not by first caller: a process holds
	// readers of different provenance (the dispatch head reads live, the
	// sandbox crossing reads envtrust.Inherited — #2813), and a sync.Once
	// would answer every reader with the table the FIRST getter produced —
	// a workspace .env's table, primed live, crossing as the operator's.
	// catalogEnv filters the planted values out before they reach this map,
	// so nothing a repository planted is memoised under the operator's name.
	operatorTables = map[string]operatorTableEntry{}
	// A parse REFUSAL is a decision made on evidence: it stands for the
	// process whatever later reads produce (a table that disappears must
	// not rehabilitate it). Over-refusal is the safe direction.
	operatorRefusal error
)

// catalogEnv reads one of the three names this catalog resolves from, and
// answers "" — "nothing said about this" — for a value a project `.env`
// planted. It is the ONE place the question is asked, because the three
// names are one class: each decides the window a gateway node compacts at,
// the max_tokens it asks for, the price it is billed at and the provenance
// all of that is recorded under, and each is read on both sides of the
// sandbox boundary. A `.env` sits in a repository — including the one under
// review, mounted at /workspace, whose `.env` the in-container runner
// auto-loads over every name the crossing left absent — so a planted value
// is one no operator and no host stands behind: in the container it is the
// resolution itself, and on the host it would be LAUNDERED into the record
// the host crosses as its own answer, where the container has no mark left
// to refuse. The question is about THIS process's environment, which is
// the only one a value can arrive in and what every production caller
// hands as `get`.
//
// Silent, like a wrong-shaped value: this read serves the capability READ
// paths too (ResolveCatalogQuiet, one call per keystroke of a free-text
// model field), where a line per call is the noise #2103 closed. The
// unknown resolution it falls through to is what says it out loud, once, on
// the launch path.
func catalogEnv(get func(string) string, name string) string {
	if envtrust.Planted(name) {
		return ""
	}
	return strings.TrimSpace(get(name))
}

// operatorModels parses OPENAI_COMPATIBLE_MODELS, memoised by the value
// read. A malformed table is a REFUSAL at every seam that resolves a
// gateway route, not a silent fallback to the catalog: the operator wrote
// a price, a typo must not price the model differently. A table the
// repository planted is not the operator's, so it neither prices nor
// refuses anything (catalogEnv) — and the launch seam says so, refusal
// included (warnPlantedOperatorTable).
func operatorModels(get func(string) string) (map[string]modelspecs.Spec, error) {
	raw := catalogEnv(get, CatalogModelsEnv)
	operatorTableMu.Lock()
	defer operatorTableMu.Unlock()
	if operatorRefusal != nil {
		return nil, operatorRefusal
	}
	if e, ok := operatorTables[raw]; ok {
		return e.table, e.err
	}
	table, err := parseOperatorTable(raw)
	if err != nil {
		operatorRefusal = err
	}
	operatorTables[raw] = operatorTableEntry{table: table, err: err}
	return table, err
}

// parseOperatorTable parses the raw OPENAI_COMPATIBLE_MODELS value. Empty
// is the empty table; anything malformed is a refusal. It is the ONE
// parser of that format: the resolution reads the operator's table
// through it, and the planted-table diagnostic re-reads a value the
// resolution declined through the SAME code, so what an operator is told
// about their typo is what would have refused their routes — not a second
// decoder that agrees today.
func parseOperatorTable(raw string) (map[string]modelspecs.Spec, error) {
	if raw == "" {
		return map[string]modelspecs.Spec{}, nil
	}
	var entries map[string]struct {
		ContextWindow    int     `json:"context_window"`
		MaxOutputTokens  int     `json:"max_output_tokens"`
		InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
		OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
		Reasoning        *bool   `json:"reasoning"`
		ToolCall         *bool   `json:"tool_call"`
		Temperature      *bool   `json:"temperature"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("openai_compatible: %s does not parse: %w", CatalogModelsEnv, err)
	}
	table := map[string]modelspecs.Spec{}
	for id, e := range entries {
		if e.InputUSDPerMTok > 0 != (e.OutputUSDPerMTok > 0) {
			return nil, fmt.Errorf("openai_compatible: %s[%q]: the prices arrive as a pair or not at all", CatalogModelsEnv, id)
		}
		if e.InputUSDPerMTok < 0 || e.OutputUSDPerMTok < 0 {
			return nil, fmt.Errorf("openai_compatible: %s[%q]: a price is negative", CatalogModelsEnv, id)
		}
		table[strings.ToLower(strings.TrimSpace(id))] = modelspecs.Spec{
			ContextWindow:   e.ContextWindow,
			MaxOutputTokens: e.MaxOutputTokens,
			InputCostPerM:   e.InputUSDPerMTok,
			OutputCostPerM:  e.OutputUSDPerMTok,
			Reasoning:       e.Reasoning,
			ToolCall:        e.ToolCall,
			Temperature:     e.Temperature,
		}
	}
	return table, nil
}

// CheckOperatorTable parses OPENAI_COMPATIBLE_MODELS and returns its
// refusal — the seam-level probe for "the operator table, if any, is
// well-formed". Memoised like the resolution itself: one parse per
// value, the same refusal every time.
//
// It is also where a table the catalog DECLINED is said out loud, because
// this is the launch-and-validation seam (pkg/backend/model checkGatewayEnv,
// run before a gateway element is dispatched anywhere) rather than a
// per-keystroke read.
func CheckOperatorTable(get func(string) string) error {
	warnPlantedOperatorTable(get)
	_, err := operatorModels(get)
	return err
}

var plantedTableWarnOnce sync.Once

// plantedTableRemedy names the channel this catalog reads, on every
// planted-table line: a diagnostic that only says "ignored" leaves the
// operator with no next step.
const plantedTableRemedy = "export it in the environment the deployment runs with — the channel this catalog reads, and the one a gateway node's crossing carries"

// warnPlantedOperatorTable says, once per process, that
// OPENAI_COMPATIBLE_MODELS holds a table a project `.env` filled in, which
// prices nothing and refuses nothing (catalogEnv). A MALFORMED one is said
// with its refusal: a malformed table is a REFUSAL at every seam that
// resolves a gateway route — the operator wrote a price and a typo must not
// price the model differently — and the narrowing turned that refusal into
// silence for a value planted this way. An operator reading an unpriced run
// and no diagnostic has nothing to look for; both halves of the fact are
// theirs (the typo, and the file it is in not being a channel).
//
// Said on this seam only: the capability READ paths resolve through
// ResolveCatalogQuiet, one call per keystroke of a free-text model field,
// where a line per call is the noise #2103 closed.
func warnPlantedOperatorTable(get func(string) string) {
	if !envtrust.Planted(CatalogModelsEnv) {
		return
	}
	raw := strings.TrimSpace(get(CatalogModelsEnv))
	if raw == "" {
		return
	}
	plantedTableWarnOnce.Do(func() {
		log := iterlog.NewFromEnv(os.Stderr)
		if _, err := parseOperatorTable(raw); err != nil {
			log.Warn("%v — and it comes from a project `.env`, so it refuses nothing and the routes resolve to unknown instead: %s", err, plantedTableRemedy)
			return
		}
		log.Warn("openai_compatible: %s comes from a project `.env`, so it prices nothing and refuses nothing: %s", CatalogModelsEnv, plantedTableRemedy)
	})
}

// ResetCatalogCaches drops the memoised operator table and the per-id
// unknown-warning memo — tests only; a production process reads its env
// once and says each id once.
func ResetCatalogCaches() {
	operatorTableMu.Lock()
	operatorTables = map[string]operatorTableEntry{}
	operatorRefusal = nil
	operatorTableMu.Unlock()
	plantedTableWarnOnce = sync.Once{}
	unknownWarns.Range(func(k, _ any) bool {
		unknownWarns.Delete(k)
		return true
	})
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
// answer the embedded snapshot does, with its provenance attached. An id no
// table carries earns one WARN per id per process — the dispatch seams and
// the launch path resolve this way, where an unknown id is about to cost a
// run.
func ResolveCatalog(modelID string, live *modelspecs.Registry, get func(string) string) Resolved {
	return resolveCatalog(modelID, live, get, true)
}

// ResolveCatalogQuiet resolves the same way and says nothing about an id no
// table carries: on the capability READ paths an unknown id is the normal
// case — the studio's free-text model fields query the capability route with
// whatever is typed so far, one request per keystroke, and the answer those
// readers render (`?` cells, `source=unknown`) already states the fact. The
// per-id memo is left untouched, so a later launch still warns once.
func ResolveCatalogQuiet(modelID string, live *modelspecs.Registry, get func(string) string) Resolved {
	return resolveCatalog(modelID, live, get, false)
}

func resolveCatalog(modelID string, live *modelspecs.Registry, get func(string) string, warnUnknown bool) Resolved {
	// The host's resolution (sandbox forward) answers before anything
	// local: agreement across the IPC beats any local freshness.
	if rec, ok := resolvedEnvRecord(get); ok && rec.Model == strings.ToLower(strings.TrimSpace(modelID)) {
		return Resolved{
			Spec: modelspecs.Spec{
				ContextWindow:   rec.ContextWindow,
				MaxOutputTokens: rec.MaxOutputTokens,
				InputCostPerM:   rec.InputUSDPerMTok,
				OutputCostPerM:  rec.OutputUSDPerMTok,
				Reasoning:       rec.Reasoning,
				ToolCall:        rec.ToolCall,
				Temperature:     rec.Temperature,
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
	prov := catalogEnv(get, CatalogProviderEnv)
	if prov == "" {
		return Resolved{Source: SourceUnknown}
	}
	ids := qualifiedIDs(prov, modelID)
	// The live table answers only when it is actually loaded — an offline
	// runner's registry falls through to the snapshot instead of blocking.
	if live != nil {
		for _, id := range ids {
			if e, ok := live.LookupExact(prov, id); ok {
				return Resolved{Spec: e, Source: SourceCatalogLive}
			}
		}
	}
	for _, id := range ids {
		if e, ok := modelspecs.SnapshotLookupExact(prov, id); ok {
			warnStaleSnapshot()
			return Resolved{Spec: e, Source: SourceCatalogSnapshot}
		}
	}
	// Said out loud, once per id per process: the record on the node
	// output is easy to miss, and a mis-set OPENAI_COMPATIBLE_CATALOG_PROVIDER
	// would otherwise be invisible in logs. A quiet caller keeps it at debug
	// and does not consume the memo.
	if !warnUnknown {
		iterlog.NewFromEnv(os.Stderr).Debug("openai_compatible: id %q is in no table (read path, not a launch)", modelID)
	} else if _, loaded := unknownWarns.LoadOrStore(strings.ToLower(strings.TrimSpace(modelID)), true); !loaded {
		iterlog.NewFromEnv(os.Stderr).Warn("openai_compatible: id %q is in no table (set %s, or %s=<models.dev provider> — currently %q)", modelID, CatalogModelsEnv, CatalogProviderEnv, prov)
	}
	return Resolved{Source: SourceUnknown}
}

// qualifiedIDs lists the ids the qualifier q is tried against for one
// gateway id, in order: the id itself (key `q/<id>`) and, when the id
// already carries q as its first segment, the id without it (so the key is
// the id as-is). A gateway naming its models provider-first — LiteLLM's
// `scaleway/glm-5.2` — would otherwise be looked up as
// `scaleway/scaleway/glm-5.2` and resolve to nothing. Only the configured
// qualifier is recognised: another provider's prefix is part of the id, one
// occurrence is stripped, and a remainder the lookups would have to trim
// (`scaleway/ glm-5.2`) is not an id — exact stays exact.
func qualifiedIDs(q, modelID string) []string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	ids := []string{id}
	if rest, ok := strings.CutPrefix(id, strings.ToLower(strings.TrimSpace(q))+"/"); ok && rest != "" && rest == strings.TrimSpace(rest) {
		ids = append(ids, rest)
	}
	return ids
}

// unknownWarns bounds the per-id warning to one line each, whatever a
// workflow's templated model strings throw at the catalog.
var unknownWarns sync.Map

// ResolvedEnvRecord is the wire shape of ResolvedEnvName — the ONE struct
// the host writes and the container reads, so a field added to the record
// crosses the IPC by construction instead of by two mirrored literals
// agreeing. The capability flags are `*bool,omitempty`: a flag no table
// stated is absent from the record and reads unknown, a stated false
// crosses as false.
type ResolvedEnvRecord struct {
	Model            string  `json:"model"`
	Source           string  `json:"source"`
	ContextWindow    int     `json:"context_window,omitempty"`
	MaxOutputTokens  int     `json:"max_output_tokens,omitempty"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
	Reasoning        *bool   `json:"reasoning,omitempty"`
	ToolCall         *bool   `json:"tool_call,omitempty"`
	Temperature      *bool   `json:"temperature,omitempty"`
}

// ForwardableRecord is the record the host forwards for one resolved
// gateway id: the resolution as the host answered it, flags included.
func ForwardableRecord(modelID string, r Resolved) ResolvedEnvRecord {
	return ResolvedEnvRecord{
		Model:            modelID,
		Source:           r.Source,
		ContextWindow:    r.Spec.ContextWindow,
		MaxOutputTokens:  r.Spec.MaxOutputTokens,
		InputUSDPerMTok:  r.Spec.InputCostPerM,
		OutputUSDPerMTok: r.Spec.OutputCostPerM,
		Reasoning:        r.Spec.Reasoning,
		ToolCall:         r.Spec.ToolCall,
		Temperature:      r.Spec.Temperature,
	}
}

// resolvedEnvRecord parses ResolvedEnvName's single record, tolerating a
// wrong-shaped value by ignoring it (the host wrote it; a garbage value
// degrades to the local sources, it never invents one).
func resolvedEnvRecord(get func(string) string) (ResolvedEnvRecord, bool) {
	var rec ResolvedEnvRecord
	// Through catalogEnv: the host crosses this name explicitly for every
	// gateway node it dispatches, empty answer included, so a PLANTED
	// record is one no host wrote — it reads as no record at all, and the
	// local sources answer as they do for any id the record does not cover.
	raw := catalogEnv(get, ResolvedEnvName)
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
	// Both forms are wired in ONE call: the quiet one is the read paths'
	// only road to a gateway price, and a nil one there would silently
	// price every gateway id at zero.
	cost.SetGatewayPricer(StaticPricer, StaticPricerQuiet)
}

// StaticPricer prices one gateway WIRE id (the caller in cost.go has
// already stripped the openai_compatible/ prefix — stripping here too would
// cut a prefixed gateway id ("scaleway/<id>") at its own slash). Unknown
// (or an unpriced operator entry) is ok=false — cost 0 is never a measured
// zero. For a launch or a usage reading: an id in no table earns the
// catalog's warning here.
func StaticPricer(wireModelID string) (inputUSDPerMTok, outputUSDPerMTok float64, known bool) {
	return staticPrice(wireModelID, ResolveCatalog)
}

// StaticPricerQuiet is StaticPricer for a READ — the same sources and the
// same verdict, resolved through ResolveCatalogQuiet. A listing, a pricing
// audit and a cost preview price ids an operator is still typing, and a
// price lookup is not where an unknown id is news; it must not consume the
// per-id memo either, or the launch that follows in the same process says
// nothing.
func StaticPricerQuiet(wireModelID string) (inputUSDPerMTok, outputUSDPerMTok float64, known bool) {
	return staticPrice(wireModelID, ResolveCatalogQuiet)
}

func staticPrice(wireModelID string, resolve func(string, *modelspecs.Registry, func(string) string) Resolved) (inputUSDPerMTok, outputUSDPerMTok float64, known bool) {
	// Live-aware, like every other catalog consumer: the stamp and the
	// estimator must read the SAME sources, or one claims what the other
	// declined (round-2's HIGH). The registry's refresh is background-only;
	// this never blocks on a fetch.
	res := resolve(wireModelID, modelspecs.Default(), os.Getenv)
	if !res.Priced() {
		return 0, 0, false
	}
	return res.Spec.InputCostPerM, res.Spec.OutputCostPerM, true
}
