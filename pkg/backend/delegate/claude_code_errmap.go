package delegate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// apiErrorResultStatusRe extracts the HTTP-ish status code the claude CLI
// prints when it renders an upstream API failure AS its result text, e.g.
// "API Error: 529 Overloaded" or "API Error: 503 {...}" — and the bracketed
// form an Anthropic-shaped facade relays, "API Error: [500][Operation
// failed][<request id>]". The bracketed status went unparsed once, so the
// render fell through to the connectivity markers, matched none, and became
// the node's output: the graph continued on an error message for 283
// minutes. Exactly three digits, closed by a bracket or a word boundary — a
// longer number is not a status and still falls through.
var apiErrorResultStatusRe = regexp.MustCompile(`(?i)^api error:?\s*\[?(\d{3})(?:\]|\b)`)

// isTransientAPIErrorResult reports whether a claude CLI "successful" result
// string is actually a rendered upstream API failure of a TRANSIENT class
// (rate-limit / server / overload / connectivity) that a bounded retry can
// recover from. The CLI occasionally finishes a stream with subtype=success
// yet puts an unrecoverable error in the result text; treating that as a
// valid node output silently corrupts the run (and any inheriting session).
//
// Precision matters — a genuine assistant answer must not be mistaken for an
// error. Two guards keep false positives near-zero: the text must be SHORT
// (the CLI's bare error render is; a real answer that merely discusses an API
// error is longer and embedded) and must BEGIN with "api error". Every 5xx
// and the retryable 4xx (408/409/425/429) return true; another 4xx
// client/auth status returns false so it surfaces as the visible output (a
// retry won't fix a misconfig) — unless its text carries a connectivity
// marker, which classified it before the status was parsed; a render with no
// status at all falls back to those markers.
func isTransientAPIErrorResult(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || len(t) >= 400 {
		return false
	}
	low := strings.ToLower(t)
	if !strings.HasPrefix(low, "api error") {
		return false
	}
	if m := apiErrorResultStatusRe.FindStringSubmatch(t); m != nil {
		switch {
		case m[1][0] == '5':
			// Every 5xx, not an allow-list: a CDN or proxy in front of a
			// facade renders 520-530 and the like, and an unlisted server
			// status that fell out of an allow-list here became the node's
			// answer.
			return true
		case m[1] == "408" || m[1] == "409" || m[1] == "425" || m[1] == "429":
			return true
		}
		// A 4xx client/auth error — a retry won't fix it — unless the text
		// itself carries a connectivity marker ("[499][Connection error]"):
		// parsing the status must not lose what the marker already said.
		return MatchesNetworkSignature(low)
	}
	// No parseable status code (e.g. "API Error: Connection error.") → fall
	// back to the shared connectivity-marker classifier.
	return MatchesNetworkSignature(low)
}

// isModelUnavailableResult detects the claude CLI's "bad/unauthorized model"
// completion. An invalid or inaccessible `--model` does NOT fail the stream
// (subtype=success, IsError=false); the CLI renders its model-error sentence AS
// the result text, e.g. "There's an issue with the selected model
// (openai/gpt-5.5). It may not exist or you may not have access to it. Run
// --model to pick a different model." Untyped, that prose flows into the
// formatting passes and finally surfaces as an opaque schema "missing required
// field" error, hiding the real cause. Detecting it lets the node fail fast and
// legibly. Bounded length + two distinctive markers keep this from matching a
// model's own prose that happens to discuss "selected model".
func isModelUnavailableResult(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || len(t) >= 400 {
		return false
	}
	low := strings.ToLower(t)
	return strings.Contains(low, "selected model") &&
		(strings.Contains(low, "may not exist") || strings.Contains(low, "access to it"))
}

