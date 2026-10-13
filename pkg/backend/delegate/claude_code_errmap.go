package delegate

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	// A 401/403 status is the provider's own verdict on the credential,
	// whatever prose (or none) follows it: a facade renders "API Error:
	// [401][Unauthorized][<id>]", which matches no signature below and would
	// otherwise flow on as the node's answer. Prefix-anchored by the regex,
	// short by the cap. 403 is in the class as the pi backend mints it from
	// the upstream status: a permission error is per-key and the pool's
	// rotation is the right first move; a facade block fails the next key
	// the same way and surfaces then, no worse than surfacing now.
	if m := apiErrorResultStatusRe.FindStringSubmatch(t); m != nil && (m[1] == "401" || m[1] == "403") {
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
//
// spawn names the credential channel the CLI that produced result was handed;
// it decides the remediation, never the evidence.
func authFailureFast(result *string, task Task, spawn credSpawn) error {
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
	// The remediation names the channel the spawn RAN on. The legacy wording
	// indicts the forfait and the Anthropic key, and a facade route cleared
	// both (facadeRouteEnv): on such a route it sends the operator re-arming
	// credentials that were never in the path. A route the engine bound to a
	// credential of the RUN keeps it — there the forfait and the key ARE what
	// it spends, and no variable of the operator's holds them.
	//
	// Every reading comes from the env the spawn was handed, never from the
	// `provider:` pin. The default precedence over the run's own keys and the
	// ZAI_API_KEY shortcut reach a facade with an EMPTY hint, so a hint-only
	// reading mis-names every unpinned facade route — and the pin has no state
	// left to answer for: a facade hint is either funded, which stamps the
	// slot (facadeRouteEnv), or refused before the spawn exists
	// (facadeHintRefusal at both spawn sites).
	//
	// The branches are the CLI's own auth precedence, top first:
	//
	//   - a cloud-provider switch at the value that enters its mode
	//     (cloudSwitches) outranks every token: the spawn spent a cloud
	//     account, and no variable the other readings describe was in the
	//     request.
	//   - the slot (FacadeSlotEnvKey) names the vendor whose key died, and
	//     facadeKeyFromEnv which of the two channels that fund such a route
	//     paid for it — the run's own key, or the provider's variable. Naming
	//     both would send the operator to the inert one half the time.
	//   - failing that, the operator's own ambient variables
	//     (ambientCredChannels) name themselves: that route carries no slot,
	//     since its base URL names no vendor. EVERY channel the spawn carried
	//     is named, not just the first — the CLI sends an API key and a bearer
	//     in one request, so the provider's verdict can indict any of them.
	//
	// Two facts ride whichever branch wins, because they are about a channel
	// NO branch owns: a cloud switch the CLI may or may not have read as on
	// (spawnCloudProviderSwitches), and the operator's own
	// ANTHROPIC_CUSTOM_HEADERS, whose lines the CLI applies over the route's
	// credential — including a route the engine bound, where nothing else in
	// the sentence would make the operator suspect their own variable. That
	// clause has two strengths, because the override is name-agnostic while
	// only one name is knowably a credential: an Authorization line is said to
	// replace the route's own, any other content is named as a variable in the
	// path and nothing is claimed about it (spawnCustomHeaders). Both stand
	// down where the ambient reading already names the variable among its
	// channels: there "check it" is the whole answer.
	//
	// What every branch owes, wherever the spawn carries a credential these
	// readings see: at least one channel the sentence names is in the spawn's
	// path. The legacy disjunction holds it by naming the two channels a route
	// the engine bound really spends — and, where it names them on an UNBOUND
	// route, the header clause is what keeps it true, that variable being the
	// only credential-bearing thing left in such a spawn.
	//
	// A spawn whose credential none of them can read keeps the disjunction as
	// the channel to ARM, not as one that ran: a desktop operator's forfait is
	// the file under $HOME/.claude, which puts no variable in the spawn at all,
	// and an expired one renders an auth failure the classifier matches.
	//
	// Known limit: a credential a sandbox CONTAINER holds by itself is in
	// none of these readings — the record sees the engine's map and, on a host
	// spawn, this process's env (spawnEnvValue) — and that spawn keeps the
	// legacy wording.
	lead := "check the forfait CLAUDE_CODE_OAUTH_TOKEN or the Anthropic API key"
	switch {
	case len(spawn.cloudSwitches) > 0:
		lead = fmt.Sprintf("no working credential on the %s route — that mode authenticates with the cloud account's own credentials, not with an Anthropic variable",
			strings.Join(spawn.cloudSwitches, ", "))
	case spawn.facadeSlot != "":
		channel := fmt.Sprintf("this run's %s key", spawn.facadeSlot)
		if spawn.facadeKeyFromEnv {
			channel = facadeEnvKey(spawn.facadeSlot)
		}
		lead = fmt.Sprintf("no working %s credential — check %s", spawn.facadeSlot, channel)
	case len(spawn.ambientChannels) == 1:
		lead = fmt.Sprintf("no working %s credential — check that variable", spawn.ambientChannels[0])
	case len(spawn.ambientChannels) > 1:
		lead = fmt.Sprintf("no working credential among the channels this spawn carried — check %s",
			strings.Join(spawn.ambientChannels, ", "))
	}
	if n := len(spawn.ambiguousCloudSwitches); n > 0 {
		verb, mode := "is", "that mode"
		if n > 1 {
			verb, mode = "are", "those modes"
		}
		// What such a switch costs depends on whether a mode was entered
		// anyway: with none on, a CLI reading presence left the token path for
		// a cloud account. With one on, claw's detectProvider takes the first
		// switch at cloudSwitchOn and a presence-reading CLI takes the first
		// one SET, so a cloud account was spent either way and the open
		// question is which — "instead" would be false there.
		consequence := "a CLI reading it as on spent a cloud account instead"
		if len(spawn.cloudSwitches) > 0 {
			consequence = "a CLI reading it as on may have spent a different cloud account"
		}
		lead += fmt.Sprintf(" — and %s %s set to a value other than %q, the one that enters %s, so %s",
			strings.Join(spawn.ambiguousCloudSwitches, ", "), verb, cloudSwitchOn, mode, consequence)
	}
	// One clause for the operator's own ANTHROPIC_CUSTOM_HEADERS, at the
	// strength of the fact: an Authorization line is a credential under the
	// name the CLI's own auth uses, while any other line is a credential only
	// the operator can recognise. Both stand down where the ambient reading
	// already names the variable among its channels — one name per sentence.
	switch {
	case spawn.authorizationHeader && !slices.Contains(spawn.ambientChannels, "ANTHROPIC_CUSTOM_HEADERS"):
		if len(spawn.cloudSwitches) > 0 {
			// A cloud-provider route carries the hedged form: claw's vertex
			// and foundry providers resolve the identity from the environment
			// and apply the line, its bedrock provider never reads the
			// variable at all, and which binary a tool node spawned is not
			// this record's fact.
			lead += " — and ANTHROPIC_CUSTOM_HEADERS carries an Authorization line, which a CLI that applies it on that route sends in place of the mode's own credential"
		} else {
			lead += " — and ANTHROPIC_CUSTOM_HEADERS carries an Authorization line, which the CLI applies over whatever credential this route resolved"
		}
	// The weak arm repeats the stand-down guard because the strong arm's own
	// guard falls through to this case: an Authorization line the ambient
	// reading already names satisfies spawn.customHeaders too, and without the
	// repetition the sentence would name the variable twice.
	//
	// One text for every route, the strong arm's two forms not being needed
	// here: this arm claims nothing about the route's own credential being
	// replaced, and the mechanism it does state is conditioned on a CLI
	// reading the variable — which covers the one reader known not to read it
	// on a cloud route (claw's bedrock provider).
	case spawn.customHeaders && !slices.Contains(spawn.ambientChannels, "ANTHROPIC_CUSTOM_HEADERS"):
		lead += " — and ANTHROPIC_CUSTOM_HEADERS is in this spawn's path, carrying lines a CLI that reads it applies over the headers its own auth set, so check it for a credential of its own"
	}
	return &ErrAuthFailed{
		Provider: BackendClaudeCode,
		Detail:   fmt.Sprintf("%s: %s", lead, redactAuthRender(t)),
	}
}

// retypeNetworkError re-classifies an opaque claude_code failure as an
// ErrTransient when the error message or captured stderr shows a transient-
// connectivity marker (fetch failed, ECONNRESET, …), so the executor retries
// it with backoff instead of failing the node on a blip. Already-typed
// transient / rate-limit errors pass through unchanged. Emits one explicit
// warn so the operator sees a connectivity issue, not just a generic retry.
//
// The two channels are scoped differently, like the shared CLI-agent retry
// loop: the process's own error text is matched against the FULL signature
// set, but stderr is free-form crash-dump text — a Node stack trace quotes
// arbitrary source, which can carry any signature verbatim (the #1645
// collision class: a Bun "JSON Parse error: Unexpected EOF" substring-
// matches "unexpected eof") — so only its transport-level lines count
// (matchesTransportStderr). The prose signatures (overloaded, 5xx bodies)
// stay load-bearing where the channel is structured: the result text
// (isTransientAPIErrorResult), which is where the CLI renders its own API
// failures — measured against a refused endpoint: "API Error: Connection
// refused … (ECONNREFUSED)" on the result, stderr empty.
func (b *ClaudeCodeBackend) retypeNetworkError(err error, stderr string, task Task) error {
	if err == nil {
		return nil
	}
	var t *ErrTransient
	var rl *ErrRateLimited
	if errors.As(err, &t) || errors.As(err, &rl) {
		return err
	}
	if !MatchesNetworkSignature(err.Error()) && !matchesTransportStderr(stderr) {
		return err
	}
	b.Logger.Warn("[%s#%d/claude-code] network connectivity issue detected; flagging for retry: %v",
		task.NodeID, task.Iteration, err)
	return &ErrTransient{Provider: BackendClaudeCode, Reason: "network", Detail: err.Error()}
}
