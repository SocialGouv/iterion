package forge

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
)

// hookRefusalMessageMax caps the forge's own message where an operator reads
// it: a refusal names a field and a reason in a line, and a forge (or the
// proxy in front of it) answering a page of text must not push the remedy
// out of sight.
const hookRefusalMessageMax = 300

// RefusalMessage is the forge's OWN reason for a non-2xx answer, as one
// capped line: the JSON `message` — a string, or the object GitLab answers
// field errors with (`{"message":{"url":["is blocked: …"]}}` → `url: is
// blocked: …`) — followed by GitHub's `errors[].message`, plus GitLab's
// `error`; anything else is the body as text. Empty when the answer carried
// no body.
//
// Object keys are read in sorted order: a map's iteration order must not
// decide what the operator reads.
func RefusalMessage(body []byte) string {
	if msg := jsonRefusalMessage(body); msg != "" {
		return oneCappedLine(msg)
	}
	return oneCappedLine(string(body))
}

// jsonRefusalMessage reads the refusal keys the three forges answer with,
// empty when the body is not a JSON object or names none of them.
func jsonRefusalMessage(body []byte) string {
	var answer struct {
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return ""
	}
	var parts []string
	for _, raw := range []json.RawMessage{answer.Message, answer.Error} {
		if m := flattenRefusalValue(raw); m != "" {
			parts = append(parts, m)
		}
	}
	for _, e := range answer.Errors {
		if m := strings.TrimSpace(e.Message); m != "" {
			parts = append(parts, m)
		}
	}
	return strings.Join(parts, "; ")
}

// flattenRefusalValue renders a refusal value whatever shape the forge gave
// it: a string, a list of strings, or the field→reasons object GitLab
// answers a rejected hook field with.
func flattenRefusalValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var list []any
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(scalarStrings(list), ", ")
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		var reasons []string
		switch v := obj[k].(type) {
		case string:
			reasons = scalarStrings([]any{v})
		case []any:
			reasons = scalarStrings(v)
		}
		if len(reasons) == 0 {
			continue
		}
		parts = append(parts, k+": "+strings.Join(reasons, ", "))
	}
	return strings.Join(parts, "; ")
}

