// Package exec turns a declared operation into an HTTP call and its answer
// into a typed result. It is the deterministic promise made executable: the
// operation, its arguments and the reading of the response are all decided by
// data, and no model is involved at any point.
//
// # What it refuses to do
//
// Three refusals shape the package, and each one is a failure mode that a
// more accommodating executor produces silently.
//
// It does not GUESS. A parameter the operation does not declare is an error,
// not a value dropped on the floor: a `.bot` that misspells `channel` as
// `chanel` must be told, because the alternative is a call that succeeds
// against the vendor while doing something other than what the workflow said.
//
// It does not RETRY BLIND. A mutating operation whose answer was lost, and
// which the vendor gives no idempotency key for, reports `unknown_outcome` —
// a first-class result, not an absence. Repeating the call might duplicate a
// comment, a payment, a release; reporting success would be a lie. Saying "I
// cannot tell" is the only honest third option.
//
// It does not read SUCCESS FROM THE STATUS ALONE where the vendor does not
// put it there. Slack's entire Web API answers 200 with `{"ok": false}`, so a
// status-only reader would checkpoint failures as successes on a pilot
// service.
package exec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// Credential is the material a connection resolved, ready to be placed on a
// request by whichever auth scheme the operation runs under.
//
// It is a value rather than an interface because placement is the PACKAGE's
// knowledge (a header, a query parameter, a prefix), not the credential's. A
// connection that decided its own placement could contradict the scheme the
// package declares, and the two would drift.
type Credential struct {
	// SchemeID names the auth scheme this credential satisfies. It is checked
	// against the operation's requirements before the call, so a connection
	// bound to the wrong scheme fails locally rather than as a vendor 403.
	SchemeID string
	// Value is the token / api key / bearer.
	Value string
	// Username and Password serve AuthBasic.
	Username string
	Password string
	// Scopes is what the provider actually granted, when it says. Empty means
	// unknown, which is not the same as none: an operation's scope
	// requirements are only enforced when the grant is known, since refusing
	// on an unknown grant would make every PAT-backed connection unusable.
	Scopes []string
}

// Executor performs one operation's call.
type Executor struct {
	// Client MUST be the guarded client (httpdial.SafeClient): a connector
	// calls hosts a tenant chose, so an unguarded one is a way to reach a
	// metadata endpoint from inside the deployment. It is a field rather than
	// a package default so a test can inject a stub, and nil is refused.
	Client *http.Client
	// BaseURL is the connection's instance origin, overriding the package's
	// default. Empty uses the package's.
	BaseURL string
	// UserAgent identifies iterion to the vendor. Empty sends the default.
	UserAgent string
	// Now is injectable so a test can pin a Retry-After deadline.
	Now func() time.Time
}

// Result is one call's outcome.
type Result struct {
	// Status is the HTTP status the vendor answered, 0 when nothing arrived.
	Status int
	// Data is the decoded JSON body, or nil for an empty one.
	Data any
	// Pending is true when the vendor accepted the work without finishing it
	// (a 202 the package declared). A workflow that treats it as completion
	// acts on work that has not happened, so it is surfaced rather than
	// folded into success.
	Pending bool
	// Requests is how many HTTP requests produced this result: 1 for a single
	// call, N for a walk.
	//
	// It exists because a paginated action can spend twenty of a vendor's
	// rate-limit slots behind one node, and nothing said so — the node
	// reported "a call" and the operator discovered the cost on the vendor's
	// dashboard. It is the smallest honest half of a per-request accounting
	// contract: it does not price anything, but it stops a walk being
	// invisible.
	Requests int
	// Bytes is how many raw response bytes produced this result — the last
	// page's for a single call, the walk's TOTAL for a paginated one, by the
	// same argument as Requests.
	//
	// It is what the walk's own budget is measured in: a page is bounded (the
	// 32 MiB LimitReader) but a WALK was not, and the items of every page are
	// held until the whole call returns.
	Bytes int
	// Err is the typed failure, nil on success. It is a FIELD rather than a
	// returned error because a business failure is a result a workflow
	// branches on — `not_found` is an answer — while a returned error means
	// iterion itself could not perform the call.
	Err *Error
}

