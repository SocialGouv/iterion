package spec

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// OpsFile is one ops/<domain>.yaml: a slice of a connector's operations,
// grouped by the vendor's own tag so a 500-operation API stays readable and
// a diff shows which domain moved.
type OpsFile struct {
	SchemaVersion int         `yaml:"schema_version" json:"schema_version"`
	Connector     string      `yaml:"connector" json:"connector"`
	Domain        string      `yaml:"domain" json:"domain"`
	Operations    []Operation `yaml:"operations" json:"operations"`
}

// Schema is a pruned JSON-Schema-shaped description of one object. It is
// deliberately NOT the vendor's full JSON Schema: only what iterion needs to
// validate an argument, render a form and type a result survives, which is
// what keeps a package's size proportional to its API rather than to the
// vendor's documentation style.
type Schema struct {
	Type        string            `yaml:"type,omitempty" json:"type,omitempty"`
	Format      string            `yaml:"format,omitempty" json:"format,omitempty"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Enum        []string          `yaml:"enum,omitempty" json:"enum,omitempty"`
	Required    []string          `yaml:"required,omitempty" json:"required,omitempty"`
	Properties  map[string]Schema `yaml:"properties,omitempty" json:"properties,omitempty"`
	// Items describes an array's element; Ref names another entry of the
	// schemas map (recursion is expressed by name, never by nesting, so a
	// self-referential vendor type cannot blow up the encoder).
	Items *Schema `yaml:"items,omitempty" json:"items,omitempty"`
	Ref   string  `yaml:"ref,omitempty" json:"ref,omitempty"`
}

// Package is a whole connector package held in memory: the identity card, the
// operations of every domain, and the shared schemas. It is what the
// generator produces, what a resolver materialises, and what the executor and
// the MCP facade read — one in-memory shape for every consumer, so the two
// offers cannot drift apart.
type Package struct {
	Connector Connector
	Ops       []OpsFile
	Schemas   map[string]Schema
}

// Operations flattens every domain's operations, sorted by id, so callers
// that want the whole surface (the index, the deterministic catalog) do not
// each re-implement the walk.
func (p *Package) Operations() []Operation {
	var out []Operation
	for _, f := range p.Ops {
		out = append(out, f.Operations...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Operation finds one operation by id.
func (p *Package) Operation(id string) (Operation, bool) {
	for _, f := range p.Ops {
		for _, op := range f.Operations {
			if op.ID == id {
				return op, true
			}
		}
	}
	return Operation{}, false
}

// AuthScheme finds one declared auth scheme by id.
func (c *Connector) AuthScheme(id string) (AuthScheme, bool) {
	for _, a := range c.Auth {
		if a.ID == id {
			return a, true
		}
	}
	return AuthScheme{}, false
}

// EffectiveOutcome resolves the outcome policy an operation runs under: its
// own if it declares one, else the connector's. One resolver, because a
// reader that consulted only the operation would read a status-only policy
// for an API that signals failure in the body — the exact defect the policy
// exists to remove.
func (p *Package) EffectiveOutcome(op Operation) *OutcomePolicy {
	if op.Outcome != nil {
		return op.Outcome
	}
	return p.Connector.Outcome
}

// EffectiveSecurity resolves the requirements an operation authorizes under:
// its own, else the connector default. An operation marked Anonymous returns
// nil — the explicit "no credential needed", which is not the same as an
// operation that simply declared nothing.
func (p *Package) EffectiveSecurity(op Operation) []SecurityRequirement {
	if op.Anonymous {
		return nil
	}
	if len(op.Security) > 0 {
		return op.Security
	}
	return p.Connector.DefaultSecurity
}

// EffectiveMaturity resolves an operation's maturity against the package
// floor. The floor WINS when it is lower: a package nobody qualified cannot
// contain a qualified operation, so the clamp lives here rather than in each
// reader — one place to be right, and Validate refuses the contradiction
// up front so the clamp never has to be discovered at runtime.
func (p *Package) EffectiveMaturity(op Operation) Maturity {
	if op.Maturity == "" {
		return p.Connector.Maturity
	}
	if maturityRank(op.Maturity) > maturityRank(p.Connector.Maturity) {
		return p.Connector.Maturity
	}
	return op.Maturity
}

// maturityRank orders the levels for the clamp above. Deprecated ranks with
// experimental: it is still attachable, and it must never let an operation
// out-rank a package that is merely experimental.
func maturityRank(m Maturity) int {
	switch m {
	case MaturityQualified:
		return 3
	case MaturityDeprecated, MaturityExperimental:
		return 2
	case MaturitySpotted:
		return 1
	}
	return 0
}

// Validate is the COMPLETE check: a package that passes is one a launch can
// use. It is what the resolver, the publish endpoint and `iterion validate`
// run, so a package accepted at publish is one a launch can use, and one a
// launch refuses is one publish would have refused (the pluginsource
// discipline).
//
// It is deliberately stricter than ValidateGenerated. The extra demands —
// an auth scheme, an addressable base URL — are things a vendor description
// frequently does NOT carry: GitHub's own OpenAPI description, MIT-licensed
// and otherwise excellent, declares no security scheme anywhere (not at the
// root, not per operation, not in components; auth lives in its prose). So
// those belong to the authored overlay, and demanding them of a generator
// would make the single most important connector ungeneratable.
func (p *Package) Validate() error {
	c := &p.Connector
	if err := p.ValidateGenerated(); err != nil {
		return err
	}
	if len(c.Auth) == 0 {
		return fmt.Errorf("connector %q: declares no auth scheme — the vendor description carries none, so the overlay must supply one (an API nobody can authenticate against is not a connector)", c.ID)
	}
	authIDs := map[string]bool{}
	for _, a := range c.Auth {
		if strings.TrimSpace(a.ID) == "" {
			return fmt.Errorf("connector %q: an auth scheme has no id", c.ID)
		}
		if authIDs[a.ID] {
			return fmt.Errorf("connector %q: duplicate auth scheme %q", c.ID, a.ID)
		}
		authIDs[a.ID] = true
		if err := a.validate(c.ID); err != nil {
			return err
		}
	}
	if c.BaseURL.Default == "" && !c.BaseURL.OperatorSupplied {
		return fmt.Errorf("connector %q: no default base url and not operator-supplied — no call could be addressed", c.ID)
	}
	// The CONNECTOR-WIDE outcome predicate, which is the one most packages
	// actually carry: an API that reports failure inside a 200 does it
	// everywhere, so it is declared once here and overridden per operation.
	// Checked with the per-operation ones below, since either can be the
	// unparsable one.
	if err := validateOutcome("connector "+c.ID, c.Outcome); err != nil {
		return err
	}
	// Every security requirement must name a scheme the package declares.
	// Otherwise a binding is accepted at launch and the operation fails
	// mid-run against the vendor, which is the same defect as an unknown
	// schema ref — just deferred to a place where it costs a run.
	for _, req := range c.DefaultSecurity {
		if err := checkRequirement(c, req, "connector "+c.ID+" default_security"); err != nil {
			return err
		}
	}
	for _, f := range p.Ops {
		for _, op := range f.Operations {
			for _, req := range op.Security {
				if err := checkRequirement(c, req, "operation "+op.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkRequirement verifies a requirement's terms against the declared
// schemes, including that a named scope is one the scheme advertises when it
// advertises any at all. A required scope no scheme offers cannot be granted,
// so an operation asking for one can never be authorized.
func checkRequirement(c *Connector, req SecurityRequirement, where string) error {
	for _, t := range req.Terms {
		scheme, ok := c.AuthScheme(t.SchemeID)
		if !ok {
			return fmt.Errorf("%s: security names unknown auth scheme %q", where, t.SchemeID)
		}
		if len(scheme.SupportedScopes) == 0 {
			continue // the vendor advertises none; nothing to check against
		}
		supported := make(map[string]bool, len(scheme.SupportedScopes))
		for _, s := range scheme.SupportedScopes {
			supported[s] = true
		}
		for _, want := range t.Scopes {
			if !supported[want] {
				return fmt.Errorf("%s: requires scope %q, which scheme %q does not advertise", where, want, t.SchemeID)
			}
		}
	}
	return nil
}

// ValidateGenerated checks everything a GENERATOR can guarantee on its own:
// identity, and that every operation is internally coherent and addressable.
// It stops short of what only an overlay can supply (see Validate).
//
// The split is what keeps the generated/authored boundary honest in both
// directions: a generator that could satisfy the complete check would be
// making judgements it has no basis for, and a package that only satisfies
// this one must never reach a launch.
func (p *Package) ValidateGenerated() error {
	c := &p.Connector
	if c.SchemaVersion > SchemaVersion {
		return fmt.Errorf("connector %q: schema_version %d newer than supported %d (upgrade iterion)", c.ID, c.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("connector: missing id")
	}
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("connector %q: missing version", c.ID)
	}
	// The package level matters more than an operation's: EffectiveMaturity
	// falls back to it, so one typo here makes EVERY operation that declares
	// none unbindable, in silence.
	if !c.Maturity.Known() {
		return fmt.Errorf("connector %q: maturity %q is not one of spotted|experimental|qualified|deprecated", c.ID, c.Maturity)
	}

	seen := map[string]string{}
	for _, f := range p.Ops {
		for _, op := range f.Operations {
			if err := op.ValidateStandalone(c.ID, p.Schemas); err != nil {
				return err
			}
			if prev, dup := seen[op.ID]; dup {
				return fmt.Errorf("connector %q: duplicate operation id %q (in %s and %s)", c.ID, op.ID, prev, f.Domain)
			}
			seen[op.ID] = f.Domain
			// The clamp exists so a reader is always safe, but a package
			// that CONTRADICTS itself is an authoring mistake worth naming:
			// silently lowering it would let an overlay claim a maturity the
			// package never earned and nobody would ever see the claim fail.
			if op.Maturity != "" && maturityRank(op.Maturity) > maturityRank(c.Maturity) {
				return fmt.Errorf("connector %q: operation %q claims maturity %q above the package's %q", c.ID, op.ID, op.Maturity, c.Maturity)
			}
		}
	}
	return nil
}

func (a AuthScheme) validate(connector string) error {
	switch a.Kind {
	case AuthAPIKey:
		if a.Name == "" {
			return fmt.Errorf("connector %q auth %q: api_key needs a name", connector, a.ID)
		}
		if a.In != "header" && a.In != "query" {
			return fmt.Errorf("connector %q auth %q: api_key `in` must be header or query, got %q", connector, a.ID, a.In)
		}
	case AuthBearer, AuthBasic:
	case AuthOAuth2:
		if a.AuthURL == "" || a.TokenURL == "" {
			return fmt.Errorf("connector %q auth %q: oauth2 needs auth_url and token_url", connector, a.ID)
		}
	default:
		return fmt.Errorf("connector %q auth %q: unknown kind %q", connector, a.ID, a.Kind)
	}
	return nil
}

// ValidateStandalone checks ONE operation against its connector id and the
// package's shared schemas — everything that can be judged without seeing its
// siblings (uniqueness and the maturity clamp need the package; Validate adds
// them).
//
// It is exported because a GENERATOR must judge each operation as it derives
// it: a vendor description of any size carries a few malformed ones, and
// validating only the finished package would force the generator to lose
// every operation to the worst one.
func (op Operation) ValidateStandalone(connector string, schemas map[string]Schema) error {
	if !strings.HasPrefix(op.ID, connector+".") {
		return fmt.Errorf("connector %q: operation id %q must start with %q", connector, op.ID, connector+".")
	}
	// ADDRESSABLE, which is what this function's own contract claims and what
	// the prefix check alone does not give.
	//
	// An id's segments are derived from a VENDOR's description — the resource
	// from its first tag — and `snake` maps every separator to `_`, so a tag of
	// `"---"` yields an empty resource and `probe..list_things`, while a tag of
	// `"1"` yields `probe.1.list_things`. Both generate clean, validate clean
	// and are refused by the compiler (C261) at the only moment they could ever
	// be used, which is the worst of the three outcomes: work an operator paid
	// for that no workflow can call, with nothing saying so. An overlay pinning
	// an id by hand reaches here too.
	if !AddressableOperationID(op.ID) {
		return fmt.Errorf("operation %q: not addressable from a workflow — an id reads `connector.resource.verb` and every segment must start with a lowercase letter and hold only [a-z0-9_]", op.ID)
	}
	if op.HTTP.Method == "" || op.HTTP.Path == "" {
		return fmt.Errorf("operation %q: missing http method or path", op.ID)
	}
	if op.Effect == "" {
		return fmt.Errorf("operation %q: missing effect (read|create|update|delete)", op.ID)
	}
	// CLOSED enums, checked like the auth scheme's `kind` a few functions
	// over. An overlay is hand-written, and a typo there landed on the inert
	// side of every predicate: `maturity: qualifed` ranks 0, so the clamp
	// never trips and Attachable answers false — the operation is silently
	// unbindable while `connectors validate` reports the package green.
	// `effect: reed` fails safe (anything but `read` reads as mutating), but
	// it is refused for the same reason: a value nobody declared is a mistake,
	// not a level.
	if !op.Effect.Known() {
		return fmt.Errorf("operation %q: effect %q is not one of read|create|update|delete", op.ID, op.Effect)
	}
	if !op.Maturity.Known() {
		return fmt.Errorf("operation %q: maturity %q is not one of spotted|experimental|qualified|deprecated", op.ID, op.Maturity)
	}
	// Every `{placeholder}` in the path must have a path param, and every
	// path param must appear in the path. A mismatch is the failure that
	// otherwise surfaces as a request to a literally-templated URL.
	declared := map[string]bool{}
	keys := map[string]bool{}
	for _, prm := range op.Params {
		if prm.Name == "" {
			return fmt.Errorf("operation %q: a parameter has no name", op.ID)
		}
		// The public key is what a `.bot` writes; two parameters sharing one
		// would make the second unaddressable, which is silent — the first
		// value would simply be sent twice.
		if prm.Key == "" {
			return fmt.Errorf("operation %q: parameter %q has no key", op.ID, prm.Name)
		}
		if keys[prm.Key] {
			return fmt.Errorf("operation %q: two parameters share the key %q — one of them could never be addressed", op.ID, prm.Key)
		}
		keys[prm.Key] = true
		if prm.SchemaRef != "" && schemas != nil {
			if _, ok := schemas[prm.SchemaRef]; !ok {
				return fmt.Errorf("operation %q: parameter %q references unknown schema %q", op.ID, prm.Name, prm.SchemaRef)
			}
		}
		if prm.In == InPath {
			declared[prm.Name] = true
			if !strings.Contains(op.HTTP.Path, "{"+prm.Name+"}") {
				return fmt.Errorf("operation %q: path parameter %q does not appear in path %q", op.ID, prm.Name, op.HTTP.Path)
			}
			if !prm.Required {
				return fmt.Errorf("operation %q: path parameter %q must be required", op.ID, prm.Name)
			}
		}
	}
	for _, name := range pathPlaceholders(op.HTTP.Path) {
		if !declared[name] {
			return fmt.Errorf("operation %q: path %q has placeholder {%s} with no path parameter", op.ID, op.HTTP.Path, name)
		}
	}
	// A body with no declared encoding is a body the executor would have to
	// guess the bytes of: the same fields mean different requests as JSON, as
	// form-urlencoded and as multipart.
	if op.HasBodyParams() && op.HTTP.RequestBody == "" {
		return fmt.Errorf("operation %q: has body parameters but declares no request_body encoding (json|form|multipart)", op.ID)
	}
	// The encoding is checked whether or not body parameters SURVIVED, and
	// that is the point: an operation whose only media type is one iterion
	// cannot build reaches here with an encoding marker and no params, and
	// admitting it would publish an operation that silently drops its
	// payload and reports success.
	if op.HTTP.RequestBody != "" && !ValidBodyEncoding(op.HTTP.RequestBody) {
		return fmt.Errorf("operation %q: request body encoding %q is not one iterion can build (want json|form|multipart)", op.ID, op.HTTP.RequestBody)
	}
	seenStatus := map[int]bool{}
	for _, r := range op.Results {
		if seenStatus[r.Status] {
			return fmt.Errorf("operation %q: duplicate result status %d", op.ID, r.Status)
		}
		seenStatus[r.Status] = true
		if r.SchemaRef != "" && schemas != nil {
			if _, ok := schemas[r.SchemaRef]; !ok {
				return fmt.Errorf("operation %q: result %d references unknown schema %q", op.ID, r.Status, r.SchemaRef)
			}
		}
	}
	for _, req := range op.Security {
		if len(req.Terms) == 0 {
			return fmt.Errorf("operation %q: a security requirement has no terms (use anonymous: true for a public operation)", op.ID)
		}
		for _, t := range req.Terms {
			if strings.TrimSpace(t.SchemeID) == "" {
				return fmt.Errorf("operation %q: a security term names no scheme", op.ID)
			}
		}
	}
	// An idempotency key that names no parameter is worse than none: it would
	// license a retry the operation cannot actually make safe.
	if op.IdempotencyKeyParam != "" && !declaresParam(op, op.IdempotencyKeyParam) {
		return fmt.Errorf("operation %q: idempotency_key_param %q names no parameter", op.ID, op.IdempotencyKeyParam)
	}
	// A request body is ONE shape or the other: a value, or a set of members.
	// A mixture has no encoding — the builder would have to pick, and either
	// choice silently discards the rest.
	whole, memberCount := 0, 0
	for _, prm := range op.Params {
		if prm.In != InBody {
			continue
		}
		if prm.WholeBody {
			whole++
		} else {
			memberCount++
		}
	}
	if whole > 1 {
		return fmt.Errorf("operation %q: two parameters both claim to BE the request body", op.ID)
	}
	if whole == 1 && memberCount > 0 {
		return fmt.Errorf("operation %q: one parameter is the whole request body and %d others are members of it — a body is one shape or the other", op.ID, memberCount)
	}
	if err := op.validatePagination(); err != nil {
		return err
	}
	if err := validateOutcome(op.ID, op.Outcome); err != nil {
		return err
	}
	return nil
}

// validateOutcome parses the outcome predicate at VALIDATION time.
//
// It is the one execution-bearing field that was admitted unread. A predicate
// that does not parse fails at evaluation — which happens AFTER the request
// was sent, so an unparsable `success_when` on a mutating operation performs
// the mutation and then reports that iterion cannot tell whether it worked.
// The expression language is total and has no I/O, so parsing it here costs
// nothing and moves the discovery to where it is free.
//
// Only the SHAPE is checked. Whether the predicate is *right* about a vendor
// is not a question a parser can answer.
func validateOutcome(opID string, policy *OutcomePolicy) error {
	if policy == nil || strings.TrimSpace(policy.SuccessWhen) == "" {
		return nil
	}
	if _, err := expr.Parse(policy.SuccessWhen); err != nil {
		return fmt.Errorf("operation %q: success_when %q does not parse: %w", opID, policy.SuccessWhen, err)
	}
	return nil
}

// validatePagination refuses a declared walk that cannot find its collection.
//
// The check is possible because the generator already recorded the shape:
// ResultCase.Array says whether the success body IS the array. When it is not
// and no items_field names where the array lives, the walk would extract
// nothing from every page — and an empty page is how every style signals the
// end, so the run would receive an empty collection reported as COMPLETE. The
// package has the facts to refuse that here, which is the only place it costs
// nothing.
func (op Operation) validatePagination() error {
	p := op.Pagination
	if p == nil {
		return nil
	}
	if !ValidPaginationStyle(p.Style) {
		// The list is what this build WALKS, not what the model spells:
		// `link_header` is declared in the style enum and has no arm in
		// CallPaged, so naming it here as an acceptable value handed the
		// author of a refused package the very value that is refused.
		return fmt.Errorf("operation %q: pagination style %q is not one iterion can walk (want page_number|cursor|offset)", op.ID, p.Style)
	}
	// The parameter the walk ADVANCES THROUGH, by the same argument as
	// cursor_field just below: CallPaged writes the position only when the
	// parameter is named (`if p.CursorParam != ""`, `if p.PageParam != ""`),
	// so without it every iteration re-sends an identical request. The vendor
	// answers page one each time with the same non-empty cursor, nothing
	// short-circuits, and the walk runs its full bound: `max_pages` identical
	// requests, page one's items appended that many times, `complete=false`.
	// A node that acts once per item then acts twenty times on each, and
	// twenty of the vendor's slots are spent per execution.
	//
	// And it must name a parameter the operation DECLARES, by the same
	// argument that cross-checks idempotency_key_param above: CallPaged writes
	// the position into the very map checkParams validates strictly, so a name
	// that is not declared makes every paginated call fail with "operation X
	// does not declare Y" — a refusal that blames the caller for a key the
	// package wrote and the caller cannot see.
	switch p.Style {
	case PageCursor:
		if p.CursorParam == "" {
			return fmt.Errorf("operation %q: cursor pagination declares no cursor_param, so every page of the walk would re-send an identical request", op.ID)
		}
		if !declaresParam(op, p.CursorParam) {
			return fmt.Errorf("operation %q: cursor_param %q names no parameter, so every paginated call would be refused for a key the caller never wrote", op.ID, p.CursorParam)
		}
	case PageNumber, PageOffset:
		if p.PageParam == "" {
			return fmt.Errorf("operation %q: %s pagination declares no page_param, so every page of the walk would re-send an identical request", op.ID, p.Style)
		}
		if !declaresParam(op, p.PageParam) {
			return fmt.Errorf("operation %q: page_param %q names no parameter, so every paginated call would be refused for a key the caller never wrote", op.ID, p.PageParam)
		}
	}
	if p.SizeParam != "" && !declaresParam(op, p.SizeParam) {
		return fmt.Errorf("operation %q: size_param %q names no parameter, so every paginated call would be refused for a key the caller never wrote", op.ID, p.SizeParam)
	}
	if p.Style == PageCursor && p.CursorField == "" {
		return fmt.Errorf("operation %q: cursor pagination declares no cursor_field, so the walk could never advance past page one", op.ID)
	}
	if p.ItemsField != "" {
		return nil
	}
	// No items_field: every success case must BE the array.
	for _, r := range op.Results {
		if r.Status < 200 || r.Status > 299 || r.Pending {
			continue
		}
		if r.SchemaRef == "" && r.Status == http.StatusNoContent {
			continue
		}
		if !r.Array {
			return fmt.Errorf("operation %q: declares pagination but its %d response (%s) is not an array and no items_field names the collection — the walk would read every page as empty and report the result complete",
				op.ID, r.Status, resultShapeName(r))
		}
	}
	return nil
}

// resultShapeName describes a result for the diagnostic above.
func resultShapeName(r ResultCase) string {
	if r.SchemaRef == "" {
		return "an empty body"
	}
	return "schema " + r.SchemaRef
}

// pathPlaceholders extracts the `{name}` templates of a path.
func pathPlaceholders(path string) []string {
	var out []string
	for i := 0; i < len(path); i++ {
		if path[i] != '{' {
			continue
		}
		end := strings.IndexByte(path[i:], '}')
		if end < 0 {
			break
		}
		out = append(out, path[i+1:i+end])
		i += end
	}
	return out
}

// declaresParam reports whether op declares a parameter under this name.
//
// Shared rather than repeated: three block-level fields (idempotency_key_param,
// and pagination's position and size parameters) name a parameter the request
// builder will write, and each one that is not cross-checked here becomes a
// refusal at call time blaming the caller for a key the package wrote. A
// second copy of this loop is where the fourth such field would drift.
//
// Both spellings count: a package may name the KEY (the DSL-facing name) or the
// wire NAME, and the executor resolves either.
func declaresParam(op Operation, name string) bool {
	for _, prm := range op.Params {
		if prm.Name == name || prm.Key == name {
			return true
		}
	}
	return false
}

// AddressableOperationID reports whether a `.bot` could name this operation.
//
// The rule is the COMPILER's, mirrored here because the two ends of the
// contract live in packages that must not depend on each other: `pkg/dsl/ir`
// is the DSL compiler and `pkg/connector/spec` is a leaf the generator reads,
// so neither may import the other. `ir.TestTheIDRuleMatchesTheConnectorSpec`
// is what keeps the two copies from drifting — an id this accepts and the
// compiler refuses is an operation nobody can ever call.
func AddressableOperationID(id string) bool {
	parts := strings.Split(id, ".")
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts {
		if !ValidIDSegment(p) {
			return false
		}
	}
	return true
}

// ValidIDSegment reports whether s can be ONE segment of an operation id: a
// lowercase letter first, then letters, digits or underscores.
//
// Exported for the GENERATOR, which derives a segment from a vendor's tag and
// must fall back rather than emit one no workflow can address.
func ValidIDSegment(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r == '_' || (r >= '0' && r <= '9'):
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return s != ""
}
