package spec

import "strings"

// Operation is one callable action of a connector: a method, a path, typed
// parameters, typed results, and a closed set of error classes. It is the
// SAME unit on both offers — a deterministic `tool … action:` node calls it
// with no LLM anywhere in the path, and the MCP facade exposes it to an agent
// — because the two differ in who chooses the arguments, never in what the
// call is.
//
// That single-unit property is what makes "a connector covers a service"
// mean something: a package cannot claim an operation for the agent side and
// silently lack it on the deterministic side.
//
// Everything here is EXECUTION SEMANTICS, not documentation. An operation a
// package cannot describe precisely enough to build the request and read the
// answer is not published as an operation at all — it is a coverage gap in
// the generation report. The alternative, an executable-looking operation
// with missing information, is the failure this package exists to avoid.
type Operation struct {
	// ID is `<connector>.<resource>.<verb>` (e.g. forgejo.issue.create_comment),
	// stable across regenerations and quoted verbatim in a `.bot`. It is
	// derived from the vendor's operationId when there is one, but it is NOT
	// that id: a vendor rename must not break every workflow that calls it,
	// so the mapping is recorded (SourceOperationID) and an overlay pins the
	// id whenever the derivation would move.
	ID string `yaml:"id" json:"id"`
	// SourceOperationID is the vendor's own operationId, kept so a
	// regeneration can tell "this operation moved" from "this is a new one".
	SourceOperationID string `yaml:"source_operation_id,omitempty" json:"source_operation_id,omitempty"`

	Summary     string `yaml:"summary,omitempty" json:"summary,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Resource and Verb are the parsed halves of ID, kept explicit so an
	// index can group by resource without re-splitting a string.
	Resource string `yaml:"resource" json:"resource"`
	Verb     string `yaml:"verb" json:"verb"`
	// Tags are the vendor's own grouping, used for the ops/<tag>.yaml split
	// and for search. Not semantic to the engine.
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`

	HTTP HTTPBinding `yaml:"http" json:"http"`

	// Params is the flattened parameter list — path, query, header and body
	// alike. Flattened on purpose: a node author writes ONE params map and
	// the executor places each value where its In says, so the `.bot` never
	// has to mirror the vendor's request layout.
	Params []Param `yaml:"params,omitempty" json:"params,omitempty"`
	// Results are the success shapes, lowest status first. A list rather than
	// one value because a vendor legitimately answers 200 or 201 or 202 for
	// the same operation, and a `.bot` that must tell an accepted-and-pending
	// answer from a completed one can only do so if the package kept both.
	Results []ResultCase `yaml:"results,omitempty" json:"results,omitempty"`
	// Errors is the closed set of failure shapes the vendor documents, each
	// mapped to an iterion error class. A status the package does not declare
	// is still classified at runtime (by status range) — the list makes the
	// documented ones typed, it does not pretend to be exhaustive.
	Errors []ErrorSpec `yaml:"errors,omitempty" json:"errors,omitempty"`
	// Outcome overrides the connector-wide outcome policy for this operation.
	// Nil inherits — which is the usual case, since an API that signals
	// failure inside a 200 does so everywhere.
	Outcome *OutcomePolicy `yaml:"outcome,omitempty" json:"outcome,omitempty"`

	// Security lists the ALTERNATIVE ways to authenticate this operation:
	// the call is authorized when any one requirement is satisfied, and a
	// requirement may name several schemes at once. Empty inherits the
	// connector's default; Anonymous is the explicit "no credential needed".
	Security  []SecurityRequirement `yaml:"security,omitempty" json:"security,omitempty"`
	Anonymous bool                  `yaml:"anonymous,omitempty" json:"anonymous,omitempty"`

	// Pagination, when set, tells the executor how to walk pages. A machine
	// can rarely derive it (the spec shows a `page` parameter, never that the
	// response is a page of something), so it is overlay territory.
	Pagination *Pagination `yaml:"pagination,omitempty" json:"pagination,omitempty"`

	// Effect classifies what calling this does to the remote system. It is
	// the property a retry decision hangs on, so it is stated rather than
	// inferred from the HTTP method: a POST that creates and a POST that
	// searches must not share a retry policy.
	Effect Effect `yaml:"effect" json:"effect"`
	// IdempotencyKeyParam names the parameter through which a caller can make
	// a mutating call safely retryable (a vendor's "Idempotency-Key" header).
	// Empty means the vendor offers none — which is what forces a mutating
	// retry to report an UNKNOWN outcome instead of retrying blind.
	IdempotencyKeyParam string `yaml:"idempotency_key_param,omitempty" json:"idempotency_key_param,omitempty"`

	// Maturity overrides the package floor for this operation, and may never
	// exceed it. Empty inherits.
	Maturity Maturity `yaml:"maturity,omitempty" json:"maturity,omitempty"`
	// Deterministic is false for an operation iterion will not certify on the
	// node path (one whose result depends on a model, or whose semantics the
	// package could not pin down). Such an operation may still be exposed to
	// an agent through the MCP facade — the two offers are qualified apart,
	// which is the whole reason the field exists rather than being assumed
	// true for everything HTTP.
	Deterministic bool `yaml:"deterministic" json:"deterministic"`
	// MCP marks the operation as part of the facade's CURATED tool set. Most
	// operations of a large API are false: 500 tools would bury an agent's
	// context, so the facade lists the curated ones and reaches the rest via
	// its search_operations / call_operation pair.
	MCP bool `yaml:"mcp,omitempty" json:"mcp,omitempty"`
}

