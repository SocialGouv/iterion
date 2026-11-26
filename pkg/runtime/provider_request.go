package runtime

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Keep the parent's context verdicts in one list for both typed API paths.
// In particular a bare "prompt is too long" retains EXECUTION_FAILED and
// its fresh-sample retry; it never becomes an operator-parameter refusal.
var providerContextMarkers = []string{"context_length_exceeded", "context length"}

func matchesProviderContext(message string) bool {
	for _, marker := range providerContextMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// The closed list reads ONLY a typed provider message. No status or error
// class (notably vLLM's catch-all BadRequestError) proves a request defect.
// Unsupported parameter/value must name one of the request controls below;
// messages/tool results/images and content-policy refusals stay outside it.
var providerParameterRefusals = []*regexp.Regexp{
	regexp.MustCompile(`\breasoning_effort must be\b`),
	regexp.MustCompile(`\bunsupported (?:parameter|value):? ["'\x60]?(?:reasoning_effort|temperature|top_p|top_k|max_tokens|max_completion_tokens|presence_penalty|frequency_penalty|seed|stop|logprobs|top_logprobs|n|stream|response_format)["'\x60]?(?:[ :,.]|$)`),
	regexp.MustCompile(`\bmax_(?:tokens|completion_tokens) is too large\b`),
}

func invalidProviderParameter(message string) bool {
	message = strings.ToLower(message)
	if matchesProviderContext(message) || strings.Contains(message, "messages[") || strings.Contains(message, "messages.") {
		return false
	}
	for _, pattern := range providerParameterRefusals {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

// routeLocalCodes are the verdicts that condemn ONE route rather than the
// node: an operator-parameter refusal (the closed list) and an answer that
// stayed off its schema after its corrections. Another route of the chain
// can still serve either way.
var routeLocalCodes = map[ErrorCode]bool{
	ErrCodeProviderRequestInvalid: true,
	ErrCodeSchemaValidation:       true,
}

// refusalCodes maps each provider refusal (api.RefusalKind) to the code a
// node refused that way fails with. A budget refusal is the spend ceiling
// every backend parks on (delegate.SpendCeilingRefusal): USAGE_LIMIT_BLOCKED,
// whose run waits and is probed hourly, since a gateway budget often reopens
// by itself (a windowed budget, a released reservation). Every other refusal
// is deterministic in retrypolicy: the same request meets the same refusal,
// so neither node recovery nor redelivery nor an automatic resume runs it
// again.
var refusalCodes = map[api.RefusalKind]ErrorCode{
	api.RefusalBudget:     ErrCodeUsageLimitBlocked,
	api.RefusalCapability: ErrCodeCapabilityUnsupported,
	api.RefusalGrammar:    ErrCodeSchemaUnusable,
	api.RefusalRequest:    ErrCodeProviderRequestInvalid,
}

// refusalPrecedence orders the deterministic refusal codes for a chain whose
// every route was refused, each its own way — the order api.RefusalOf reads
// the kinds in, so the chain's code and its remedy name the same refusal:
// the model, then the schema, then the request, whatever order the routes
// ran in. A budget refusal is not among them: a chain holding one is read
// as a usage window (chainWaitsOnUsageWindow).
var refusalPrecedence = []ErrorCode{
	ErrCodeCapabilityUnsupported,
	ErrCodeSchemaUnusable,
	ErrCodeProviderRequestInvalid,
}

// IsProviderRefusalCode reports a code a deterministic provider refusal fails
// a node with (refusalPrecedence): a relaunch of the same work meets the same
// refusal, whatever else raised the code too — each is deterministic.
func IsProviderRefusalCode(code ErrorCode) bool {
	return slices.Contains(refusalPrecedence, code)
}

// providerRefusalCode reads the provider refusal err's chain carries
// (api.RefusalOf) as the code it fails the node with. A request-level
// refusal that names the context window is left to the context reading,
// whose compaction can shrink the next request.
func providerRefusalCode(err error) (ErrorCode, bool) {
	kind := api.RefusalOf(err)
	code, ok := refusalCodes[kind]
	if !ok {
		return "", false
	}
	if kind == api.RefusalRequest && matchesProviderContext(strings.ToLower(err.Error())) {
		return "", false
	}
	return code, true
}

// refusalRemedies is the operator's remedy for each provider refusal.
var refusalRemedies = map[api.RefusalKind]string{
	api.RefusalBudget:     "the provider refused the call because the spend budget of its key, team or account is exhausted — raise that budget (or add credits) at the provider or gateway; a declared fallback route is taken within the run; the run then parks, and the hourly probe relaunches it — the relaunch reuses the same key until the budget clears",
	api.RefusalCapability: "the model cannot take an input this node sends it (e.g. an image to a text-only model) — route the node to a model that accepts it, or stop sending that input, then resume",
	api.RefusalGrammar:    "the gateway cannot compile the request's schemas (tool or output) into a decoding grammar — simplify the schema construct it names, or serve the node on a model or gateway that accepts it, then resume",
	api.RefusalRequest:    "the provider refused the request itself — fix what it names, then resume",
}

// ProviderRefusalRemedy is the operator's remedy for the provider refusal err
// carries when that refusal is what classified it as code, naming the
// provider's own words, bounded before it reaches persisted hints; ""
// otherwise. The words are those of every error of the chain refused the
// way the code says — sorted and deduplicated, so a fallback chain's remedy
// names the refusal its code names, in whatever order its routes ran.
func ProviderRefusalRemedy(err error, code ErrorCode) string {
	refused, ok := providerRefusalCode(err)
	if !ok || refused != code {
		return ""
	}
	kind := api.RefusalOf(err)
	var words []string
	for _, r := range api.RefusalsOf(err) {
		if r.Refusal() == kind {
			words = append(words, refusalMessage(r))
		}
	}
	slices.Sort(words)
	return refusalRemedies[kind] + "; provider refused: " + iterlog.Truncate(strings.Join(slices.Compact(words), " | "), 2048)
}

// refusalMessage is the provider's own explanation a refusal carries: the
// extracted message, else the raw body.
func refusalMessage(refusal api.ProviderRefusal) string {
	typed, ok := refusal.(*api.APIError)
	switch {
	case !ok:
		return refusal.Error()
	case typed.Message != "":
		return typed.Message
	}
	return typed.Body
}

// routeLocal reports a route's verdict that condemns that route alone: a
// route-local code, or a deterministic provider refusal (another route —
// another key, model or gateway — may serve what this one refused). A budget
// refusal is read with the usage windows instead (chainWaitsOnUsageWindow).
func routeLocal(code ErrorCode, cause error) bool {
	if routeLocalCodes[code] {
		return true
	}
	refused, ok := providerRefusalCode(cause)
	return ok && refused == code && code != ErrCodeUsageLimitBlocked
}

// classifyRefusedChain decides an exhausted fallback chain that holds at
// least one route-local verdict (routeLocal). Only a chain whose EVERY route
// ended that way keeps one of those codes — SCHEMA_VALIDATION when any
// route's answer stayed off its schema, else the first refusal code of
// refusalPrecedence a route met, in whatever order the routes ran. Otherwise
// the routes that failed for another reason decide, read like any other
// chain: a rescue route's transient outage stays transient. It declines
// (decided=false) a chain with no route-local verdict — the reading of
// ClassifyError stays the one it always had — and a chain a RuntimeError
// encloses, whose code is the engine's own verdict on the whole chain.
func classifyRefusedChain(err error) (code ErrorCode, decided bool) {
	chain := exhaustedChainToRead(err)
	if chain == nil {
		return "", false
	}
	var others []error
	met := map[ErrorCode]bool{}
	for _, cause := range chain.Errs {
		c := ClassifyError(cause)
		if !routeLocal(c, cause) {
			others = append(others, cause)
			continue
		}
		met[c] = true
	}
	switch len(others) {
	case 0:
		if met[ErrCodeSchemaValidation] {
			return ErrCodeSchemaValidation, true
		}
		for _, c := range refusalPrecedence {
			if met[c] {
				return c, true
			}
		}
		return ErrCodeProviderRequestInvalid, true
	case len(chain.Errs):
		return "", false
	}
	return ClassifyError(&model.ErrChainExhausted{Chain: chain.Chain, Errs: others}), true
}

// exhaustedChainToRead is the exhausted fallback chain a chain reading may
// decide, or nil: no chain in err, an empty one, or one a RuntimeError
// encloses — whose code is the engine's own verdict on the whole chain, so no
// reading of its routes overrides it.
func exhaustedChainToRead(err error) *model.ErrChainExhausted {
	var chain *model.ErrChainExhausted
	if !errors.As(err, &chain) || len(chain.Errs) == 0 {
		return nil
	}
	var enclosing *RuntimeError
	if errors.As(err, &enclosing) {
		var inner *model.ErrChainExhausted
		if errors.As(enclosing.Cause, &inner) && inner == chain {
			return nil
		}
	}
	return chain
}

// chainWaitsOnUsageWindow reports an exhausted fallback chain that holds at
// least one route refused on a provider usage window waiting cures, whatever
// its other routes say. The node parks on the window (USAGE_LIMIT_BLOCKED)
// and the run resumes when it reopens; a credential another route found dead
// is a fact the diagnostic names (ChainRouteVerdicts), not the reason for the
// stop. Read BEFORE the credential check, on every route: errors.As over
// Unwrap() []error finds the first *delegate.ErrAuthFailed of the set
// wherever it sits, so a dead route anywhere in the walk used to pause the
// run for a human with a five-hour wait as its whole cause (#3032). A chain
// whose routes are all dead credentials holds no window and keeps
// AUTH_FAILED.
//
// A route refused by the ACCOUNT (a spend ceiling, a fair-usage restriction:
// delegate.IsAccountRefusal) names no reset, and no wait is known to cure it.
// Such routes park the run as a window does — the chain's own verdict, in
// every order of its routes — except beside a dead credential: the human the
// pause calls can revive that route, while hourly blind parks would only
// spend the attempts against two walls that both need one.
func chainWaitsOnUsageWindow(err error) bool {
	chain := exhaustedChainToRead(err)
	if chain == nil {
		return false
	}
	accountRefused, deadCredential := false, false
	for _, cause := range chain.Errs {
		if cause == nil {
			continue
		}
		switch ClassifyError(cause) {
		case ErrCodeUsageLimitBlocked:
			if !delegate.IsAccountRefusal(cause) {
				return true
			}
			accountRefused = true
		case ErrCodeAuthFailed:
			deadCredential = true
		}
	}
	return accountRefused && !deadCredential
}

// UsageWindowRoutes returns the routes of the exhausted fallback chain err
// holds — wherever it sits, a RuntimeError enclosing it included — that a
// provider usage window refused, account refusals included, in walk order;
// nil when err holds no chain or none of its routes met a window. The runner
// reads the instant its usage park waits for from these routes' own refusals:
// the chain's message names its LAST route only, and errors.As finds the
// FIRST refusal of the walk, so neither is a fact about the earliest route to
// reopen.
func UsageWindowRoutes(err error) []error {
	var chain *model.ErrChainExhausted
	if !errors.As(err, &chain) {
		return nil
	}
	var out []error
	for _, route := range chain.Errs {
		if route != nil && ClassifyError(route) == ErrCodeUsageLimitBlocked {
			out = append(out, route)
		}
	}
	return out
}

// ChainRouteVerdicts is the per-route evidence of an exhausted fallback
// chain — each route's own classification and error, in walk order — or nil
// when err holds no such chain. The node's single code says what the run
// does next; this says what every route met, so a dead credential behind a
// usage window stays visible on the node_recovery event.
//
// Each verdict names the route it is about (model.ErrChainExhausted.Routes):
// the backend, the model spec, the provider, the claude_code session label
// and the credential keys its call resolved (credroute.PutEventKeys), under
// the keys a delegate event carries them — so a reader books a verdict on the
// credential that met it, the way it books the route's spend, and never on a
// credential another route of the same chain spent. A typed usage-window
// refusal carries its reset instant too (reset_at).
func ChainRouteVerdicts(err error) []map[string]any {
	var chain *model.ErrChainExhausted
	if !errors.As(err, &chain) || len(chain.Errs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(chain.Errs))
	for i, cause := range chain.Errs {
		verdict := map[string]any{"route": i}
		if cause != nil {
			verdict["code"] = string(ClassifyError(cause))
			verdict["error"] = cause.Error()
			var rl *delegate.ErrRateLimited
			if errors.As(cause, &rl) && rl.Kind == delegate.RateLimitKindUsageWindow && !rl.ResetAt.IsZero() {
				verdict["reset_at"] = rl.ResetAt.UTC().Format(time.RFC3339Nano)
			}
		}
		route := chain.Route(i)
		for key, value := range map[string]string{
			"backend": route.Backend, "route_model": route.Model, "provider": route.Provider, "fingerprint": route.Source,
		} {
			if value != "" {
				verdict[key] = value
			}
		}
		credroute.PutEventKeys(verdict, route.Credential)
		out = append(out, verdict)
	}
	return out
}

func providerParameterCause(err error) string {
	var typed *api.APIError
	if errors.As(err, &typed) && typed.StatusCode == 400 && invalidProviderParameter(typed.Message) {
		return typed.Message
	}
	var stream *model.APIError
	if errors.As(err, &stream) && stream.StatusCode == 400 && invalidProviderParameter(stream.Message) {
		return stream.Message
	}
	return "request parameter rejected"
}
