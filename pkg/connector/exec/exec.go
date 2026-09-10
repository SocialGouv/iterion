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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Retryable reports whether repeating this call is safe AND useful. It is the
// one place the two questions meet, which is why it takes the operation: a
// class may be retryable in the abstract while the operation is not, and
// answering only the first is how a blind retry duplicates an effect.
func (e *Error) Retryable(op spec.Operation) bool {
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
	// A mutation may only be repeated when the vendor offers a way to make it
	// idempotent. Rate limiting is the exception: a 429 means the request was
	// REFUSED, not performed, so repeating it cannot duplicate anything.
	return e.Class == spec.ErrRateLimited || op.IdempotencyKeyParam != ""
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
		return Result{Err: e.transportError(op, err)}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// A vendor answer is bounded: an operation that streams gigabytes would
	// otherwise decide a run's memory.
	const maxResponseBytes = 32 << 20
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		// The status arrived, the body did not. For a mutation that is the
		// ambiguous case: the vendor may well have performed it.
		return Result{Status: resp.StatusCode, Err: e.transportError(op, readErr)}, nil
	}
	return e.readResponse(pkg, op, resp, body), nil
}

// transportError classifies a call that got no usable answer.
//
// This is where `unknown_outcome` is produced, and the distinction it draws is
// the package's most consequential: a READ that failed to answer can simply be
// repeated, while a MUTATION that failed to answer may or may not have
// happened. Reporting the second as a plain transport error would invite a
// retry that duplicates the effect.
func (e *Executor) transportError(op spec.Operation, cause error) *Error {
	if op.Effect.Mutating() && op.IdempotencyKeyParam == "" {
		return &Error{
			Class: spec.ErrUnknownOutcome,
			Message: "the request was sent and no answer came back; this operation mutates and the vendor offers no idempotency key, " +
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
	walk := make(map[string]any, len(params)+2)
	for k, v := range params {
		walk[k] = v
	}

	// An author who set the page or cursor parameter themselves is paging by
	// hand: they asked for ONE page. Walking on from there would silently
	// return several where one was requested, so the walk does not start —
	// the single page is performed and its completeness reported honestly.
	if authorPaged(op, p, params) {
		res, callErr := e.Call(ctx, pkg, op, walk, cred)
		if callErr != nil || !res.OK() {
			return nil, false, res, callErr
		}
		batch, found := pageItems(res.Data, p.ItemsField)
		if !found {
			return nil, false, res, errNoCollection(op, p.ItemsField, res.Data)
		}
		return batch, p.DefaultSize > 0 && len(batch) < p.DefaultSize, res, nil
	}

	if p.DefaultSize > 0 && p.SizeParam != "" {
		if _, set := walk[keyForWireName(op, p.SizeParam)]; !set {
			walk[keyForWireName(op, p.SizeParam)] = p.DefaultSize
		}
	}

	page := 1
	cursor := ""
	for n := 0; n < maxPages; n++ {
		switch p.Style {
		case spec.PageNumber:
			if p.PageParam != "" {
				walk[keyForWireName(op, p.PageParam)] = page
			}
		case spec.PageOffset:
			if p.PageParam != "" {
				walk[keyForWireName(op, p.PageParam)] = (page - 1) * max(p.DefaultSize, 1)
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
		last = res
		batch, found := pageItems(res.Data, p.ItemsField)
		if !found {
			// Refused rather than treated as the end of the collection: the
			// items gathered so far are returned with the error, so a caller
			// that logs both can see how far the walk got.
			return items, false, last, errNoCollection(op, p.ItemsField, res.Data)
		}
		items = append(items, batch...)

		// A short page ends the walk on every style: it is the one signal
		// every vendor gives.
		if p.DefaultSize > 0 && len(batch) < p.DefaultSize {
			return items, true, last, nil
		}
		if len(batch) == 0 {
			return items, true, last, nil
		}
		if p.Style == spec.PageCursor {
			cursor = stringField(res.Data, p.CursorField)
			if cursor == "" {
				return items, true, last, nil
			}
		}
		page++
	}
	// Stopped at the bound, so the collection may well continue.
	return items, false, last, nil
}

// defaultMaxPages bounds a walk whose package declared no ceiling. A
// collection is unbounded from iterion's side; a run's budget is not.
const defaultMaxPages = 20

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
