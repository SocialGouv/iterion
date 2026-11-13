package runtime

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"strings"
)

// ClassifyError is the shared classification used by execution boundaries and
// recovery dispatch, including runs without a dispatcher.
func ClassifyError(err error) ErrorCode {
	if err == nil {
		return ""
	}
	// An UNDECIDED remote effect, checked before everything else including
	// the RuntimeError code: a failure that may have already committed a
	// mutation must not be reclassified into any bucket that retries. The
	// transport wording of such a failure ("connection reset", "i/o
	// timeout") matches the network-transient needles below almost by
	// definition, so any later position would hand it to the exponential
	// backoff — retrying the exact call whose effect is unknown.
	if IsAmbiguousEffect(err) {
		return ErrCodeAmbiguousEffect
	}
	// Before the RuntimeError code: a route of the chain may carry its own
	// RuntimeError, and errors.As must not let that one route decide for
	// the others. A usage window waiting cures, on any route, decides first —
	// the run waits for it to reopen, whatever the other routes met (an
	// account refusal parks the same way, unless a route found a dead
	// credential: chainWaitsOnUsageWindow).
	if chainWaitsOnUsageWindow(err) {
		return ErrCodeUsageLimitBlocked
	}
	if code, decided := classifyRefusedChain(err); decided {
		return code
	}
	var rerr *RuntimeError
	if errors.As(err, &rerr) {
		return rerr.Code
	}
	// A refused GRANT, read by interface before the tool-failure arm below
	// and before every needle: a native action's denial reaches here wrapped
	// as a tool node's failure, and the same IR meets the same
	// `capabilities:` on every attempt, so the catch-all's one retry would
	// only buy the same refusal. The interface — not a concrete type —
	// because the engine must not learn what a transport is
	// (CapabilityDenial, ADR-151).
	if IsCapabilityDenial(err) {
		return ErrCodeCapabilityDenied
	}
	// One of iterion's host tools (board, run store, watch) refused a node
	// that does not hold its capability. The node's declaration and the
	// run's trust decide it, and neither changes between attempts.
	if errors.Is(err, tool.ErrHostCapabilityNotHeld) {
		return ErrCodeToolFailedPermanent
	}
	// A tool node's failure, checked by TYPE before any provider reading:
	// its stderr is data, and an HTTP-looking line in it ("status code 400",
	// "error 401") says nothing about the model provider. Every execution
	// boundary carries this type, `iterion resume` and child runs included.
	// The network reading still applies: a command whose own I/O hit a
	// reset connection or a DNS timeout gets the network backoff, as any
	// other failure does.
	var toolFailure *model.ToolExecutionError
	if errors.As(err, &toolFailure) {
		if matchesNetworkTransient(strings.ToLower(err.Error())) {
			return ErrCodeNetworkTransient
		}
		return ErrCodeExecutionFailed
	}
	// A credential the provider rejected. Checked by TYPE, before any
	// message matching: the needles below only catch the wordings that
	// happen to be in the list today, so a backend that reports "upstream
	// 401" instead of "API error 401" would be retried against a dead
	// credential rather than paused for re-auth.
	var authFailed *delegate.ErrAuthFailed
	if errors.As(err, &authFailed) {
		return ErrCodeAuthFailed
	}
	var unavailable *delegate.ErrModelUnavailable
	if errors.As(err, &unavailable) {
		return ErrCodeModelUnavailable
	}
	// The node's own declaration is what the backend refused. Checked by
	// TYPE first, like the credential above: the needle below only
	// catches today's wording.
	var unsupported *delegate.ErrCapabilityUnsupported
	if errors.As(err, &unsupported) {
		return ErrCodeCapabilityUnsupported
	}
	// Same code for the same class on the host plane: a CLI-agent backend no
	// published image ships, whose executable is absent from this host's PATH
	// (#1643). Deterministic — retrying cannot install a CLI.
	var cliAbsent *delegate.ErrBackendCLIAbsent
	if errors.As(err, &cliAbsent) {
		return ErrCodeCapabilityUnsupported
	}
	var outputSchema *model.OutputSchemaError
	if errors.As(err, &outputSchema) {
		return ErrCodeSchemaValidation
	}
	var schemaUnusable *delegate.ErrSchemaUnusable
	if errors.As(err, &schemaUnusable) {
		return ErrCodeSchemaUnusable
	}
	var rateLimited *delegate.ErrRateLimited
	if errors.As(err, &rateLimited) {
		if rateLimited.Kind == delegate.RateLimitKindUsageWindow {
			return ErrCodeUsageLimitBlocked
		}
		return ErrCodeRateLimited
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 {
			return ErrCodeRateLimited
		}
		body := strings.ToLower(apiErr.Message + " " + apiErr.Body)
		if matchesProviderContext(body) {
			return ErrCodeContextLengthExceeded
		}
		// 5xx and 408 (Request Timeout) from the upstream API are
		// textbook transient failures: gateway timeouts, upstream
		// connection resets, service overload, deploys mid-rollout.
		// Route them to NETWORK_TRANSIENT so the longer exponential
		// backoff recipe (~10 min) absorbs the outage instead of the
		// catch-all 1-shot ExecutionFailed retry. 409 is intentionally
		// excluded — it's usually a logical conflict, not a transient
		// network issue.
		if apiErr.StatusCode == 408 || (apiErr.StatusCode >= 500 && apiErr.StatusCode <= 599) {
			return ErrCodeNetworkTransient
		}
		// A 403 can deny one model while the credential remains valid for
		// others. Explicit credential failures take precedence; a bare
		// 401/403 keeps the authentication remedy.
		if delegate.ModelAccessDenied(apiErr.StatusCode, body) && !matchesAuthFailed(body) {
			return ErrCodeModelUnavailable
		}
		if apiErr.StatusCode == 401 || apiErr.StatusCode == 403 {
			return ErrCodeAuthFailed
		}
		// A model this caller may not have. 404 says it by status; the
		// wordings say it under a 400, which the backends use for a
		// model gated on a minimum client release. Classify these before
		// other invalid requests so the remedy still names model access.
		if apiErr.StatusCode == 404 || matchesModelUnavailable(body, 0) {
			return ErrCodeModelUnavailable
		}
		// A 400 is terminal only when the provider's own message names a
		// request parameter the operator set (the closed list of
		// invalidProviderParameter). A bare 400 is NOT: one raised mid-turn
		// can be the model's own tool arguments, an image a tool produced or
		// a moderation verdict on generated text, and those differ on the
		// next sample.
		if apiErr.StatusCode == 400 && invalidProviderParameter(apiErr.Message) {
			return ErrCodeProviderRequestInvalid
		}
		return ErrCodeExecutionFailed
	}
	// A stream error event is a separate type carrying the provider's status
	// and message. Its 400 reads the same context markers and the same
	// closed parameter list as the HTTP arm; anything else — vLLM's
	// BadRequestError catch-all first — goes on to the reading below,
	// whose verdict it always had.
	var streamErr *model.APIError
	if errors.As(err, &streamErr) && streamErr.StatusCode == 400 {
		if matchesProviderContext(strings.ToLower(streamErr.Message)) {
			return ErrCodeContextLengthExceeded
		}
		if invalidProviderParameter(streamErr.Message) {
			return ErrCodeProviderRequestInvalid
		}
	}
	// A node ended by the bound its own `timeout:` declared, read BY TYPE
	// and before every needle below — the same discipline the credential
	// above follows, and for the same reason: the needles catch wordings.
	// What a cut node reports is whatever its backend saw — a sandboxed
	// crossing's killed runner, the container's whole stderr, the bare
	// context.DeadlineExceeded the in-node retry loop hands up when the
	// bound cuts its backoff — so read by words, the last of those matches
	// "context deadline exceeded" and the node is re-run six times against
	// the one bound its author declared, while the others fall to the
	// catch-all retry (#2456). TIMEOUT is that bound expiring: the code the
	// engine already persists for a deadline it cut a node at
	// (store.FailureTimeout), and its recipe does not re-run the node.
	//
	// AFTER every typed branch above, deliberately: a cut that wraps a usage
	// window, a rejected credential or an unusable schema is still that, and
	// those are what an operator can act on. The boundary that costs is
	// named rather than hidden: two of those branches RETRY (a 429, a 5xx
	// through *api.APIError), so a cut wrapping one of them is re-run under
	// a fresh bound — unchanged from before this arm existed, and rarely
	// reached, because the in-node retry loop eats such a failure and hands
	// up the bare context error this arm catches. A per-request timeout
	// INSIDE the node carries no type at all and is not this — it stays a
	// transient fault for the needles below.
	var nodeDeadline *model.ErrNodeDeadline
	if errors.As(err, &nodeDeadline) {
		return ErrCodeTimeout
	}
	// String-pattern fallback for unstructured errors that bubble up
	// from out-of-process backends (claude_code CLI, iterion sandbox
	// drivers). The Anthropic CLI emits "API Error: Unable to connect
	// to API (FailedToOpenSocket)" when the host loses the socket
	// mid-request; OpenAI tools emit variations on "connection refused"
	// / "connection reset"; Go's net/http surface prints "no such host"
	// for DNS failures + "i/o timeout" for read/dial timeouts. None of
	// these arrive as *api.APIError because the upstream never produced
	// an HTTP response; the wrapping layers stringify the lower-level
	// error and we don't have a typed channel for it. Match the known
	// phrases case-insensitively and route to NETWORK_TRANSIENT so the
	// recovery dispatcher applies the longer exponential backoff
	// instead of the catch-all 1-shot retry.
	msg := strings.ToLower(err.Error())
	// Credential rejections often arrive as plain strings: the claw
	// runner is an out-of-process subprocess whose typed *api.APIError is
	// flattened to text by the time it bubbles up through the backend
	// wrappers (e.g. "claw backend: ... openai: API error 401: Provided
	// authentication token is expired. Please try signing in again").
	// Match those before the network needles so they pause-for-reauth
	// instead of falling to the 1-shot ExecutionFailed retry.
	if matchesAuthFailed(msg) {
		return ErrCodeAuthFailed
	}
	// A usage-window refusal whose TYPE did not survive a hop — a layer that
	// formatted with %v, a runner's text relay — read from the marker the
	// typed refusal prints (delegate.UsageWindowInFlattenedError): the same
	// last-resort reading the runner arms its usage park on, so a route that
	// carries it decides an exhausted chain the way its typed form does, and
	// a node that fails on it parks rather than spending a retry on a wall.
	// After the credential needles: a refusal quoting both is the
	// credential's to answer, as its typed forms are read above.
	if delegate.UsageWindowInFlattenedError(msg) {
		return ErrCodeUsageLimitBlocked
	}
	// The out-of-process host (the claw runner) flattens the typed error
	// above into text before it reaches here, so the wording is read too
	// — this is the shape that was actually measured in the cloud.
	if strings.Contains(msg, "parse explicitschema") {
		return ErrCodeSchemaUnusable
	}
	// Explicit invalid credentials take precedence over a model denial,
	// but a 403 alone does not prove that the credential is invalid.
	status := 0
	if strings.Contains(msg, "error 403") {
		status = 403
	} else if strings.Contains(msg, "error 401") {
		status = 401
	}
	if matchesModelUnavailable(msg, status) {
		return ErrCodeModelUnavailable
	}
	if strings.Contains(msg, "error 403") || strings.Contains(msg, "error 401") {
		return ErrCodeAuthFailed
	}
	if matchesNetworkTransient(msg) {
		return ErrCodeNetworkTransient
	}
	return ErrCodeExecutionFailed
}