// PrimaryResult is the lowest-status success case — the answer a caller gets
// when nothing unusual happened. Callers that must distinguish variants read
// Results directly.
func (op Operation) PrimaryResult() ResultCase {
	if len(op.Results) == 0 {
		return ResultCase{}
	}
	return op.Results[0]
}

// HTTPBinding is where the call goes on the wire.
type HTTPBinding struct {
	// Method is upper-case ("GET", "POST", …).
	Method string `yaml:"method" json:"method"`
	// Path is the templated path under the connector's base URL and path
	// prefix, with `{name}` placeholders matching Params of In "path".
	Path string `yaml:"path" json:"path"`
	// RequestBody is how the body params are encoded; empty when there are
	// none. It is REQUIRED as soon as an operation has body/form params,
	// because "which encoding" is not a detail the executor may guess: the
	// same field set means different bytes as JSON, as form-urlencoded and
	// as multipart.
	RequestBody BodyEncoding `yaml:"request_body,omitempty" json:"request_body,omitempty"`
	// ContentType is the media type the vendor actually named, when it is not
	// the canonical one for RequestBody.
	//
	// It exists because the ENCODING and the MEDIA TYPE are different facts,
	// and collapsing them sends the wrong header. `application/json-patch+json`
	// and `application/vnd.api+json` are JSON on the wire — the encoding is
	// right — but they are not `application/json`, and a vendor that declares
	// one of them refuses the other outright. Sending the canonical type meant
	// an operation that the description says exists, that iterion can encode
	// correctly, and that the vendor rejects at the header.
	//
	// Empty means the canonical type for the encoding, which is the common
	// case and keeps every existing package unchanged.
	ContentType string `yaml:"content_type,omitempty" json:"content_type,omitempty"`
	// Accept overrides the response media type when the vendor answers
	// something other than JSON.
	Accept string `yaml:"accept,omitempty" json:"accept,omitempty"`
}

// RequestContentType is the header value to send: the vendor's own media type
// when it named a specific one, else the canonical type for the encoding.
func (h HTTPBinding) RequestContentType() string {
	if h.ContentType != "" {
		return h.ContentType
	}
	switch h.RequestBody {
	case BodyJSON:
		return "application/json"
	case BodyForm:
		return "application/x-www-form-urlencoded"
	}
	// Multipart's header carries the generated boundary, so it is never
	// derived from the encoding alone — the body builder produces it.
	return ""
}

// BodyEncoding is the wire encoding of a request body. Only the three iterion
// can actually build are named: a vendor that requires anything else (XML, a
// raw octet stream, a bespoke type) makes the operation a coverage gap rather
// than an operation whose body would be silently mis-encoded.
type BodyEncoding string

const (
	// BodyJSON is `application/json`.
	BodyJSON BodyEncoding = "json"
	// BodyForm is `application/x-www-form-urlencoded` — Slack's whole Web API
	// takes its arguments this way.
	BodyForm BodyEncoding = "form"
	// BodyMultipart is `multipart/form-data`, the file-upload encoding.
	BodyMultipart BodyEncoding = "multipart"
)

// ParamIn is where a parameter's value is placed on the wire.
type ParamIn string