// OK reports whether the call succeeded.
func (r Result) OK() bool { return r.Err == nil }

// Error is a typed failure, in iterion's closed vocabulary plus whatever the
// vendor said about it.
type Error struct {
	Class spec.ErrorClass
	// Status is the HTTP status, 0 for a transport failure.
	Status int
	// Code and Message are the vendor's own, when the outcome policy says
	// where to find them.
	Code    string
	Message string
	// RetryAfter is the delay a 429 named, zero when it named none.
	RetryAfter time.Duration
	// Ambiguous marks a failure that leaves a MUTATION undecided: the request
	// was dispatched, the vendor may have performed it, and nothing available
	// says whether it did.
	//
	// It is a FIELD rather than a property of the class, because the class
	// alone cannot answer it. A 500 on a POST is `upstream` like any other 500,
	// yet the write may well have committed before the server failed; a 400 on
	// the same POST is a refusal and nothing happened. Only the place that
	// holds the status AND the operation's effect can tell them apart, so that
	// is where it is decided.
	//
	// `unknown_outcome` is the case where the answer never arrived; this is the
	// wider class it belongs to. Marking only the first left the engine
	// retrying every other undecided mutation — the same defect, one error
	// class over.
	Ambiguous bool
	// Cause is the underlying Go error for a transport failure.
	Cause error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{string(e.Class)}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, ": ")
}

// AmbiguousEffect implements runtime.AmbiguousEffect, the seam that tells the
// engine's recovery dispatcher this failure must never be retried
// automatically.
//
// Retryable below is this package's own answer, and it was not enough: it is
// consulted by a caller that decides to retry, whereas the ENGINE decides on
// its own when a node returns an error. Without this, an `unknown_outcome`
// reached recovery as ordinary text, classified as EXECUTION_FAILED, and was
// replayed two seconds later — the duplicate mutation the class exists to
// prevent, arriving through the one path that never asked.
func (e *Error) AmbiguousEffect() bool {
	return e != nil && (e.Class == spec.ErrUnknownOutcome || e.Ambiguous)
}

// markAmbiguous decides whether a DISPATCHED failure left a mutation
// undecided, and is the one place that judgement is made.
//
// Only a mutation can be undecided — a read that failed changed nothing, so it
// is always safe to repeat. Among mutations, the statuses that leave the
// question open are the ones where the vendor may have acted before failing to
// say so:
//
//   - 5xx: the write may have committed and the server then failed.
//   - 408 / 504: a timeout AFTER the request was received.
//   - a 2xx whose body could not be read: the mutation certainly HAPPENED and
//     only the answer is lost, which makes a repeat a duplicate rather than a
//     second chance.
//
// A 4xx is excluded deliberately: the vendor refused the request, so nothing
// happened and a retry duplicates nothing. Marking those ambiguous would park
// runs that should simply fail.
func markAmbiguous(op spec.Operation, err *Error, status int) {
	if err == nil || !op.Effect.Mutating() {
		return
	}
	switch {
	case status >= 500,
		status == http.StatusRequestTimeout,
		status >= 200 && status < 300,
		// 303 See Other is the canonical answer to a POST whose effect
		// ALREADY HAPPENED ("done, go look over there"), and 302 is used the
		// same way by vendors and by anything fronting them. iterion does not
		// follow either — the guarded dialer pins the host it resolved — so
		// the redirect is not a success, but the mutation may well have been
		// performed, which is the definition of undecided.
		//
		// The other 3xx are NOT undecided and are deliberately absent: 307 and
		// 308 preserve the method precisely because the request must be
		// re-sent, and 301 is a resource that moved. Each says the vendor did
		// not process this call.
		status == http.StatusFound,
		status == http.StatusSeeOther:
		err.Ambiguous = true
	}
}

