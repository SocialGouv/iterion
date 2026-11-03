// Package liveenv reads the launch knobs of the live e2e harness from the
// environment: the route every LLM node of a run is re-targeted at, the
// credential route that funds it, and the per-target cost cap.
//
// It lives OUTSIDE the `live`-tagged harness on purpose. The rules here
// decide what a paid run spends and what its committed ledger row claims
// served it, so they are witnessed by the free gate instead of only by a
// run that costs money.
package liveenv

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

const (
	// EnvModel re-targets EVERY LLM node of a live run at one model spec
	// ("openai_compatible/gpt-oss-120b"). It reaches the executor through
	// the "*" selector of model.ModelOverrides, which outranks the node's
	// DSL `model:` — a live re-prove re-targets the whole bot, never one
	// node of it.
	EnvModel = "ITERION_LIVE_MODEL"
	// EnvBackend re-targets every LLM node's backend the same way.
	EnvBackend = "ITERION_LIVE_BACKEND"
	// EnvFundedBy names the credential route that pays for the run, as the
	// ledger's funded_by column records it. A route NAME, never a key:
	// Read refuses the shapes a pasted credential carries, because the
	// ledger is a committed file. That refusal is a bound on an ACCIDENT,
	// not a proof — a short, name-shaped secret (a bare UUID) passes it,
	// so the operator still owes the route's name rather than its key.
	EnvFundedBy = "ITERION_LIVE_FUNDED_BY"
	// EnvMaxCostUSD SETS one target's spend ceiling, in USD. It reaches
	// the run through ir.ApplyBudgetOverrides, which is non-zero-wins in
	// both directions: the operator's launch value replaces the bot's own
	// `budget: max_cost_usd`, LOWERING it or RAISING it. That is the
	// precedence `iterion run --max-cost-usd` already has ("Override the
	// workflow budget's max_cost_usd"), and clamping it to the shipped
	// value instead would silently replace an operator's explicit choice.
	EnvMaxCostUSD = "ITERION_LIVE_MAX_COST_USD"
)

// maxFundedByLen bounds the route NAME. The ledger is committed, so a key
// pasted into EnvFundedBy would land in git: this bound and the key
// prefixes below refuse the paste instead of recording it.
const maxFundedByLen = 64

// credentialEnvRoutes maps a credential env var NAME to the provider route
// it funds. One table, read in both directions: the requireX guards ask
// which route an env var protects, and the ledger asks which env var could
// have funded a route.
var credentialEnvRoutes = map[string]string{
	"OPENAI_COMPATIBLE_API_KEY": modelroute.OpenAICompatible,
	"ANTHROPIC_API_KEY":         "anthropic",
	"OPENAI_API_KEY":            "openai",
	"XAI_API_KEY":               "xai",
	"MOONSHOT_API_KEY":          "moonshot",
	"ZAI_API_KEY":               "zai",
}

// soleFundedRoutes are the routes served by exactly ONE credential env
// var, so naming that var is a reading of the environment rather than a
// choice between pockets. Every other route can be driven by a CLI forfait
// OR a metered key depending on the backend, so the harness derives
// nothing for it and EnvFundedBy is how the operator names the pocket.
//
// Membership is necessary but not sufficient: the derivation also requires
// the variable to be SET (presentCredentials). A gateway can be reached by
// network auth or through the DB credential cascade with no env key at all,
// and naming an unset variable in a committed receipt would be a guess
// dressed as a fact.
var soleFundedRoutes = map[string]bool{modelroute.OpenAICompatible: true}

// Override is the resolved set of launch knobs for one live run. The zero
// value re-targets nothing, caps nothing and names no funder, so a run
// with no knobs behaves exactly as before.
type Override struct {
	// Model is the spec every LLM node is re-pointed at, as written.
	Model string
	// Backend is the backend every LLM node is re-pointed at.
	Backend string
	// FundedBy is the operator's name for the credential route paying.
	FundedBy string
	// MaxCostUSD is the per-target spend ceiling, in USD. Zero = the
	// harness sets none (the bot's own `budget:` stands).
	MaxCostUSD float64
	// presentCredentials holds the credential env var NAMES of
	// credentialEnvRoutes that the environment actually carried at Read
	// time. Captured here, never re-read: a derived funded_by must name a
	// variable that IS set, and the committed row must say what the
	// environment looked like when the run started.
	presentCredentials map[string]bool
}

