package spec

// Operation is one callable action of a connector: a method, a path, typed
// parameters, a typed result, and a closed set of error classes. It is the
// SAME unit on both offers — a deterministic `tool … action:` node calls it
// with no LLM anywhere in the path, and the MCP facade exposes it to an agent
// — because the two differ in who chooses the arguments, never in what the
// call is.
//
// That single-unit property is what makes "a connector covers a service"
// mean something: a package cannot claim an operation for the agent side and
// silently lack it on the deterministic side.
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
	// Result describes the success response.
	Result Result `yaml:"result" json:"result"`
	// Errors is the closed set of failure shapes the vendor documents, each
	// mapped to an iterion error class. A status the package does not declare
	// is still classified at runtime (by status range) — the list makes the
	// documented ones typed, it does not pretend to be exhaustive.
	Errors []ErrorSpec `yaml:"errors,omitempty" json:"errors,omitempty"`

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

// HTTPBinding is where the call goes on the wire.
type HTTPBinding struct {
	// Method is upper-case ("GET", "POST", …).
	Method string `yaml:"method" json:"method"`
	// Path is the templated path under the connector's base URL and path
	// prefix, with `{name}` placeholders matching Params of In "path".
	Path string `yaml:"path" json:"path"`
	// Accept / ContentType are the negotiated media types when the vendor
	// declares something other than JSON.
	Accept      string `yaml:"accept,omitempty" json:"accept,omitempty"`
	ContentType string `yaml:"content_type,omitempty" json:"content_type,omitempty"`
}

// ParamIn is where a parameter's value is placed on the wire.
type ParamIn string

const (
	InPath   ParamIn = "path"
	InQuery  ParamIn = "query"
	InHeader ParamIn = "header"
	// InBody is a member of the JSON request body. Swagger 2.0's single
	// `in: body` parameter and OpenAPI 3's requestBody both flatten to a set
	// of InBody params, so a node author writes one flat map either way.
	InBody ParamIn = "body"
	// InForm is a multipart/form-data field (file uploads).
	InForm ParamIn = "form"
)

// Param is one input of an operation, already flattened out of the vendor's
// request layout.
type Param struct {
	// Name is the wire name; BodyPath is the dotted path inside the JSON body
	// when In is InBody and the member is nested.
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

	// SchemaRef names a shared schema (see Package.Schemas) when the value is
	// an object or an array of objects. Shared rather than inlined because a
	// large API's schemas are the bulk of a package's bytes and are reused by
	// dozens of operations.
	SchemaRef string `yaml:"schema_ref,omitempty" json:"schema_ref,omitempty"`

	// Secret marks a parameter whose value must never be logged, echoed into
	// an event, or shown to a model. A credential normally arrives through
	// the connection, not here — but some APIs take one as an argument.
	Secret bool `yaml:"secret,omitempty" json:"secret,omitempty"`
}

// Result is the operation's success shape.
type Result struct {
	// Status is the documented success status (200, 201, 204…).
	Status int `yaml:"status,omitempty" json:"status,omitempty"`
	// SchemaRef names the response schema; empty for an empty body.
	SchemaRef string `yaml:"schema_ref,omitempty" json:"schema_ref,omitempty"`
	// Array marks a response that is a bare array of SchemaRef.
	Array bool `yaml:"array,omitempty" json:"array,omitempty"`
	// Description is the vendor's own wording for the success case.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
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

// ErrorSpec is one documented failure of an operation.
type ErrorSpec struct {
	Status      int        `yaml:"status" json:"status"`
	Class       ErrorClass `yaml:"class" json:"class"`
	Description string     `yaml:"description,omitempty" json:"description,omitempty"`
	SchemaRef   string     `yaml:"schema_ref,omitempty" json:"schema_ref,omitempty"`
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