const (
	InPath   ParamIn = "path"
	InQuery  ParamIn = "query"
	InHeader ParamIn = "header"
	// InBody is a member of the request body. Swagger 2.0's single `in: body`
	// parameter, its `in: formData` parameters and OpenAPI 3's requestBody all
	// flatten to a set of InBody params; HTTPBinding.RequestBody says how they
	// are encoded, so a node author writes one flat map either way.
	InBody ParamIn = "body"
)

// ParamStyle is how a non-scalar value is serialized into a query string or a
// header. It is not decoration: `labels=[a,b]` reaches a vendor as
// `labels=a,b`, as `labels=a&labels=b`, or as `labels=a%20b` depending on the
// style, and only one of those is the one the vendor parses.
type ParamStyle string

const (
	// StyleForm is the default for query parameters: comma-joined when not
	// exploded, repeated `k=v` pairs when exploded.
	StyleForm ParamStyle = "form"
	// StyleSimple is the default for path and header parameters: comma-joined.
	StyleSimple ParamStyle = "simple"
	// StyleSpaceDelimited joins with spaces (Swagger's `ssv`).
	StyleSpaceDelimited ParamStyle = "spaceDelimited"
	// StylePipeDelimited joins with pipes (Swagger's `pipes`).
	StylePipeDelimited ParamStyle = "pipeDelimited"
	// StyleDeepObject encodes an object as `k[prop]=v`.
	StyleDeepObject ParamStyle = "deepObject"
)

// Param is one input of an operation, already flattened out of the vendor's
// request layout.
type Param struct {
	// Key is the name a `.bot` writes in its `params:` map, unique within the
	// operation. Name is the WIRE name, which is not always usable as a key:
	// an operation legitimately carries `name` in its path and another `name`
	// in its body, and one flat map cannot hold both under the same key. When
	// that happens the later location's key is prefixed (`body_name`), and
	// the wire name is untouched.
	Key      string  `yaml:"key" json:"key"`
	Name     string  `yaml:"name" json:"name"`
	In       ParamIn `yaml:"in" json:"in"`
	BodyPath string  `yaml:"body_path,omitempty" json:"body_path,omitempty"`

	Type        string   `yaml:"type" json:"type"`
	Format      string   `yaml:"format,omitempty" json:"format,omitempty"`
	Items       string   `yaml:"items,omitempty" json:"items,omitempty"`
	Enum        []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	Required    bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any      `yaml:"default,omitempty" json:"default,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`

	// Style and Explode carry the serialization of a non-scalar value.
	// Explode is a pointer because its DEFAULT depends on the style (true for
	// form, false for the others), so "unset" and "false" are different.
	Style   ParamStyle `yaml:"style,omitempty" json:"style,omitempty"`
	Explode *bool      `yaml:"explode,omitempty" json:"explode,omitempty"`

	// SchemaRef names a shared schema (see Package.Schemas) when the value is
	// an object or an array of objects. Shared rather than inlined because a
	// large API's schemas are the bulk of a package's bytes and are reused by
	// dozens of operations.
	SchemaRef string `yaml:"schema_ref,omitempty" json:"schema_ref,omitempty"`

	// Secret marks a parameter whose value must never be logged, echoed into
	// an event, or shown to a model. A credential normally arrives through
	// the connection, not here — but some APIs take one as an argument
	// (Slack's Web API declares `token` as an ordinary header parameter).
	Secret bool `yaml:"secret,omitempty" json:"secret,omitempty"`

	// WholeBody marks a body parameter that IS the request body rather than a
	// member of it — a vendor whose endpoint takes a bare array, or a scalar.
	//
	// Without it the generator invented a member called `body` and the builder
	// wrapped the value in an object, so an endpoint expecting `[1,2]`
	// received `{"body":[1,2]}`. The parameter still needs a NAME, because a
	// `.bot` has to address it; what the marker changes is that the name never
	// reaches the wire.
	//
	// Exclusive with ordinary body members: a request body is one shape or the
	// other, and Validate refuses a mixture.
	WholeBody bool `yaml:"whole_body,omitempty" json:"whole_body,omitempty"`
}

// ExplodeOrDefault resolves Explode against the style's own default, so an
// executor never has to know that "unset" means different things per style.
func (p Param) ExplodeOrDefault() bool {
	if p.Explode != nil {
		return *p.Explode
	}
	return p.Style == StyleForm || p.Style == ""
}