// Read resolves the knobs through getenv (os.Getenv in the harness, a map
// in a test). A malformed value is an ERROR, never a silent default: a cap
// the harness quietly dropped is a cap the operator believes is in force.
func Read(getenv func(string) string) (Override, error) {
	o := Override{
		Model:    strings.TrimSpace(getenv(EnvModel)),
		Backend:  strings.TrimSpace(getenv(EnvBackend)),
		FundedBy: strings.TrimSpace(getenv(EnvFundedBy)),
	}
	if err := validateFundedBy(o.FundedBy); err != nil {
		return Override{}, err
	}
	o.presentCredentials = presentCredentials(getenv)
	if raw := strings.TrimSpace(getenv(EnvMaxCostUSD)); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Override{}, fmt.Errorf("%s=%q is not a number of US dollars: %w", EnvMaxCostUSD, raw, err)
		}
		if v <= 0 {
			return Override{}, fmt.Errorf("%s=%q is not a spend ceiling: a per-target ceiling is a positive amount, and zero would read as 'inherit the bot's budget' where the operator meant 'spend nothing'", EnvMaxCostUSD, raw)
		}
		o.MaxCostUSD = v
	}
	return o, nil
}

// fundedByName is the shape a route NAME has: it starts alphanumeric and
// carries only the punctuation a name uses. It refuses the charsets a
// credential paste brings — whitespace (a "Bearer <token>" header), base64
// padding and `+`, a leading `-`. Narrow on purpose: this is a shape test,
// not an entropy test (see validateFundedBy).
var fundedByName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// validateFundedBy refuses the shapes a pasted credential carries: a known
// key prefix, a value past the length bound, or a charset no route name
// uses. The error names the bound or the prefix it matched, never the value.
//
// Honest scope: a secret that is short and name-shaped (a bare UUID, a
// dotted token) passes all three, so this guard catches the accident, not a
// determined paste. What makes funded_by safe is that it is a NAME the
// operator chooses; the checks below only stop the obvious slip from
// reaching a committed file.
func validateFundedBy(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxFundedByLen {
		return fmt.Errorf("%s must name a credential ROUTE (e.g. \"pliage-team-key\"), not a key: the value is %d characters, past the %d-character bound, and the ledger it lands in is committed", EnvFundedBy, len(v), maxFundedByLen)
	}
	for _, p := range []string{"sk-", "sk_", "gsk_", "xai-", "Bearer "} {
		if strings.HasPrefix(v, p) {
			return fmt.Errorf("%s starts with %q, a credential prefix — name the route that funds the run, never the credential itself (the ledger it lands in is committed)", EnvFundedBy, p)
		}
	}
	if !fundedByName.MatchString(v) {
		return fmt.Errorf("%s must be a route NAME (alphanumeric start, then letters, digits and ._:/-), and this value is not shaped like one — the ledger it lands in is committed, so a pasted credential must never reach it", EnvFundedBy)
	}
	return nil
}

// presentCredentials reads which of the credential env vars this package
// maps carry a value. Only the NAMES are kept; no value is read into the
// Override, which lands in a committed file.
func presentCredentials(getenv func(string) string) map[string]bool {
	out := map[string]bool{}
	for env := range credentialEnvRoutes {
		if strings.TrimSpace(getenv(env)) != "" {
			out[env] = true
		}
	}
	return out
}

// Empty reports whether no knob carries a directive.
func (o Override) Empty() bool {
	return o.Model == "" && o.Backend == "" && o.FundedBy == "" && o.MaxCostUSD <= 0
}

// Route reads the provider prefix of the model override through
// modelroute.Parse — THE reading of a model spec — or "" when no model
// override is set, the spec is a bare id, or its prefix is not one the
// model registry routes on: `scaleway/gpt-oss-120b` and
// `meta-llama/Llama-3.3-70B` carry a slash that is part of the model id
// (modelroute's own examples), so which provider would serve them is a
// guess, and a guess waves no guard.
func (o Override) Route() string {
	if o.Model == "" {
		return ""
	}
	p := modelroute.Parse(o.Model).Provider
	if !modelroute.IsRoutingProvider(p) {
		return ""
	}
	return p
}

// LaunchKnobs lists the env names this package reads. Every one of them is
// operator-grade (internal/envtrust): the harness's own dotenv loader and
// the engine's refuse a project file that plants one, and a test pins the
// two inventories together so a fifth knob cannot arrive unclassified.
// Those loaders cannot see the repo-root `.env` Task's root `dotenv:`
// plants before any of them runs, so the harness also refuses to run while
// that file assigns a knob at all (DotEnvKnobs).
func LaunchKnobs() []string {
	return []string{EnvModel, EnvBackend, EnvFundedBy, EnvMaxCostUSD}
}