// Retryable reports whether repeating this call is safe AND useful. It is the
// one place the two questions meet, which is why it takes the operation: a
// class may be retryable in the abstract while the operation is not, and
// answering only the first is how a blind retry duplicates an effect.
//
// It takes the call's ARGUMENTS as well, because the operation alone cannot
// answer the question. What makes a repeat safe is a key the vendor actually
// received, not a parameter the package mentions: an optional
// `Idempotency-Key` that the caller left unset licensed the retry it was
// supposed to earn.
func (e *Error) Retryable(op spec.Operation, params map[string]any) bool {
	if e == nil {
		return false
	}
	if e.Class == spec.ErrUnknownOutcome {
		// The whole meaning of the class: iterion cannot tell whether the
		// mutation happened, so repeating it is exactly what must not happen
		// automatically.
		return false
	}
	if !e.Class.Retryable() {
		return false
	}
	if !op.Effect.Mutating() {
		return true
	}
	// A mutation may only be repeated when THIS call carried a key that makes
	// it idempotent. Rate limiting is the exception: a 429 means the request
	// was REFUSED, not performed, so repeating it cannot duplicate anything.
	return e.Class == spec.ErrRateLimited || idempotencyKeySent(op, params)
}

// idempotencyKeySent reports whether this call actually carried a usable
// idempotency key.
//
// The one reading of that question, shared by Retryable and transportError,
// because they are the two halves of the same promise: one decides whether a
// repeat is allowed, the other decides whether a lost answer is ambiguous, and
// they must never disagree about what makes a mutation safe.
//
// A declared parameter is not a sent key, and a sent EMPTY key is not a key:
// a vendor receiving `Idempotency-Key: ""` deduplicates nothing.
func idempotencyKeySent(op spec.Operation, params map[string]any) bool {
	if op.IdempotencyKeyParam == "" {
		return false
	}
	// The declaration may name either the public key or the wire name; the
	// caller writes the public key. Validation guarantees one of them matches
	// a declared parameter.
	keys := []string{op.IdempotencyKeyParam}
	for _, p := range op.Params {
		if p.Name == op.IdempotencyKeyParam && p.Key != op.IdempotencyKeyParam {
			keys = append(keys, p.Key)
		}
	}
	for _, k := range keys {
		v, ok := params[k]
		if !ok || v == nil {
			continue
		}
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
			continue
		}
		return true
	}
	return false
}

// Call performs ONE attempt of an operation and classifies its answer.
//
// One attempt on purpose: a retry needs a run's budget, its clock and its
// checkpoint, none of which belong here. What does belong here is deciding
// whether a retry is permissible at all — see Error.Retryable.
func (e *Executor) Call(ctx context.Context, pkg *spec.Package, op spec.Operation, params map[string]any, cred Credential) (Result, error) {
	if e.Client == nil {
		return Result{}, fmt.Errorf("exec: no HTTP client — a connector must call through the guarded client, never a default one")
	}
	req, err := e.buildRequest(ctx, pkg, op, params, cred)
	if err != nil {
		// A local refusal: nothing was sent, so this is a bad request in the
		// typed sense rather than an iterion failure.
		var bad *Error
		if asError(err, &bad) {
			return Result{Err: bad}, nil
		}
		return Result{}, err
	}

	resp, err := e.Client.Do(req)
	if err != nil {
		return Result{Err: e.transportError(op, params, redactSecrets(err, op, params, cred))}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// A vendor answer is bounded: an operation that streams gigabytes would
	// otherwise decide a run's memory.
	const maxResponseBytes = 32 << 20
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		// The status arrived, the body did not. For a mutation that is the
		// ambiguous case: the vendor may well have performed it.
		return Result{Status: resp.StatusCode, Err: e.transportError(op, params, redactSecrets(readErr, op, params, cred))}, nil
	}
	res := e.readResponse(pkg, op, resp, body)
	res.Requests = 1
	// The VENDOR's own words are redacted too, not only iterion's.
	//
	// Redaction covered the transport path — the URL Go prints in a *url.Error
	// — and stopped there. But an error body is vendor text copied verbatim
	// into the message, and a gateway answering 403 routinely echoes the
	// credential it rejected. That message becomes the node's error and
	// travels to the run's events, the tool hooks and error tracking. This is
	// the last point where the secret bytes are still identifiable.
	redactErrorText(res.Err, op, params, cred)
	return res, nil
}