// scalarStrings keeps the string items of a JSON array, trimmed: a reason
// list is what the forges put there, and a nested structure is not a message.
func scalarStrings(items []any) []string {
	var out []string
	for _, it := range items {
		if s, ok := it.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// cappedLineEllipsis marks a message oneCappedLine truncated.
const cappedLineEllipsis = "…"

// oneCappedLine collapses every whitespace run to a single space and caps
// the result: the message lands inside an error string and an HTTP detail,
// both read as one line.
func oneCappedLine(s string) string {
	line := strings.Join(strings.Fields(s), " ")
	if r := []rune(line); len(r) > hookRefusalMessageMax {
		return string(r[:hookRefusalMessageMax]) + cappedLineEllipsis
	}
	return line
}

// HookWriteErr types a non-2xx answer to a hook create/update, carrying the
// forge's own message: the remedy for a refused hook is a setting, and WHICH
// setting depends on what the forge said — "Invalid url given" sends the
// operator to the outbound allowlist, "Maximum number of project hooks (100)
// exceeded" to the project's hooks. The status mapping is StatusErr's, so
// 401/403/404 keep the sentinels every caller classifies on (they carry no
// message of their own).
func HookWriteErr(provider, op string, code int, body []byte) error {
	err := StatusErr(provider, op, code)
	var se *StatusError
	if errors.As(err, &se) && se.Detail == "" {
		se.Detail = RefusalMessage(body)
	}
	return err
}

// hookSecretRedaction stands in for the webhook secret wherever a forge's own
// answer carried it back. It keeps the iwh_ prefix so a reader knows what was
// removed, and is not a token.
const hookSecretRedaction = "iwh_…"

// hookSecretPartialMin is the shortest run of the secret's own bytes worth
// removing from the end of a message: a secret that straddles the cap of
// oneCappedLine survives there as a prefix of itself.
const hookSecretPartialMin = 8

// scrubbedError carries a message the secret was removed from, over the error
// chain it was removed from — which still classifies, since errors.Is and
// errors.As walk straight through it.
//
// It exists because fmt.Errorf("…: %w", err) RENDERS its message when it is
// built: a wrapper constructed before the scrub has already copied the forge's
// message into its own text, and rewriting the typed value underneath cannot
// reach that copy. Only a new message can.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }

// Unwrap exposes the forge's own failure, so every caller that classified on
// its status or sentinel keeps doing so.
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubHookSecret removes the webhook secret a hook write put on the wire
// from the forge's own message, and answers an error that no longer carries
// it. A forge — or the WAF/proxy in front of a self-managed one — that quotes
// the request it refused hands the live secret back, and that message travels
// on to an operator-visible HTTP detail and a Warn line: only here is it
// still known which bytes are the secret.
//
// Both readings of the error are covered: *StatusError.Detail — the message's
// typed carrier (RefusalMessage of a refusal body, or the forge's rate-limit
// message), rewritten IN PLACE so the type and the HTTP detail built from it
// survive — and the rendered text of the chain, which an enclosing wrapper
// may already hold a copy of.
//
// A minted secret (webhooks.MintToken: "iwh_" + base64url) is URL- and
// JSON-safe, so the bytes as sent are the only spelling a forge can echo
// (TestMintedWebhookSecretNeedsNoEscaping holds that).
func scrubHookSecret(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	var se *StatusError
	if errors.As(err, &se) && se.Detail != "" {
		se.Detail = scrubSecretFromLine(se.Detail, secret)
	}
	var redirect *RedirectError
	if errors.As(err, &redirect) {
		redirect.Location = scrubSecretFromLine(redirect.Location, secret)
	}
	if text := err.Error(); scrubSecretFromLine(text, secret) != text {
		return &scrubbedError{msg: scrubSecretFromLine(text, secret), err: err}
	}
	return err
}

// scrubSecretFromLine replaces the secret with the redaction marker, whole or
// truncated: oneCappedLine caps a long message, and a secret the cap cuts
// through survives there as its own first bytes followed by the cap's
// ellipsis. A run shorter than hookSecretPartialMin is left alone — it is not
// credential material, and eating it would take the forge's words with it.
func scrubSecretFromLine(line, secret string) string {
	if secret == "" {
		return line
	}
	line = strings.ReplaceAll(line, secret, hookSecretRedaction)
	for n := len(secret) - 1; n >= hookSecretPartialMin; n-- {
		if cut := secret[:n] + cappedLineEllipsis; strings.Contains(line, cut) {
			return strings.ReplaceAll(line, cut, hookSecretRedaction)
		}
	}
	return line
}

// HookRefusedError is the forge refusing the ADDRESS of the webhook, not the
// grant behind the call: a 422 answering the hook create/update during
// provisioning. GitLab answers exactly this ("Invalid url given") for a URL
// outside its outbound allowlist, and no retry and no broader token changes
// it — the remedy is a hook base the forge accepts, which is an operator
// gesture on the connection (or on the forge). Typed so the route can name
// that remedy instead of echoing an HTTP code.
//
// Pinned is the connection's own base when it pins one, empty when the hook
// was built from the deployment's public URL: the two cases have different
// remedies (pin a base vs. check the pinned host), and the caller cannot tell
// them apart from the URL alone.
type HookRefusedError struct {
	Provider Provider
	// ConnectionID is the connection the hook was built for — the one whose
	// pin is the remedy.
	ConnectionID string
	// URL is the hook address the forge refused, verbatim.
	URL string
	// Pinned is the connection's pinned hook base, empty when unpinned.
	Pinned string
	// Detail is the forge's own message for the refusal, one capped line
	// (RefusalMessage), empty when the answer carried none. The remedy the
	// route states depends on it: a 422 is also how a forge refuses a hook
	// for a reason that has nothing to do with the address.
	Detail string
	// Err is the forge's own refusal, kept whole so errors.As still reaches
	// the *StatusError carrying the 422.
	Err error
}

func (e *HookRefusedError) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(string(e.Provider))
		b.WriteString(": ")
	}
	b.WriteString("refused the webhook URL ")
	b.WriteString(e.URL)
	if e.Pinned != "" {
		b.WriteString(" (pinned base ")
		b.WriteString(e.Pinned)
		b.WriteString(")")
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// Unwrap exposes the forge's refusal, so every caller that classified on the
// status keeps doing so.
func (e *HookRefusedError) Unwrap() error { return e.Err }

// hookRefusal types a hook upsert failure the forge answered 422 to a hook
// CREATE or UPDATE — the one failure whose cause is the URL itself. Anything
// else passes through unchanged: a 403 is a grant, a 5xx is an outage, a 422
// answered to the hook listing is a project that has too many hooks, and
// dressing any of them as a refused address would send the operator to the
// wrong setting.
func hookRefusal(conn Connection, hookURL string, err error) error {
	var se *StatusError
	if err == nil || !errors.As(err, &se) || se.Code != http.StatusUnprocessableEntity {
		return err
	}
	// A READ never offered the forge an address, so no answer to one refuses
	// it: a hook listing that answers 422 (GitLab does, for a project at its
	// hook ceiling) names a limit, not this URL.
	var read *hookReadError
	if errors.As(err, &read) {
		return err
	}
	// Nor did a hook request that was never sent: a 422 there answered the
	// credential step before it.
	var notSent *HookNotSentError
	if errors.As(err, &notSent) {
		return err
	}
	return &HookRefusedError{
		Provider:     conn.Provider,
		ConnectionID: conn.ID,
		URL:          hookURL,
		Pinned:       strings.TrimRight(conn.WebhookBaseURL, "/"),
		Detail:       se.Detail,
		Err:          err,
	}
}

// ConfigRestoreError is a re-provision that could not put the previous webhook
// config back after the forge hook upsert failed: the stored config carries the
// fresh secret while the forge still signs with the old one, so every delivery
// to the repo fails until the same request is re-run. Typed so the route
// reports it ahead of the hook failure it is joined with — a refused address
// or a forge outage would otherwise be the only thing the operator reads.
type ConfigRestoreError struct {
	WebhookID string
	Err       error
}

func (e *ConfigRestoreError) Error() string {
	return fmt.Sprintf("forge: restore webhook config %s after the hook upsert failed: %v — previous settings could not be restored; the webhook keeps its immutable secret; re-run the same request", e.WebhookID, e.Err)
}

// Unwrap exposes the store's own failure.
func (e *ConfigRestoreError) Unwrap() error { return e.Err }

// HookWriteIndeterminateError is a hook write whose outcome the forge never
// stated: a transport error, timeout, cancellation, redirect or 5xx. Durable
// identity and attempted settings stay available for the operator's retry.
//
// Typed so the answer says that instead of implying a settled failure, and
// so the error still unwraps to the forge's own (a 5xx stays a 502).
type HookWriteIndeterminateError struct {
	WebhookID string
	Err       error
}

func (e *HookWriteIndeterminateError) Error() string {
	return fmt.Sprintf("forge: the webhook %s hook write failed without the forge saying whether it applied it: %v — the stored config and durable reservation keep this webhook identity; re-run the same request, provisioning converges", e.WebhookID, e.Err)
}

// Unwrap exposes the forge's own failure, so every caller that classified on
// its status or sentinel keeps doing so.
func (e *HookWriteIndeterminateError) Unwrap() error { return e.Err }

// hookReadError is a forge READ that failed on the way to a hook write: the
// probe of the delivery URL (upsertHook). A read registers nothing on the
// forge, so a step that ends on one applied no write — whatever the read
// answered, a 502 and a timeout included. Typed at the call site, the only
// frame that knows the failing call was a GET, so the undo classifies it as
// what it is instead of reading the status alone.
//
// priorWrite carries the write attempted before the read in the same step,
// with the failure it ended on, and is nil when there was none: a read cannot
// settle what a write before it left open, so the step is only "nothing
// applied" when that write was settled too.
type hookReadError struct {
	op         string
	err        error
	priorWrite error
}

func (e *hookReadError) Error() string { return fmt.Sprintf("forge: %s: %v", e.op, e.err) }

// Unwrap exposes the forge's own failure, so every caller that classified on
// its status or sentinel keeps doing so.
func (e *hookReadError) Unwrap() error { return e.err }

// statusRequestTimeout and statusClientClosedRequest are the two 4xx a
// GATEWAY answers for a request it may already have forwarded: 408, and
// nginx's non-standard 499 for a client that went away. They are the only
// 4xx that do not prove the forge refused the write.
const (
	statusRequestTimeout      = http.StatusRequestTimeout
	statusClientClosedRequest = 499
)

// HookNotSentError is a hook create or update that failed BEFORE its request
// was sent: the step that prepares it — minting the installation token a
// GitHub App connection writes with — failed, so the forge's hook is the one
// it had. Typed by the client that knows the failing call was that step; the
// status the step answered (a 502 from the token endpoint) says nothing about
// the hook.
type HookNotSentError struct {
	// Step names the call that failed before the hook request.
	Step string
	Err  error
}

func (e *HookNotSentError) Error() string { return "forge: " + e.Step + ": " + e.Err.Error() }

// Unwrap exposes the step's own failure.
func (e *HookNotSentError) Unwrap() error { return e.Err }

// forgeRejectedTheWrite reports a forge call the forge provably did not
// apply. That is the whole condition under which a re-provision may put its
// previous webhook config back — the forge-side hook is untouched, so the
// secret it signs with is the previous one.
//
// Four shapes prove it. The request was never SENT: the step that prepares
// it — a GitHub App's installation-token mint — failed (HookNotSentError),
// whatever that step answered. The step never WROTE: it ended on a read, whose
// failure says nothing about the forge's hook because a read cannot change it
// (hookReadError) — and the write attempted before that read, when there was
// one, was itself settled. Or the forge ANSWERED a refusal: the sentinels the
// transport maps 401/403/404 to (ErrUnauthorized, ErrForbidden,
// ErrHookNotFound / *NotFoundError), or a *StatusError in the 4xx range (the
// 422 of a refused address among them) other than the gateway timeouts above.
// Or the request never LEFT: a name that does not resolve, a dial that was
// refused, a server certificate that does not verify — in all three the
// connection the write would have travelled on was never established.
//
// Everything else — a timeout or a reset mid-flight, a cancellation, a 5xx of
// the hook request itself — says nothing about what the forge did with it.
func forgeRejectedTheWrite(err error) bool {
	if err == nil {
		return false
	}
	// A hook request the client never sent — the step before it (minting the
	// credential it is sent with) failed — left the forge's hook untouched,
	// whatever status that step answered.
	var notSent *HookNotSentError
	if errors.As(err, &notSent) {
		return true
	}
	// Asked before the status, since the status of a READ is not the status of
	// a write: a probe answered 502 proves the hook is untouched exactly as a
	// 422 does.
	var read *hookReadError
	if errors.As(err, &read) {
		return read.priorWrite == nil || forgeRejectedTheWrite(read.priorWrite)
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 400 && se.Code < 500 &&
			se.Code != statusRequestTimeout && se.Code != statusClientClosedRequest
	}
	if requestNeverLeft(err) {
		return true
	}
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound)
}

// requestNeverLeft reports a transport failure that happened BEFORE any byte
// of the request could reach the forge: DNS resolution, the dial itself, or
// the TLS handshake's certificate verification. A failure of any of the three
// means no connection existed to write on, so the forge's hook is the one it
// had — the case a self-managed forge behind an expired certificate, a
// firewall change or a DNS outage puts a deployment in for hours, where
// keeping the fresh config would break a repo that works.
func requestNeverLeft(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return true
	}
	var authority x509.UnknownAuthorityError
	return errors.As(err, &authority)
}