// matchesNetworkTransient is the ONE reading of networkTransientNeedles:
// the tool branch and the flattened-string fallback of ClassifyError both
// call it. msg is lowercase.
func matchesNetworkTransient(msg string) bool {
	for _, needle := range networkTransientNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// modelUnavailableNeedles enumerates lowercase substrings that say the
// provider will not serve the MODEL, whatever the request contains. ALL
// entries MUST be lowercase — both callers lowercase first.
var modelUnavailableNeedles = []string{
	// Scoped to the MODEL on purpose: a bare "requires a newer version"
	// is any tool announcing a version floor, and a false positive here
	// PARKS a run — the disposition is deterministic.
	"model requires a newer version",
	"api error 404",   // claw/openai verbatim: the model id reached no endpoint
	"model_not_found", // OpenAI error code field
	"unknown model",   // gateway wording for an id no backend claims
}

// matchesModelUnavailable is the ONE reading of that list. Both the
// typed *api.APIError branch and the flattened-string fallback call it,
// because an out-of-process backend (the claw runner) stringifies its
// typed error before it reaches here — the same reason the credential
// needles exist twice over.
func matchesModelUnavailable(msg string, status int) bool {
	for _, needle := range modelUnavailableNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	// Without a forbidden status, "not available" can describe temporary
	// maintenance. Only a 403 makes that wording a model access denial.
	return delegate.ModelAccessDenied(status, msg)
}

// authFailedNeedles enumerates lowercase substrings that indicate the
// model provider rejected our credentials (not a transient fault). ALL
// entries MUST be lowercase — Classify lowercases the err string first.
var authFailedNeedles = []string{
	"authentication token is expired", // OpenAI/ChatGPT-forfait expired OAuth access token
	"please try signing in",           // OpenAI/codex re-auth prompt
	"invalid api key",                 // OpenAI/Anthropic bad key
	"incorrect api key",               // OpenAI verbatim
	"invalid_api_key",                 // OpenAI error code field
	"authentication_error",            // Anthropic error type field
	"oauth token has expired",         // Anthropic OAuth (claude_code) expiry
}

func matchesAuthFailed(msg string) bool {
	if delegate.ExplicitCredentialFailure(msg) {
		return true
	}
	for _, needle := range authFailedNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// networkTransientNeedles enumerates the lowercase substrings that
// indicate a transient connectivity failure to the upstream model API.
// Kept as a plain slice so hosts patching this file can append site-
// specific phrases (corporate proxy timeouts, etc.) without touching
// the matcher logic. ALL entries MUST be lowercase — Classify lowercases
// the err string before checking.
var networkTransientNeedles = []string{
	"failedtoopensocket",        // anthropic claude CLI verbatim ("Unable to connect to API (FailedToOpenSocket)")
	"unable to connect to api",  // anthropic claude CLI prefix
	"connection refused",        // tcp refused (host present, port closed / firewall)
	"connection reset",          // tcp reset by peer (mid-request drop)
	"no such host",              // dns failure (transient resolver outage)
	"i/o timeout",               // go net package timeouts (dial / read / write)
	"context deadline exceeded", // request-level timeout in upstream client
	"network is unreachable",    // host has no route (eg vpn dropped)
	"no route to host",          // routing-table drop (often transient)
	"tls handshake timeout",     // tls negotiation hung (proxy / mitm flap)
	"unexpected eof",            // SSE / streaming response cut short by peer
	"http: eof",                 // net/http verbatim: server hung up mid-stream
	"http2: timeout",            // golang.org/x/net/http2: "http2: timeout awaiting response headers"
	"http2: server sent goaway", // http/2 graceful shutdown signal from upstream mid-request
	"upstream connect error",    // envoy / cloudfront gateway: e.g. "upstream connect error or disconnect/reset before headers"
	"server closed idle connection",
}