// redactErrorText strips credential material out of a typed error's own text.
//
// Separate from redactSecrets, which works on a Go error: here the fields are
// structured, so the Code and the Message are scrubbed in place and the typed
// error — with its retry disposition and its ambiguity marker — survives
// intact. Replacing it with a flat error would have traded a leak for a
// duplicate mutation.
func redactErrorText(e *Error, op spec.Operation, params map[string]any, cred Credential) {
	if e == nil {
		return
	}
	for _, secret := range secretValues(op, params, cred) {
		if secret == "" {
			continue
		}
		e.Message = strings.ReplaceAll(e.Message, secret, redactedMarker)
		e.Code = strings.ReplaceAll(e.Code, secret, redactedMarker)
		if esc := url.QueryEscape(secret); esc != secret {
			e.Message = strings.ReplaceAll(e.Message, esc, redactedMarker)
		}
	}
}

// transportError classifies a call that got no usable answer.
//
// This is where `unknown_outcome` is produced, and the distinction it draws is
// the package's most consequential: a READ that failed to answer can simply be
// repeated, while a MUTATION that failed to answer may or may not have
// happened. Reporting the second as a plain transport error would invite a
// retry that duplicates the effect.
func (e *Executor) transportError(op spec.Operation, params map[string]any, cause error) *Error {
	if op.Effect.Mutating() && !idempotencyKeySent(op, params) {
		return &Error{
			Class: spec.ErrUnknownOutcome,
			Message: "the request was sent and no answer came back; this operation mutates and this call carries no idempotency key, " +
				"so iterion cannot tell whether it happened — reconcile before retrying",
			Cause: cause,
		}
	}
	return &Error{Class: spec.ErrTransport, Message: cause.Error(), Cause: cause}
}

// Page is one step of a paginated walk.
type Page struct {
	Result Result
	// Items is the collection this page carried, extracted per the
	// pagination's ItemsField (or the whole body when it is a bare array).
	Items []any
}