// ResultCase is one success shape of an operation.
type ResultCase struct {
	// Status is the documented success status (200, 201, 204…).
	Status int `yaml:"status" json:"status"`
	// SchemaRef names the response schema; empty for an empty body.
	SchemaRef string `yaml:"schema_ref,omitempty" json:"schema_ref,omitempty"`
	// Array marks a response that is a bare array of SchemaRef.
	Array bool `yaml:"array,omitempty" json:"array,omitempty"`
	// Description is the vendor's own wording for this case.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Pending marks a status that means "accepted, not finished" (202). A
	// workflow that treats it as completion acts on work that has not
	// happened, so the distinction is data rather than a caller's guess.
	Pending bool `yaml:"pending,omitempty" json:"pending,omitempty"`
}

// OutcomePolicy describes an API that does NOT signal failure with the HTTP
// status. It exists because a whole pilot service works that way: every Slack
// Web API method answers 200 and puts `{"ok": false, "error": "…"}` in the
// body, so a status-only executor would checkpoint a failure as a success —
// precisely where a workflow needs to branch.
//
// It is connector-wide by default, because an API that does this does it
// everywhere; an operation may override.
type OutcomePolicy struct {
	// SuccessWhen is an expression over the decoded response body (`body`)
	// and the HTTP status (`status`) that must hold for the call to count as
	// a success — e.g. `body.ok == true`. Empty means the status decides.
	//
	// It is evaluated by pkg/dsl/expr, which is TOTAL: no recursion, no I/O,
	// bounded combinators. An outcome predicate that could loop would be a
	// way to hang a deterministic node.
	SuccessWhen string `yaml:"success_when,omitempty" json:"success_when,omitempty"`
	// ErrorCodeField is the dotted path to the vendor's own error code in a
	// failed body (`error` for Slack), and ErrorMessageField to its prose.
	ErrorCodeField    string `yaml:"error_code_field,omitempty" json:"error_code_field,omitempty"`
	ErrorMessageField string `yaml:"error_message_field,omitempty" json:"error_message_field,omitempty"`
	// ErrorCodeMap maps a vendor code onto an iterion class, so a `.bot`
	// branches on `unauthorized` rather than on a vendor's spelling. A code
	// absent from the map classifies from the status, then falls back to
	// ErrBadRequest — a business failure is never silently a success.
	ErrorCodeMap map[string]ErrorClass `yaml:"error_code_map,omitempty" json:"error_code_map,omitempty"`
}

// ErrorClass is iterion's normalised failure vocabulary. It is what a `.bot`
// branches on and what decides retryability, so it stays small and closed:
// an unrecognised status maps into it by range rather than growing the enum.
type ErrorClass string

const (
	// ErrBadRequest — the caller's arguments are wrong. Never retryable.
	ErrBadRequest ErrorClass = "bad_request"
	// ErrUnauthorized — the credential is missing, expired or rejected.
	ErrUnauthorized ErrorClass = "unauthorized"
	// ErrForbidden — authenticated, but not permitted.
	ErrForbidden ErrorClass = "forbidden"
	// ErrNotFound — the addressed resource does not exist.
	ErrNotFound ErrorClass = "not_found"
	// ErrConflict — the request contradicts the resource's current state.
	ErrConflict ErrorClass = "conflict"
	// ErrRateLimited — retryable after a delay the response usually names.
	ErrRateLimited ErrorClass = "rate_limited"
	// ErrUpstream — the vendor failed (5xx). Retryable for a read; for a
	// mutation it is precisely the case where the outcome is UNKNOWN.
	ErrUpstream ErrorClass = "upstream"
	// ErrTransport — the call never got an answer (dial, TLS, timeout).
	ErrTransport ErrorClass = "transport"
	// ErrUnknownOutcome — the request was sent, the answer was lost, and the
	// operation is not idempotent: iterion cannot say whether it happened.
	// It is a first-class class, not an absence, because reporting it is the
	// only honest alternative to a blind retry that may duplicate an effect.
	ErrUnknownOutcome ErrorClass = "unknown_outcome"
)

// Retryable reports whether a class may be retried on its own. Note what is
// absent: ErrUpstream and ErrTransport are retryable HERE only as a class
// property — the executor still refuses to retry a mutating operation with
// no idempotency key, and reports ErrUnknownOutcome instead.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ErrRateLimited, ErrUpstream, ErrTransport:
		return true
	}
	return false
}