// isAuthErrorResult detects the claude CLI's rendered AUTHENTICATION failure —
// a dead/expired forfait CLAUDE_CODE_OAUTH_TOKEN or a rejected API key. Like the
// model-unavailable case it completes the stream (subtype=success, IsError=true)
// with the auth error AS the result text, e.g. "Failed to authenticate. API
// Error: 401 Invalid bearer token". Untyped, that flows into the formatting
// passes and surfaces as an opaque schema "missing required field" error,
// masking a simply-dead credential (this cost a full debugging session once).
// Bounded length + distinctive auth markers keep an agent quoting these phrases
// mid-answer from false-positiving. Non-transient — a retry can't revive a dead
// credential; the caller fails fast with a legible auth error.
func isAuthErrorResult(s string) bool {
	t := strings.TrimSpace(s)
	// A malformed Authorization value (a secret that is not a token at
	// all — the paid case embedded a whole CLI login transcript) renders
	// as an API Error whose length is unbounded because the garbage value
	// is quoted back. Prefix-anchored: the WHOLE result must be the API
	// error, so an agent merely quoting one mid-answer cannot match.
	if strings.HasPrefix(t, "API Error: Header '") && strings.Contains(t, "has invalid value") {
		return true
	}
	// Real auth renders are short one-liners (like a rate-limit notice); the cap
	// keeps an agent discussing auth in a long answer from false-positiving.
	if t == "" || len(t) > 200 {
		return false
	}
	// A 401 status is the provider's own verdict on the credential, whatever
	// prose (or none) follows it: a facade renders "API Error:
	// [401][Unauthorized][<id>]", which matches no signature below and would
	// otherwise flow on as the node's answer. Prefix-anchored by the regex,
	// short by the cap. A 403 is a refusal, not a credential verdict — see
	// isRefusedRequestResult.
	if m := apiErrorResultStatusRe.FindStringSubmatch(t); m != nil && m[1] == "401" {
		return true
	}
	low := strings.ToLower(t)
	for _, sig := range []string{
		"invalid bearer token",
		"failed to authenticate",
		"authentication_error",
		"invalid api key",
		"invalid x-api-key",
		"oauth token has expired",
		"oauth token expired",
		// The CLI's no-credential render (a pod where neither the OAuth
		// forfait nor an API key reached the spawn): a bare
		// "Not logged in \u00b7 Please run /login" in exit 0.
		"not logged in",
		"please run /login",
	} {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

// isRefusedRequestResult detects a 403 render, bare or bracketed: the
// upstream refused the request — a facade or CDN block, a policy refusal —
// which a retry will not change, but which is not a verdict on the
// credential either (a dead token renders 401). Same brevity and prefix
// guards as the transient classifier.
func isRefusedRequestResult(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || len(t) >= 400 {
		return false
	}
	m := apiErrorResultStatusRe.FindStringSubmatch(t)
	return m != nil && m[1] == "403"
}

// redactAuthRender strips the quoted credential out of an auth render:
// the malformed-Authorization shape quotes the rejected secret back
// verbatim, and this text flows into durable, run-readable state (the
// run document, the failure events, the operator log) — none of which
// is scrubbed. The prefix alone identifies the failure.
func redactAuthRender(t string) string {
	if i := strings.Index(t, "has invalid value"); i >= 0 {
		return t[:i] + "has invalid value: <redacted>"
	}
	return t
}

// isHighConfidenceAuthRender reports whether the result is the CLI's OWN
// auth-failure render — prefix-anchored shapes and distinctive wire
// phrases — as opposed to prose that merely mentions logging in. Only
// these arm the credential-skip evidence: a classifier false positive
// used to cost one visibly-failed node, but an evidence write benches
// the credential fleet-wide for DefaultMaxAge, so the loose substring
// signatures ("not logged in" inside an agent's sentence) must not
// reach it.
func isHighConfidenceAuthRender(t string) bool {
	if strings.HasPrefix(t, "API Error: Header '") && strings.Contains(t, "has invalid value") {
		return true
	}
	low := strings.ToLower(t)
	// "Not logged in" (and a bare "Failed to authenticate") are
	// deliberately NOT here: they indict the DELIVERY, not the
	// credential — this package documents a healthy materialised forfait
	// rendered "Not logged in" by pod-env shadowing (claudeForfaitEnv,
	// live run 019f8a6c), and benching the credential for a delivery
	// fault would skip a key that works everywhere else. The dead-record
	// half of that render belongs to ingestion-time validation. Only the
	// provider's own verdict on the credential arms the skip:
	for _, sig := range []string{
		"invalid bearer token",
		"authentication_error",
		"invalid x-api-key",
		"oauth token has expired",
		"oauth token expired",
	} {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

// authFailureFast classifies a CLI result as a rendered authentication
// failure, records the refusal as meter evidence, and returns the typed
// error — nil when the result is not an auth render. The evidence write
// comes BEFORE the failure on purpose: re-resolution is already universal
// server-side, and without a reading the next resolution re-picks this
// same dead credential, gating the healthy tiers off behind it. The
// session's Source stamp keys the reading under the credential that
// actually ran. Fail-fast and evidence are two thresholds on purpose:
// every auth render fails the node legibly, but only the CLI's own
// high-confidence shapes bench the credential.
func authFailureFast(result *string, task Task) error {
	if result == nil || !isAuthErrorResult(*result) {
		return nil
	}
	t := strings.TrimSpace(*result)
	if isHighConfidenceAuthRender(t) && task.Hooks.OnUsageWindow != nil {
		_ = task.Hooks.OnUsageWindow(usagecap.Reading{
			Window:     usagecap.WindowAuth,
			Status:     usagecap.StatusRejected,
			ObservedAt: time.Now().UTC(),
		})
	}
	return &ErrAuthFailed{
		Provider: BackendClaudeCode,
		Detail:   fmt.Sprintf("check the forfait CLAUDE_CODE_OAUTH_TOKEN or the Anthropic API key: %s", redactAuthRender(t)),
	}
}

// retypeNetworkError re-classifies an opaque claude_code failure as an
// ErrTransient when the error message or captured stderr shows a transient-
// connectivity marker (fetch failed, ECONNRESET, overloaded, 5xx, …), so the
// executor retries it with backoff instead of failing the node on a blip.
// Already-typed transient / rate-limit errors pass through unchanged. Emits
// one explicit warn so the operator sees a connectivity issue, not just a
// generic retry.
func (b *ClaudeCodeBackend) retypeNetworkError(err error, stderr string, task Task) error {
	if err == nil {
		return nil
	}
	var t *ErrTransient
	var rl *ErrRateLimited
	if errors.As(err, &t) || errors.As(err, &rl) {
		return err
	}
	if !MatchesNetworkSignature(err.Error()) && !MatchesNetworkSignature(stderr) {
		return err
	}
	b.Logger.Warn("[%s#%d/claude-code] network connectivity issue detected; flagging for retry: %v",
		task.NodeID, task.Iteration, err)
	return &ErrTransient{Provider: BackendClaudeCode, Reason: "network", Detail: err.Error()}
}