// CallPaged walks a paginated collection, returning every item and the last
// page's result.
//
// Complete is what a caller must read before treating the items as the whole
// collection: the walk stops at the declared MaxPages, and a walk that stopped
// early SAYS so rather than looking finished. A silent truncation is how a
// workflow concludes that an issue does not exist because it was on page 21.
func (e *Executor) CallPaged(ctx context.Context, pkg *spec.Package, op spec.Operation, params map[string]any, cred Credential) (items []any, complete bool, last Result, err error) {
	if op.Pagination == nil {
		res, callErr := e.Call(ctx, pkg, op, params, cred)
		if callErr != nil || !res.OK() {
			return nil, false, res, callErr
		}
		// No pagination declared: the operation was never promised to be a
		// collection, so a non-array body is an ordinary single result rather
		// than a defect. Its items are simply empty.
		got, _ := pageItems(res.Data, "")
		return got, true, res, nil
	}

	p := op.Pagination
	maxPages := p.MaxPages
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}
	// The package's own number is an upper bound the PACKAGE chose, and a
	// package is not the workflow's to trust with an unbounded one: they
	// arrive from tiers a `.bot` never named, and spec validation cross-checks
	// the pagination block's parameter names without ever looking at this
	// number. Clamped rather than refused — a large walk is a legitimate ask,
	// and the honest answer to "further than iterion will go" is the partial
	// collection with complete=false, which is what that flag already means.
	if maxPages > maxWalkPages {
		maxPages = maxWalkPages
	}
	walk := make(map[string]any, len(params)+2)
	for k, v := range params {
		walk[k] = v
	}

	// An author who set the page or cursor parameter themselves is paging by
	// hand: they asked for ONE page. Walking on from there would silently
	// return several where one was requested, so the walk does not start.
	if authorPaged(op, p, params) {
		res, callErr := e.Call(ctx, pkg, op, walk, cred)
		if callErr != nil || !res.OK() {
			return nil, false, res, callErr
		}
		batch, found := pageItems(res.Data, p.ItemsField)
		if !found {
			return nil, false, res, errNoCollection(op, p.ItemsField, res.Data)
		}
		// NEVER complete. Complete means "these items are the whole
		// collection", and one hand-picked page is not: asking for page 7
		// says nothing about pages 1-6, and a short page 7 says nothing
		// about page 8 either. Reporting a short hand-picked page as
		// complete told a workflow it had seen everything after it had
		// deliberately looked at one slice.
		return batch, false, res, nil
	}

	// The size the vendor is actually being asked for, which is what a short
	// page must be measured against. A caller who set the size parameter
	// themselves overrides the package's default, and comparing their pages
	// to the default instead would end the walk on the first page whenever
	// they asked for less — or never, whenever they asked for more.
	effectiveSize := p.DefaultSize
	if p.SizeParam != "" {
		sizeKey := keyForWireName(op, p.SizeParam)
		if given, set := walk[sizeKey]; set {
			if n, ok := asPositiveInt(given); ok {
				effectiveSize = n
			}
		} else if p.DefaultSize > 0 {
			walk[sizeKey] = p.DefaultSize
		}
	}

	page := 1
	cursor := ""
	walked := 0
	for n := 0; n < maxPages; n++ {
		switch p.Style {
		case spec.PageNumber:
			if p.PageParam != "" {
				walk[keyForWireName(op, p.PageParam)] = page
			}
		case spec.PageOffset:
			if p.PageParam != "" {
				// Advanced by the size actually requested: stepping by the
				// package default while the caller asked for another size
				// skips rows or returns them twice.
				walk[keyForWireName(op, p.PageParam)] = (page - 1) * max(effectiveSize, 1)
			}
		case spec.PageCursor:
			if cursor != "" && p.CursorParam != "" {
				walk[keyForWireName(op, p.CursorParam)] = cursor
			}
		default:
			return nil, false, Result{}, fmt.Errorf("exec: operation %q declares pagination style %q, which this build cannot walk", op.ID, p.Style)
		}

		res, callErr := e.Call(ctx, pkg, op, walk, cred)
		if callErr != nil {
			return items, false, res, callErr
		}
		if !res.OK() {
			return items, false, res, nil
		}
		// The walk's TOTAL, not the last page's one: what an operator needs to
		// know is how many of the vendor's rate-limit slots this node spent.
		res.Requests = n + 1
		walked += res.Bytes
		res.Bytes = walked
		last = res
		batch, found := pageItems(res.Data, p.ItemsField)
		if !found {
			// Refused rather than treated as the end of the collection: the
			// items gathered so far are returned with the error, so a caller
			// that logs both can see how far the walk got.
			return items, false, last, errNoCollection(op, p.ItemsField, res.Data)
		}
		items = append(items, batch...)

		// The walk's own ceiling, checked after the page that crossed it is
		// KEPT: a page is bounded (the 32 MiB LimitReader) but the walk was
		// not, and `items` is held entire until the call returns and is then
		// marshalled whole into the artifact. At the default twenty pages that
		// is already ~640 MiB with no malicious package in sight.
		//
		// Reported as an incomplete collection rather than an error, because
		// that is what it is, and `complete=false` — which CallPaged's
		// contract already tells a caller to read — is exactly the "stopped
		// early, there may be more" signal.
		if walked >= maxWalkBytes {
			return items, false, last, nil
		}

		// Termination is the PROTOCOL's, and for a cursor walk the CURSOR is
		// the whole of it — checked before the batch, not after.
		//
		// An empty page is not the end of a cursor walk: Slack documents
		// exactly this, a page carrying no items and a next cursor, and
		// stopping there returned an empty collection marked complete while
		// the vendor was still saying "there is more". The length test was
		// moved out of the cursor branch once; it had to be moved out of the
		// shared pre-check too.
		if p.Style == spec.PageCursor {
			cursor = stringField(res.Data, p.CursorField)
			if cursor == "" {
				return items, true, last, nil
			}
			page++
			continue
		}

		// A page/offset walk ends on an empty page: there is nothing to
		// follow, and no cursor to say otherwise.
		if len(batch) == 0 {
			return items, true, last, nil
		}
		if effectiveSize > 0 && len(batch) < effectiveSize {
			// For page/offset walks a short page IS the end signal — measured
			// against what was actually asked for.
			return items, true, last, nil
		}
		page++
	}
	// Stopped at the bound, so the collection may well continue.
	return items, false, last, nil
}