// ClassifyStatus maps an HTTP status onto the closed vocabulary, so a status
// the package never declared is still typed rather than opaque.
func ClassifyStatus(status int) ErrorClass {
	switch status {
	case 400, 422:
		return ErrBadRequest
	case 401:
		return ErrUnauthorized
	case 403:
		return ErrForbidden
	case 404, 410:
		return ErrNotFound
	case 409:
		return ErrConflict
	case 429:
		return ErrRateLimited
	}
	switch {
	case status >= 500:
		return ErrUpstream
	case status >= 400:
		return ErrBadRequest
	}
	return ""
}

// ClassifyCode maps a vendor's own error code through the policy, falling
// back to the status. It never returns "": a body-signalled failure with an
// unrecognised code is a bad request, not a success.
func (o *OutcomePolicy) ClassifyCode(code string, status int) ErrorClass {
	if o != nil && len(o.ErrorCodeMap) > 0 {
		if c, ok := o.ErrorCodeMap[code]; ok {
			return c
		}
	}
	if c := ClassifyStatus(status); c != "" {
		return c
	}
	return ErrBadRequest
}

// ErrorSpec is one documented failure of an operation.
type ErrorSpec struct {
	Status      int        `yaml:"status" json:"status"`
	Class       ErrorClass `yaml:"class" json:"class"`
	Description string     `yaml:"description,omitempty" json:"description,omitempty"`
	SchemaRef   string     `yaml:"schema_ref,omitempty" json:"schema_ref,omitempty"`
}

// SecurityRequirement is ONE way to authorize a call: every term in it must
// be satisfied together (a vendor that wants an app key AND a user token
// needs both). A list of requirements is a list of alternatives.
type SecurityRequirement struct {
	Terms []SecurityTerm `yaml:"terms" json:"terms"`
}

// SecurityTerm names a scheme and the scopes this operation needs from it.
//
// Scopes here are REQUIRED scopes, which is a different thing from the
// scopes a scheme advertises (AuthScheme.SupportedScopes) and from the ones
// a connection was actually granted. Collapsing the three is how an
// integration ends up requesting every scope a vendor offers in order to read
// one channel.
type SecurityTerm struct {
	SchemeID string   `yaml:"scheme" json:"scheme"`
	Scopes   []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
}

// SatisfiedBy reports whether a connection authenticating with schemeID and
// holding grantedScopes can perform this operation, and names what is missing
// when it cannot. A binding accepted at launch that cannot invoke its own
// operation is the failure this prevents — it would otherwise surface as a
// vendor 403 in the middle of a run.
func (op Operation) SatisfiedBy(schemeID string, grantedScopes []string) (bool, []string) {
	if op.Anonymous || len(op.Security) == 0 {
		return true, nil
	}
	granted := make(map[string]bool, len(grantedScopes))
	for _, s := range grantedScopes {
		granted[s] = true
	}
	var closest []string
	for _, req := range op.Security {
		// A multi-term requirement needs every term; iterion's connection
		// model carries ONE scheme, so a requirement naming two distinct
		// schemes cannot be satisfied by it — reported, not silently passed.
		ok := true
		var missing []string
		for _, term := range req.Terms {
			if term.SchemeID != schemeID {
				ok = false
				break
			}
			for _, want := range term.Scopes {
				if !granted[want] {
					missing = append(missing, want)
				}
			}
		}
		if !ok {
			continue
		}
		if len(missing) == 0 {
			return true, nil
		}
		if closest == nil || len(missing) < len(closest) {
			closest = missing
		}
	}
	if closest == nil {
		return false, []string{"no requirement is satisfiable with scheme " + schemeID}
	}
	return false, closest
}

// SatisfiableBy answers the question an UNKNOWN grant leaves open: could this
// scheme perform the operation, if every scope it is asked for were granted?
//
// It exists because "the provider never said what this token carries" is a
// third state, and both collapses are wrong. Read as "no scopes", an unstated
// grant refuses every PAT-backed connection — most providers never enumerate a
// token's scopes. Read as "all scopes", it authorises whatever is asked.
//
// So the SCOPES are left unjudged while the SCHEME conjunction is still
// enforced: a requirement naming two distinct schemes cannot be met by a
// connection holding one, however generous we are about scopes. Skipping that
// too — accepting any requirement with a matching term — is how a connection
// authorised for A alone performed an operation requiring A AND B.
//
// One definition, because two callers ask it: the executor before a call, and
// the connection layer before handing over a credential. A second copy would
// drift, and the drift would be a silent authorisation difference.
func (op Operation) SatisfiableBy(schemeID string) (bool, []string) {
	var required []string
	for _, req := range op.Security {
		for _, term := range req.Terms {
			if term.SchemeID == schemeID {
				required = append(required, term.Scopes...)
			}
		}
	}
	return op.SatisfiedBy(schemeID, required)
}