// dotEnvAssignment matches one dotenv line that assigns a variable, in the
// shapes Task's loader accepts: `NAME=…`, `NAME: …`, an `export ` prefix and
// blanks around the name. A comment line never matches: `#` is not a name.
var dotEnvAssignment = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*[=:]`)

// DotEnvKnobs names the launch knobs a dotenv text assigns, sorted. Names
// only — no value is read into the result — and an assignment counts
// whatever its value. Task's root `dotenv:` plants the repo-root `.env`
// into every `task test:live:*` process BEFORE the harness starts, so the
// harness's own loader (which refuses an operator-grade line) never gets to
// see the line, and a value already in the environment cannot say whether
// the operator's shell or the file put it there. The file itself must
// therefore carry no knob: the launch knobs are the operator's shell
// gesture, and a project file must not re-target a run, move its ceiling
// or name the funder of a committed receipt.
func DotEnvKnobs(content string) []string {
	knobs := map[string]bool{}
	for _, k := range LaunchKnobs() {
		knobs[k] = true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		m := dotEnvAssignment.FindStringSubmatch(line)
		if m != nil && knobs[m[1]] {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Retargets reports whether the override re-points the run's LLM nodes —
// a model or a backend directive. The ceiling and the funder are not
// re-targets: one bounds what the run spends, the other names who pays.
func (o Override) Retargets() bool {
	return o.Model != "" || o.Backend != ""
}

// RunKnobs names, in a stable order, the knobs set on this override that
// act on the RUN itself: the re-target pair and the spend ceiling. A target
// that launches its run through a path with no seam for them — a CLI
// command, a delegate backend driven directly — refuses while any is set,
// naming them, rather than spending without what the operator set.
// EnvFundedBy is not among them: it names who pays, and every recording
// path hands it to the ledger row.
func (o Override) RunKnobs() []string {
	var out []string
	if o.Model != "" {
		out = append(out, EnvModel)
	}
	if o.Backend != "" {
		out = append(out, EnvBackend)
	}
	if o.MaxCostUSD > 0 {
		out = append(out, EnvMaxCostUSD)
	}
	return out
}

// UnfundedRetarget says why this machine cannot serve the re-target, or ""
// when it can — or when nothing about the re-target's funding is
// decidable. r is the engine's own availability report (detect.Detect), the
// probe iterion resolves its backends and providers with, so the harness
// never calls a route fundable that the engine would refuse, nor the
// reverse.
//
// A backend directive other than claw re-targets onto a CLI that pays with
// its own login, so that backend's row decides. Otherwise the LLM nodes run
// on claw, which serves the model route's provider: that provider's row
// decides. A bare model id, or a prefix the registry does not route on,
// names no provider — which one serves it is the registry's choice at
// execution — so nothing is decided here, and SupersedesProvider waves no
// guard for it either.
//
// The answer is conservative where the report is: an anthropic route that
// only the subscription OAuth could serve reads as unfunded, because the
// engine's provider row counts the API key alone. It is read from the
// override, not per node: a model-only re-target of a node that stays on
// its DSL CLI backend is judged by the route's provider too. The escape
// hatch is the backend knob — ITERION_LIVE_BACKEND=claude_code lets the
// CLI's own login decide.
func (o Override) UnfundedRetarget(r detect.Report) string {
	if o.Backend != "" && o.Backend != detect.BackendClaw {
		for _, b := range r.Backends {
			if b.Name != o.Backend {
				continue
			}
			if b.Available {
				return ""
			}
			return fmt.Sprintf("%s=%s re-targets every LLM node onto a backend this machine cannot serve: the engine's detection reports %s unavailable", EnvBackend, o.Backend, o.Backend)
		}
		return fmt.Sprintf("%s=%s names a backend the engine's detection does not report, so nothing says this machine can serve it", EnvBackend, o.Backend)
	}
	route := o.Route()
	if route == "" {
		return ""
	}
	for _, p := range r.Providers {
		if p.Name != route {
			continue
		}
		if p.Available {
			return ""
		}
		return fmt.Sprintf("%s=%s routes every LLM node to %q, and this machine cannot fund that route: the engine's detection reports it unavailable (its credential: %s)", EnvModel, o.Model, route, strings.Join(routeCredentialEnvs(route), ", "))
	}
	return fmt.Sprintf("%s=%s routes every LLM node to %q, a provider the engine's detection does not report, so nothing says this machine can fund it", EnvModel, o.Model, route)
}

// routeCredentialEnvs names what an operator sets to fund route: the
// credential variables this package maps to it, plus the gateway's base URL
// for the OpenAI-compatible route, whose client needs both (compatgw).
func routeCredentialEnvs(route string) []string {
	out := credentialEnvsForRoute(route)
	if route == modelroute.OpenAICompatible {
		out = append(out, compatgw.BaseURLEnv)
		sort.Strings(out)
	}
	if len(out) == 0 {
		return []string{"none this harness maps"}
	}
	return out
}

// SupersedesProvider reports whether the override routes every LLM node
// AWAY from provider, so a guard protecting that provider's credential
// must not skip the run: the credential is no longer on the run's path.
//
// False when no model override is set (nothing was re-targeted), false
// when the override's own route IS provider (an override onto anthropic
// still needs anthropic's credential), and false for a bare model id or a
// prefix the registry does not route on — which provider would serve it is
// the registry's decision, and a guard is never waved through on a guess.
func (o Override) SupersedesProvider(provider string) bool {
	if provider == "" {
		return false
	}
	route := o.Route()
	if route == "" {
		return false
	}
	return !strings.EqualFold(route, provider)
}

// SupersedesCredentialEnv is SupersedesProvider keyed on the credential env
// var a guard checks ("ANTHROPIC_API_KEY"). An env var this package does
// not map to a route supersedes nothing: an unknown credential's route is
// unknown, and a guard must keep refusing.
func (o Override) SupersedesCredentialEnv(name string) bool {
	return o.SupersedesProvider(ProviderOfCredentialEnv(name))
}

// ProviderOfCredentialEnv names the provider route a credential env var
// funds, or "" for a name this package does not map.
func ProviderOfCredentialEnv(name string) string {
	return credentialEnvRoutes[strings.TrimSpace(name)]
}

// ModelOverrides folds the knobs into the launch-time override set the
// executor consumes. The selector is "*" — every LLM node — documented to
// outrank the node's DSL backend:/model: in
// pkg/backend/model/model_override.go.
func (o Override) ModelOverrides() model.ModelOverrides {
	var mo model.ModelOverrides
	if o.Model != "" {
		mo.SetModel("*", o.Model)
	}
	if o.Backend != "" {
		mo.SetBackend("*", o.Backend)
	}
	return mo
}

// BudgetOver layers the knob's ceiling over the target's own budget
// override. The env value WINS where both are set: it is the operator's
// launch-time gesture, the precedence `iterion run --max-cost-usd` already
// has over a .bot's `budget:`. ir.ApplyBudgetOverrides then applies the
// result per-field, non-zero wins, in BOTH directions — it can lower a
// shipped cap, which the raise-only steering path
// (runtime.SharedBudget.RaiseCaps) cannot, and it can raise one, which is
// what re-proving a target on a metered route the bot never budgeted for
// needs.
func (o Override) BudgetOver(spec ir.BudgetOverrides) ir.BudgetOverrides {
	if o.MaxCostUSD > 0 {
		spec.MaxCostUSD = o.MaxCostUSD
	}
	return spec
}

// FundedByFor resolves the ledger's funded_by column for a run whose LLM
// nodes were served by specs.
//
// The operator's EnvFundedBy wins — an explicit choice is never silently
// replaced. Otherwise the route is DERIVED, under two conditions that
// together make the value a READING rather than a guess: the route must be
// served by a single credential env var (soleFundedRoutes), and that
// variable must have been SET when Read captured the environment. A
// gateway reached by network auth, or served through the DB credential
// cascade, therefore yields "" instead of naming a variable nobody set.
// Several funders in one run fold to a sorted, comma-separated list — a
// stable key, so the column does not depend on the order the steps
// finished in. A run the harness cannot attribute yields "", which reads
// as "unknown" rather than as a guess.
func (o Override) FundedByFor(specs []string) string {
	if o.FundedBy != "" {
		return o.FundedBy
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		route := modelroute.Parse(strings.TrimSpace(spec)).Provider
		if route == "" || !soleFundedRoutes[route] {
			continue
		}
		for _, env := range credentialEnvsForRoute(route) {
			if o.presentCredentials[env] {
				seen[env] = true
			}
		}
	}
	return joinSorted(seen)
}

// credentialEnvsForRoute reads credentialEnvRoutes backwards: EVERY env var
// name whose route is route, sorted. All of them, not the first the map
// range happened to yield — a verdict read off Go's map iteration order is
// a different receipt per run, and the day a second variable maps to one
// route the "first match" would pick a machine-dependent one.
func credentialEnvsForRoute(route string) []string {
	var out []string
	for env, r := range credentialEnvRoutes {
		if r == route {
			out = append(out, env)
		}
	}
	sort.Strings(out)
	return out
}

// joinSorted renders a set as a sorted, comma-separated list.
func joinSorted(set map[string]bool) string {
	if len(set) == 0 {
		return ""
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