// defaultMaxPages bounds a walk whose package declared no ceiling. A
// collection is unbounded from iterion's side; a run's budget is not.
const defaultMaxPages = 20

// maxWalkPages is the ceiling a package's own `max_pages` is clamped to, and
// maxWalkBytes the raw response budget of one walk.
//
// The page bound alone is not a bound: each page reads up to maxResponseBytes
// and every page's items are held until the call returns, so `max_pages: 5000`
// — a number nothing validated — is unbounded in practice. The byte budget is
// what actually protects the pod, the page ceiling what keeps a walk from
// spending thousands of the vendor's rate-limit slots behind one node.
//
// Both end the walk the way running out of pages already does: the items
// gathered so far, complete=false.
const (
	maxWalkPages = 500
	maxWalkBytes = 64 << 20
)

// redactedMarker replaces a secret in any text a human or a log will see.
const redactedMarker = "…redacted…"

// redactSecrets strips credential material out of a transport error before it
// becomes a node error.
//
// A transport failure's text is Go's `*url.Error`, which prints the FULL
// request URL — and an auth scheme placed `in: query` puts the credential
// there, so the token appeared verbatim in an error that travels to the run's
// events, the tool hooks and error tracking. A secret-marked parameter can
// reach the same text. Neither belongs in any of those places, and no reader
// downstream can redact what it cannot recognise: only here is it still known
// which bytes are the secret.
func redactSecrets(err error, op spec.Operation, params map[string]any, cred Credential) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	out := msg
	// The credential itself, and its URL-escaped form: it reaches the query
	// string encoded, so matching only the raw bytes would miss exactly the
	// case that leaks.
	for _, secret := range secretValues(op, params, cred) {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, redactedMarker)
		if esc := url.QueryEscape(secret); esc != secret {
			out = strings.ReplaceAll(out, esc, redactedMarker)
		}
	}
	if out == msg {
		return err
	}
	// The typed error is deliberately NOT preserved: its own Error() would
	// re-render the URL and undo the redaction the moment anything unwrapped
	// it. What a caller needs from a transport failure is the text.
	return errors.New(out)
}

// secretValues lists every string in this call that must never appear in a
// message: the credential — in every shape it travels in — and each parameter
// the package marked secret.
func secretValues(op spec.Operation, params map[string]any, cred Credential) []string {
	out := []string{cred.Value}
	// BOTH halves of a basic credential, and the base64 blob they travel as.
	//
	// `applyCredential` sends `Basic base64(Username+":"+Password)` for
	// `AuthBasic`, so this layer transmits bytes its redaction could not
	// recognise — the exact failure mode already closed for token-style
	// credentials, left open for the one scheme the shipped Forgejo package
	// declares.
	//
	// The username is a user-id rather than a secret in RFC 7617's own terms,
	// and redacting it costs a reader the owner segment of a URL when the two
	// coincide. It is in the set anyway: a connector catalog accepts whatever
	// vendor a package describes, and putting the KEY in the user-id half
	// (`<api key>:` with an empty password) is a widespread convention. This
	// layer cannot tell which half a given vendor made secret, and the cost of
	// over-redacting is a marker in an error message, while the cost of
	// under-redacting is a key in the run's events. The base64 form covers the
	// vendor that echoes the header it rejected, the raw halves the vendor
	// that decodes it first.
	out = append(out, cred.Username, cred.Password)
	if cred.Username != "" || cred.Password != "" {
		out = append(out, base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password)))
	}
	for _, p := range op.Params {
		if !p.Secret {
			continue
		}
		if v, ok := params[p.Key]; ok {
			if s := scalarString(v); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// asPositiveInt reads a page size a caller supplied. Values arrive from a
// `.bot` through template rendering and JSON decoding, so the same number can
// be an int, a float64 or a string depending on the path it took.
func asPositiveInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n > 0
	case int64:
		return int(n), n > 0
	case float64:
		return int(n), n > 0 && n == float64(int(n))
	case json.Number:
		// The shape an action node's coercion now produces for an integer —
		// added so a large id survives to the wire exactly. Without this case
		// the caller's `limit: 2` was not recognised as a size at all, so the
		// walk compared its two-item pages against the package's default of
		// 100 and called the first one short: one request, and "complete".
		// A fix in one package silently un-fixed a guard in another.
		i, err := n.Int64()
		return int(i), err == nil && i > 0
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil && i > 0
	}
	return 0, false
}