// Effect is what an operation does to the remote system.
type Effect string

const (
	// EffectRead changes nothing and may always be retried.
	EffectRead Effect = "read"
	// EffectCreate adds something; a blind retry duplicates it.
	EffectCreate Effect = "create"
	// EffectUpdate mutates in place; usually idempotent in practice but the
	// vendor rarely promises it, so it is not assumed.
	EffectUpdate Effect = "update"
	// EffectDelete removes; a repeat normally answers not_found.
	EffectDelete Effect = "delete"
)

// Mutating reports whether an effect changes remote state — the predicate the
// retry policy reads, so "is this safe to repeat" is asked once.
func (e Effect) Mutating() bool { return e != EffectRead }

// PaginationStyle names how a vendor pages a collection.
type PaginationStyle string

const (
	// PageNumber walks `page=1,2,3…` with a page-size parameter.
	PageNumber PaginationStyle = "page_number"
	// PageCursor follows an opaque cursor the response hands back.
	PageCursor PaginationStyle = "cursor"
	// PageOffset walks offset/limit.
	PageOffset PaginationStyle = "offset"
	// PageLink follows an RFC 5988 `Link: rel="next"` header.
	//
	// Declared but NOT yet walkable: the executor has no arm for it, so
	// ValidPaginationStyle refuses it and a package naming it fails
	// validation rather than failing at its first call in production.
	PageLink PaginationStyle = "link_header"
)

// Pagination is how the executor walks a collection. MaxPages bounds the walk
// so an operation cannot spend a run's budget on an unbounded collection; a
// walk that stops there says so in its result rather than looking complete.
type Pagination struct {
	Style PaginationStyle `yaml:"style" json:"style"`
	// PageParam / SizeParam / CursorParam are the request parameters, and
	// CursorField / ItemsField are where the response carries the next cursor
	// and the items.
	PageParam   string `yaml:"page_param,omitempty" json:"page_param,omitempty"`
	SizeParam   string `yaml:"size_param,omitempty" json:"size_param,omitempty"`
	CursorParam string `yaml:"cursor_param,omitempty" json:"cursor_param,omitempty"`
	CursorField string `yaml:"cursor_field,omitempty" json:"cursor_field,omitempty"`
	ItemsField  string `yaml:"items_field,omitempty" json:"items_field,omitempty"`
	// DefaultSize is the page size the executor asks for; MaxPages bounds the
	// number of pages a single call may walk (0 = the package's default).
	DefaultSize int `yaml:"default_size,omitempty" json:"default_size,omitempty"`
	MaxPages    int `yaml:"max_pages,omitempty" json:"max_pages,omitempty"`
}

// HasBodyParams reports whether the operation sends a request body, which is
// what makes HTTPBinding.RequestBody mandatory.
func (op Operation) HasBodyParams() bool {
	for _, p := range op.Params {
		if p.In == InBody {
			return true
		}
	}
	return false
}

// ValidBodyEncoding reports whether e is one iterion can actually build.
func ValidBodyEncoding(e BodyEncoding) bool {
	switch e {
	case BodyJSON, BodyForm, BodyMultipart:
		return true
	}
	return false
}

// ValidPaginationStyle reports whether s is one the executor can walk. A style
// it cannot walk is refused at validation rather than at the first call: the
// executor's own default arm returns an error, and discovering it there means
// discovering it in production.
func ValidPaginationStyle(s PaginationStyle) bool {
	switch s {
	case PageNumber, PageCursor, PageOffset:
		return true
	}
	return false
}

// NormalizeMediaType maps a media type onto the encoding iterion would use,
// or "" when it is one iterion cannot build. Parameters after `;` (charset,
// boundary) are ignored — they do not change how the body is assembled.
func NormalizeMediaType(mediaType string) BodyEncoding {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch mt {
	case "application/json":
		return BodyJSON
	case "application/x-www-form-urlencoded":
		return BodyForm
	case "multipart/form-data":
		return BodyMultipart
	}
	// A `+json` suffix (application/vnd.github.v3+json) is JSON on the wire.
	if strings.HasSuffix(mt, "+json") {
		return BodyJSON
	}
	return ""
}
