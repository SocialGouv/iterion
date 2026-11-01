package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
)

// ContextOverflowError indicates the prompt exceeded the model's context window.
type ContextOverflowError struct {
	Message string
}

func (e *ContextOverflowError) Error() string {
	return e.Message
}

// APIError represents a non-overflow API error.
type APIError struct {
	Message     string
	StatusCode  int
	IsRetryable bool
}

func (e *APIError) Error() string {
	return e.Message
}

// ClassifyStreamError parses a stream error event and returns the appropriate
// typed error (*ContextOverflowError or *APIError), or nil if the data is not
// a recognized error event.
func ClassifyStreamError(body []byte) error {
	var obj struct {
		Type  string `json:"type"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	if obj.Type != "error" {
		return nil
	}

	switch obj.Error.Code {
	case "context_length_exceeded":
		return &ContextOverflowError{Message: "Input exceeds context window of this model"}
	case "insufficient_quota":
		return &APIError{Message: "Quota exceeded. Check your plan and billing details."}
	case "usage_not_included":
		return &APIError{Message: "To use Codex with your ChatGPT plan, upgrade to Plus."}
	case "invalid_prompt":
		msg := "Invalid prompt."
		if obj.Error.Message != "" {
			msg = obj.Error.Message
		}
		return &APIError{Message: msg}
	}

	return nil
}

// streamTransportMarkers identify a stream `error` event that stems from a
// truncated / partially-read stream — the protocol-shape failures the
// generic network-signature list (delegate.MatchesNetworkSignature) does
// NOT cover. Genuine network/transport signatures (connection reset, EOF,
// timeout, 5xx, …) are matched via that shared list so the two never
// drift; these are only the stream-reader-specific additions
// (claw-code-go surfaces them as "read stream: …", "openai stream
// read: …", "… truncated …", "parse SSE: …", and its idle watchdog as
// "openai stream stalled: …"). "stream stalled" stays in the list and
// stays reachable: clawStreamStall takes only the reports that also carry
// the watchdog's window or its knob in the watchdog's own wording, so a
// message that merely quotes the phrase keeps the retryable transport
// verdict it earns here.
var streamTransportMarkers = []string{
	"read stream", "stream read", "parse sse", "truncat", "incomplete",
	"stream stalled",
}

// clawStallMarker is the phrase a claw idle watchdog puts in the stream
// `error` event it sends when a wire falls silent: "<wire> stream stalled:
// no data for <window> — aborting (retryable; tune via
// CLAW_STREAM_IDLE_TIMEOUT)" (sseutil.IdleWatchdog, wired on every SSE
// reader openaiwire.StreamEvents serves — the OpenAI chat and foundry
// wires — and on the Responses reader). Any wire claw gives that watchdog
// reports in the same shape, so the class is keyed on that wording — the
// phrase and one of the two markers below — rather than on the wire names.
const clawStallMarker = "stream stalled"

// clawStallWindowPrefix precedes the idle window a stall report names, and
// clawStallTunePrefix the knob it points the operator at. Each is anchored
// to the report's own wording — the colon after the phrase, the parenthesis
// before "retryable" — so prose quoting the words outside that shape ("we
// have no data for the requested range", "stalled, tune via the dashboard")
// stays outside the class. Either is the SECOND marker a watchdog report
// carries and a relayed provider message does not: both emit sites write
// both (openaiwire/stream.go for the chat and foundry wires it serves,
// openai/responses.go for the Responses reader), from the one wording
// sseutil.IdleWatchdog's callers give it. The knob's own name is
// deliberately not spelled here — iterion never reads that variable, and a
// constant spelling it would claim a sandbox classification it does not
// have.
const (
	clawStallWindowPrefix = "stream stalled: no data for "
	clawStallTunePrefix   = "(retryable; tune via "
)

// clawStreamStall types a stall a provider wire's own watchdog reported as
// the *StreamIdleError iterion's watchdog raises for the same condition — a
// provider stream gone silent. Both verdicts are then read through
// errors.As by the inner retry loop (isRetryable, and the larger transient
// attempt budget) and by the outer delegate loop (isDelegateRetryable),
// which reads no *APIError at all. The message rides along verbatim so the
// operator keeps the wire's own report.
//
// Two markers are required, not one, because branch order alone does not
// protect a provider's verdict here. The provider-verdict branches of
// classifyStreamEventError run first, but they recognise a verdict only on
// the wires that preserve its type and code; claw's generic SSE client
// forwards a provider's `error.message` verbatim and drops both
// (internal/api/client.go), so on the three providers that build it —
// anthropic, moonshot, zai — this is the first branch that can match such
// a refusal at all.
// Requiring the window the watchdog measured or the knob it names — in the
// report's own wording, anchored around the phrase — keys the class on the
// watchdog's report rather than on a phrase a provider can relay: a refusal
// that merely quotes "stream stalled", even alongside the window's or the
// knob's bare words, falls through to the transport markers below and keeps
// its standard budget.
func clawStreamStall(msg string) (*StreamIdleError, bool) {
	low := strings.ToLower(msg)
	if !strings.Contains(low, clawStallMarker) {
		return nil, false
	}
	if !strings.Contains(low, clawStallWindowPrefix) && !strings.Contains(low, clawStallTunePrefix) {
		return nil, false
	}
	return &StreamIdleError{Phase: StreamIdleWire, Idle: clawStallWindow(msg), Reported: msg}, true
}

// clawStallWindow reads the idle window a stall message names. A report
// that names the knob but no window leaves the window unknown rather than
// dropping the stall verdict: the typed fact is what the retry loops read,
// the window only describes it.
//
// The token is read out of the LOWERED text, where the position was found:
// ToLower is not length-preserving (İ, two bytes, lowercases to i, one), so
// the same index in the original lands earlier than the token — inside the
// preceding word once the prose has shrunk by more than the space before
// it. Go's duration units are lowercase, so reading the lowered form costs
// nothing.
func clawStallWindow(msg string) time.Duration {
	low := strings.ToLower(msg)
	at := strings.Index(low, clawStallWindowPrefix)
	if at < 0 {
		return 0
	}
	fields := strings.Fields(low[at+len(clawStallWindowPrefix):])
	if len(fields) == 0 {
		return 0
	}
	window, err := time.ParseDuration(fields[0])
	if err != nil {
		return 0
	}
	return window
}

// classifyStreamEventError turns a stream `error` event's message into a
// typed error. A recognised provider error (quota, context overflow,
// invalid prompt) keeps its permanent classification via
// ClassifyStreamError. A silence the wire's own idle watchdog reported
// becomes the same *StreamIdleError iterion's watchdog raises
// (clawStreamStall). Any other transport / truncation failure — matched
// either by the shared network-signature list or a stream-reader-specific
// marker — is wrapped as a RETRYABLE *APIError so the retry loop re-issues
// the request instead of surfacing a half-response as if it were complete.
// Anything else stays a plain, non-retryable stream error.
func classifyStreamEventError(msg string) error {
	if classified := ClassifyStreamError([]byte(msg)); classified != nil {
		return classified
	}
	if d, ok := openAIStreamErrorDetail(msg); ok {
		// A code a JSON error frame is classified by keeps that
		// classification in this textual form (context overflow, quota…).
		if d.code != "" {
			frame, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"code": d.code, "message": d.message}})
			if classified := ClassifyStreamError(frame); classified != nil {
				return classified
			}
		}
		// The failure came after the provider accepted the request — a
		// chat-wire error frame, most often an upstream failure a gateway
		// relays, or a failed Responses API response — so it is retried
		// unless it names a condition no new request clears: a wrong retry
		// costs a bounded attempt, a wrong refusal the run. OpenAI's own
		// Codex client applies the same rule to a failed response's code.
		// A failure that names nothing — the bare forms — is retried like
		// any other. The verdict is typed, so isRetryable reads it as is.
		status, _ := statusOf(d.code)
		return &APIError{Message: "stream error: " + msg, StatusCode: status, IsRetryable: !permanentProviderError(d.typ, d.code)}
	}
	if stall, ok := clawStreamStall(msg); ok {
		return stall
	}
	if delegate.MatchesNetworkSignature(msg) || matchesStreamTransportMarker(msg) {
		return &APIError{Message: "stream error: " + msg, IsRetryable: true}
	}
	return fmt.Errorf("stream error: %s", msg)
}

// openAIStreamError is what an error event from claw's OpenAI provider says
// about the provider's own verdict. Types and codes are lowercased.
type openAIStreamError struct {
	message, typ, code string
}

// openAIStreamErrorDetail reads an error event raised by claw's OpenAI
// provider. The chat wire reports an error frame as
// "openai stream error: <message> (type=<type>, code=<code>)" — type and
// code optional, the frame's raw JSON instead when it carries neither a
// message nor a type — and a data line it could not parse as
// "openai stream error: unparseable data frame: …". The Responses API
// reports "openai stream error: <code>: <message>",
// "openai response failed: <code>: <message>", or either bare. The two
// forms share a prefix, so a chat message that leads with a token
// ("RuntimeError: …") reads as a code too; either way only a code named
// permanent decides a refusal. ok is false for any other message.
func openAIStreamErrorDetail(msg string) (openAIStreamError, bool) {
	var rest string
	switch {
	case msg == "openai stream error", msg == "openai response failed":
		return openAIStreamError{}, true
	case strings.HasPrefix(msg, "openai stream error: "):
		rest = strings.TrimPrefix(msg, "openai stream error: ")
	case strings.HasPrefix(msg, "openai response failed: "):
		return leadingCode(strings.TrimPrefix(msg, "openai response failed: ")), true
	default:
		return openAIStreamError{}, false
	}
	if t, c, m, isJSON := errorFrameFields(rest); isJSON {
		return openAIStreamError{message: m, typ: t, code: c}, true
	}
	if i := strings.LastIndex(rest, " ("); i >= 0 && strings.HasSuffix(rest, ")") {
		d := openAIStreamError{message: rest[:i]}
		for _, part := range strings.Split(rest[i+2:len(rest)-1], ", ") {
			if v, found := strings.CutPrefix(part, "type="); found {
				d.typ = strings.ToLower(v)
			} else if v, found := strings.CutPrefix(part, "code="); found {
				d.code = strings.ToLower(v)
			}
		}
		if d.typ != "" || d.code != "" {
			return d, true
		}
	}
	return leadingCode(rest), true
}

// leadingCode reads the Responses form: a bare code token, alone or before
// ": <message>". A message holding a colon after words ("Invalid schema:
// …") has a space before it, so it names no code.
func leadingCode(rest string) openAIStreamError {
	if head, tail, found := strings.Cut(rest, ": "); found && isErrorCodeToken(head) {
		return openAIStreamError{message: tail, code: strings.ToLower(head)}
	}
	if isErrorCodeToken(rest) {
		return openAIStreamError{code: strings.ToLower(rest)}
	}
	return openAIStreamError{message: rest}
}

// errorFrameFields decodes an error frame claw passed on as raw JSON — the
// frame carried neither a message nor a type — reading its code (a string
// or a number), type and message, one "error" level down when nested.
func errorFrameFields(raw string) (typ, code, message string, ok bool) {
	var frame map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &frame) != nil {
		return "", "", "", false
	}
	if inner, nested := frame["error"]; nested {
		var innerFrame map[string]json.RawMessage
		if json.Unmarshal(inner, &innerFrame) == nil && len(innerFrame) > 0 {
			frame = innerFrame
		}
	}
	scalar := func(key string) string {
		v := strings.TrimSpace(string(frame[key]))
		if v == "" || v == "null" {
			return ""
		}
		var s string
		if json.Unmarshal(frame[key], &s) == nil {
			return s
		}
		return v // a number
	}
	return strings.ToLower(scalar("type")), strings.ToLower(scalar("code")), scalar("message"), true
}

// isErrorCodeToken reports whether s spells a provider error code
// ("server_error", "RATE_LIMIT_EXCEEDED", "503"): letters, digits and
// underscores.
func isErrorCodeToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// statusOf reads a provider error code that is an HTTP status ("503", 429).
func statusOf(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 100 && n <= 599
}

// permanentProviderError reports whether an error's type or code names a
// condition no new request clears: a malformed or refused request, missing
// rights, an exhausted balance, a policy or content refusal, an unusable
// image, an input a validator rejected (Hugging Face TGI's `validation`
// error_type), or a 4xx status the HTTP path does not retry either (claw's
// IsRetryableStatus: 408, 409, 429 and 5xx are retried). A permanent type
// counts in either field — Ollama's Responses failures carry it as their
// code. Two labels are no verdict on their own: invalid_request_error beside
// a status the HTTP path retries (LiteLLM labels every unmapped 4xx with it,
// its 408 timeouts included), and vLLM's BadRequestError 400, its catch-all
// for an exception raised mid-stream whatever failed.
func permanentProviderError(typ, code string) bool {
	status, isStatus := statusOf(code)
	retryableStatus := isStatus && api.IsRetryableStatus(status)
	for _, label := range []string{typ, code} {
		switch label {
		case "invalid_request_error":
			if !retryableStatus {
				return true
			}
		case "authentication_error", "permission_error", "not_found_error", "billing_error", "insufficient_quota", "validation":
			return true
		}
	}
	if permanentProviderCodes[code] {
		return true
	}
	if typ == "badrequesterror" && code == "400" {
		return false
	}
	return isStatus && status >= 400 && !retryableStatus
}

// permanentProviderCodes are provider error codes no new request clears:
// the Responses API's documented refusals (openai-python ResponseError.code)
// and the terminal codes OpenAI's Codex client maps to a dedicated error.
// The codes ClassifyStreamError types (context overflow, quota,
// usage_not_included, invalid_prompt) are decided there.
var permanentProviderCodes = map[string]bool{
	"content_filter": true, "invalid_api_key": true, "model_not_found": true,
	"data_residency_mismatch": true, "bio_policy": true, "misalignment_policy_violation": true,
	"cyber_policy": true, "credit_balance_exhausted": true,
	"organization_spend_limit_exceeded": true, "project_spend_limit_exceeded": true,
	"invalid_image": true, "invalid_image_format": true, "invalid_base64_image": true,
	"invalid_image_url": true, "image_too_large": true, "image_too_small": true,
	"image_parse_error": true, "image_content_policy_violation": true, "invalid_image_mode": true,
	"image_file_too_large": true, "unsupported_image_media_type": true, "empty_image_file": true,
	"image_file_not_found": true,
}

// admissionRefused reports a stream error event the provider answered
// with instead of serving the request: either a refusal named at
// admission (a rate limit, an overload, an exhausted balance), or any
// permanent condition no new request clears (context overflow, bad key,
// unknown model) — billed nothing on every wire that names its verdict.
// A refusal named only in prose is not recognised.
func admissionRefused(msg string) bool {
	d, ok := streamErrorDetail(msg)
	if !ok {
		return false
	}
	if admissionRefusalLabels[d.typ] || admissionRefusalLabels[d.code] {
		return true
	}
	if permanentProviderError(d.typ, d.code) {
		return true
	}
	status, _ := statusOf(d.code)
	return status == 429 || status == 529
}

// streamErrorDetail parses a stream error event into the verdict its
// provider named: the chat/Responses textual forms first, else a raw JSON
// error frame passed through verbatim.
func streamErrorDetail(msg string) (openAIStreamError, bool) {
	if d, ok := openAIStreamErrorDetail(msg); ok {
		return d, true
	}
	typ, code, _, isJSON := errorFrameFields(msg)
	if !isJSON {
		return openAIStreamError{}, false
	}
	return openAIStreamError{typ: typ, code: code}, true
}

// admissionRefusalLabels are the error types and codes providers give a
// request they refuse to serve: OpenAI's and Anthropic's rate-limit,
// overload and balance refusals, and the Responses API's.
var admissionRefusalLabels = map[string]bool{
	"rate_limit_exceeded": true, "rate_limit_error": true, "overloaded": true,
	"overloaded_error": true, "server_is_overloaded": true, "slow_down": true,
	"insufficient_quota": true, "credit_balance_exhausted": true, "billing_error": true,
	"organization_spend_limit_exceeded": true, "project_spend_limit_exceeded": true,
	"usage_not_included": true,
	// Over-window and oversized-prompt refusals: the request is rejected
	// before processing, so nothing is billed.
	"context_length_exceeded": true, "prompt_too_long": true,
	"request_too_large": true, "string_above_max_length": true,
}

func matchesStreamTransportMarker(msg string) bool {
	return strutil.ContainsAnyFold(msg, streamTransportMarkers)
}