// authorPaged reports whether the caller supplied the position themselves —
// the signal that they are paging by hand and want exactly what they asked
// for. The SIZE parameter is deliberately NOT such a signal: asking for 100
// per page says nothing about wanting only the first hundred.
func authorPaged(op spec.Operation, p *spec.Pagination, params map[string]any) bool {
	for _, wire := range []string{p.PageParam, p.CursorParam} {
		if wire == "" {
			continue
		}
		if _, set := params[keyForWireName(op, wire)]; set {
			return true
		}
	}
	return false
}

// keyForWireName maps a pagination parameter's WIRE name onto the public key
// a caller writes, since an overlay may have renamed it. Falls back to the
// wire name, which is the key whenever nothing was renamed.
func keyForWireName(op spec.Operation, wire string) string {
	for _, p := range op.Params {
		if p.Name == wire {
			return p.Key
		}
	}
	return wire
}

// pageItems extracts a page's collection: the named field, or the body itself
// when it is already an array.
//
// The second result separates "this page carried no items" from "there is no
// collection at this address". Both used to be an empty slice, and the walk
// reads an empty page as the end of the collection — so a package pointing at
// the wrong field, or a vendor wrapping its array in an envelope, made
// CallPaged return zero items and report the walk COMPLETE. That is the exact
// silent truncation the Complete flag exists to prevent, arriving through the
// extraction rather than through the bound.
func pageItems(data any, field string) ([]any, bool) {
	target := data
	if field != "" {
		target = descend(data, field)
	}
	switch v := target.(type) {
	case []any:
		return v, true
	case nil:
		// An absent or null field is a miss when a field was named. A vendor
		// answering a bare `null` body for an empty collection is answering
		// honestly, so an unnamed field accepts it.
		return nil, field == ""
	default:
		return nil, false
	}
}

// errNoCollection reports a response whose declared collection is not where
// the package says it is. It names both addresses because the fix is always in
// one of them: the operation's items_field, or the vendor's shape.
func errNoCollection(op spec.Operation, field string, data any) error {
	at := "the response body"
	if field != "" {
		at = "field " + strconv.Quote(field)
	}
	return fmt.Errorf("exec: operation %q is declared paginated, but %s carries no array (got %s); "+
		"set items_field on the operation to the field that does", op.ID, at, jsonShape(data))
}

// jsonShape names a decoded body's shape for a diagnostic, listing an object's
// keys so the reader can see the field they should have named.
func jsonShape(data any) string {
	switch v := data.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 8 {
			keys = append(keys[:8], "…")
		}
		return "an object with keys [" + strings.Join(keys, " ") + "]"
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	default:
		return fmt.Sprintf("%T", data)
	}
}

func stringField(data any, field string) string {
	if field == "" {
		return ""
	}
	s, _ := descend(data, field).(string)
	return s
}

// descend walks a dotted path into a decoded JSON value.
func descend(data any, path string) any {
	cur := data
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

// asError reports whether err is one of this package's typed errors.
func asError(err error, out **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*out = e
	}
	return ok
}

// decodeJSON decodes a body, tolerating an empty one.
func decodeJSON(body []byte) (any, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}
