package delegate

import (
	"errors"
	"regexp"
	"strings"
)

// apiErrorResultStatusRe extracts the HTTP-ish status code the claude CLI
// prints when it renders an upstream API failure AS its result text, e.g.
// "API Error: 529 Overloaded" or "API Error: 503 {...}".
var apiErrorResultStatusRe = regexp.MustCompile(`(?i)^api error:?\s+(\d{3})\b`)

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
// error is longer and embedded) and must BEGIN with "api error". A parseable
// 4xx client/auth status returns false so it surfaces as the visible output
// (a retry won't fix a misconfig); 429/5xx/529 and bare connectivity markers
// return true.
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
		switch m[1] {
		case "408", "409", "425", "429", "500", "502", "503", "504", "529":
			return true
		default:
			return false // 4xx client/auth error — a retry won't fix it
		}
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
	// Real auth renders are short one-liners (like a rate-limit notice); the cap
	// keeps an agent discussing auth in a long answer from false-positiving.
	if t == "" || len(t) > 200 {
		return false
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
	} {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
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
