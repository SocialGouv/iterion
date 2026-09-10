package gen

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// walker holds one generation's state while it crosses the description.
//
// The two formats meet here rather than in two parsers. Everything that is
// the same in OpenAPI 3 and Swagger 2 — paths, methods, path/query/header
// parameters, tags, operation ids — is written once; the four places they
// genuinely differ (servers, request bodies, responses, security) each carry
// their own explicit branch, so the difference is readable instead of
// duplicated across two files that drift.
type walker struct {
	doc    map[string]any
	format Format
	opts   Options

	baseURL spec.BaseURL
	auth    []spec.AuthScheme
	schemas map[string]spec.Schema
	// ops is keyed by domain (the vendor's first tag), which becomes one
	// ops/<domain>.yaml file.
	ops map[string][]spec.Operation
	// usedIDs guards the derived-id namespace: a vendor's operation ids are
	// not always unique once normalised, and two operations sharing an id
	// would make one of them unaddressable.
	usedIDs map[string]bool
	// skipped collects the operations the description described badly enough
	// that iterion could not derive a callable one. See Report.
	skipped []Skip
	// authIDBySourceKey maps the vendor's own securityDefinitions key onto the
	// iterion scheme id derived from it. An operation's `security` block names
	// the VENDOR's key, while everything downstream stores iterion's — without
	// this translation a per-operation requirement could never be resolved.
	authIDBySourceKey map[string]string
	// defaultSecurity is the description's root-level `security`, applied to
	// every operation that declares none of its own.
	defaultSecurity []spec.SecurityRequirement
	// lastBodyMediaType carries the vendor's own media type out of
	// requestBody, when it differs from the canonical one for the encoding.
	// A field rather than a fifth return value: the walk is single-threaded
	// and one operation's body is read exactly once, immediately before it is
	// consumed.
	lastBodyMediaType string
}

func (w *walker) run() error {
	w.ops = map[string][]spec.Operation{}
	w.usedIDs = map[string]bool{}

	w.baseURL = w.readBaseURL()
	w.baseURL.OperatorSupplied = w.opts.OperatorSuppliedBaseURL
	// An empty auth list is NOT an error here. A vendor may document
	// authentication entirely in prose — GitHub's own MIT-licensed OpenAPI
	// description declares no security scheme at all — and the overlay is
	// where that judgement belongs. The generated package simply does not
	// pass the complete Validate until an overlay supplies one.
	w.auth = w.readAuth()
	// The root `security` is the default for every operation that declares
	// none. Read AFTER the schemes, since a requirement names a vendor key
	// that only readAuth can translate.
	if list, ok := w.doc["security"].([]any); ok {
		w.defaultSecurity = w.requirements(list)
	}
	w.readSchemas()
	return w.readPaths()
}

// readBaseURL resolves the API origin and path prefix. OpenAPI 3 carries a
// `servers` list of full URLs; Swagger 2 splits the same information across
// `schemes` + `host` + `basePath`. Both land on the same two fields, which is
// what lets a self-hosted connection supply only an origin.
func (w *walker) readBaseURL() spec.BaseURL {
	if w.format == FormatOpenAPI3 {
		for _, s := range sliceAt(w.doc, "servers") {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			raw := str(sm, "url")
			if raw == "" {
				continue
			}
			u, err := url.Parse(raw)
			if err != nil {
				continue
			}
			if u.Host == "" {
				// A relative server URL ("/api/v3") names a prefix only —
				// the host is whatever instance the operator connects to.
				return spec.BaseURL{PathPrefix: strings.TrimRight(u.Path, "/")}
			}
			return spec.BaseURL{
				Default:    u.Scheme + "://" + u.Host,
				PathPrefix: strings.TrimRight(u.Path, "/"),
			}
		}
		return spec.BaseURL{}
	}

	out := spec.BaseURL{PathPrefix: strings.TrimRight(str(w.doc, "basePath"), "/")}
	host := str(w.doc, "host")
	if host == "" {
		// Forgejo publishes no `host`: the description is served BY the
		// instance it describes, so the origin is the operator's.
		return out
	}
	scheme := "https"
	for _, s := range strSlice(w.doc, "schemes") {
		if s == "https" {
			scheme = s
			break
		}
		scheme = s
	}
	out.Default = scheme + "://" + host
	return out
}

// readAuth maps the description's security schemes onto iterion's own.
//
// The scheme's iterion id is DERIVED FROM ITS SHAPE, not copied from the
// vendor's key: a connection stores that id, and a vendor renaming
// `AuthorizationHeaderToken` between releases must not orphan every stored
// connection. Two schemes of the same shape are disambiguated by suffix.
func (w *walker) readAuth() []spec.AuthScheme {
	var raw map[string]any
	if w.format == FormatOpenAPI3 {
		raw = mapAt(mapAt(w.doc, "components"), "securitySchemes")
	} else {
		raw = mapAt(w.doc, "securityDefinitions")
	}

	seen := map[string]int{}
	w.authIDBySourceKey = map[string]string{}
	var out []spec.AuthScheme
	for _, key := range sortedKeys(raw) {
		sm, ok := raw[key].(map[string]any)
		if !ok {
			continue
		}
		s, ok := w.authScheme(key, sm)
		if !ok {
			continue
		}
		if n := seen[s.ID]; n > 0 {
			s.ID = fmt.Sprintf("%s_%d", s.ID, n+1)
		}
		seen[baseAuthID(s)]++
		w.authIDBySourceKey[key] = s.ID
		out = append(out, s)
	}
	return out
}

// baseAuthID is the shape-derived id before disambiguation.
func baseAuthID(s spec.AuthScheme) string {
	switch s.Kind {
	case spec.AuthBasic:
		return "basic"
	case spec.AuthOAuth2:
		return "oauth"
	case spec.AuthBearer:
		return "bearer"
	default:
		return "token"
	}
}

func (w *walker) authScheme(key string, sm map[string]any) (spec.AuthScheme, bool) {
	desc := firstLine(str(sm, "description"))
	switch strings.ToLower(str(sm, "type")) {
	case "apikey":
		s := spec.AuthScheme{
			ID:          "token",
			Kind:        spec.AuthAPIKey,
			DisplayName: key,
			Description: desc,
			In:          strings.ToLower(str(sm, "in")),
			Name:        str(sm, "name"),
		}
		// A vendor that documents a required word in front of the value
		// states it in prose and nowhere machine-readable. Forgejo's "API
		// tokens must be prepended with \"token\"" is the case in hand:
		// missing the prefix produces a 401 that looks like a bad token, so
		// the derivation is worth making, and the overlay corrects it when
		// the phrasing is unusual.
		if s.In == "header" && strings.EqualFold(s.Name, "authorization") {
			s.ValuePrefix = detectValuePrefix(str(sm, "description"))
			if s.ValuePrefix == "Bearer " {
				s.Kind = spec.AuthBearer
				s.ID = "bearer"
			}
		}
		if s.In == "" || s.Name == "" {
			return spec.AuthScheme{}, false
		}
		return s, true

	case "basic":
		return spec.AuthScheme{ID: "basic", Kind: spec.AuthBasic, DisplayName: key, Description: desc}, true

	case "http":
		switch strings.ToLower(str(sm, "scheme")) {
		case "bearer":
			return spec.AuthScheme{ID: "bearer", Kind: spec.AuthBearer, DisplayName: key, Description: desc}, true
		case "basic":
			return spec.AuthScheme{ID: "basic", Kind: spec.AuthBasic, DisplayName: key, Description: desc}, true
		}
		return spec.AuthScheme{}, false

	case "oauth2":
		s := spec.AuthScheme{ID: "oauth", Kind: spec.AuthOAuth2, DisplayName: key, Description: desc}
		if w.format == FormatOpenAPI3 {
			flows := mapAt(sm, "flows")
			// authorizationCode is the only flow iterion drives; implicit is
			// deprecated and the client-credentials/password grants are not a
			// per-user connection, which is what a connection models.
			f := mapAt(flows, "authorizationCode")
			s.AuthURL = str(f, "authorizationUrl")
			s.TokenURL = str(f, "tokenUrl")
			// What the vendor ADVERTISES. Copying it into DefaultScopes would
			// make every connection request every scope the API offers, which
			// is the opposite of least privilege — what a connection asks for
			// is decided by the overlay and the operations actually bound.
			s.SupportedScopes = sortedKeys(mapAt(f, "scopes"))
		} else {
			s.AuthURL = str(sm, "authorizationUrl")
			s.TokenURL = str(sm, "tokenUrl")
			s.SupportedScopes = sortedKeys(mapAt(sm, "scopes"))
		}
		if s.AuthURL == "" || s.TokenURL == "" {
			return spec.AuthScheme{}, false
		}
		return s, true
	}
	return spec.AuthScheme{}, false
}

// detectValuePrefix reads the word a vendor requires in front of an
// Authorization value out of its prose. Only the two forms that actually
// occur are recognised; anything else leaves the prefix empty for the
// overlay to state, rather than guessing from arbitrary wording.
func detectValuePrefix(description string) string {
	d := strings.ToLower(description)
	switch {
	case strings.Contains(d, "bearer"):
		return "Bearer "
	case strings.Contains(d, "prepended with \"token\""), strings.Contains(d, "token followed by a space"):
		return "token "
	}
	return ""
}

// readPaths walks every path and method into an operation.
func (w *walker) readPaths() error {
	paths := mapAt(w.doc, "paths")
	if len(paths) == 0 {
		return fmt.Errorf("gen: the description declares no path")
	}
	methods := []string{"get", "post", "put", "patch", "delete"}
	for _, path := range sortedKeys(paths) {
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		// Parameters declared on the PATH item apply to every method under
		// it. Dropping them would silently lose required path parameters,
		// which Validate then rejects as a placeholder with no parameter —
		// the failure would be real but the cause unreadable.
		shared := sliceAt(item, "parameters")
		for _, method := range methods {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			generated, dropped := w.operation(path, method, op, shared)
			// A REQUIRED parameter that could not be derived makes the
			// operation uncallable, and Validate cannot see it: what it
			// checks is the operation as DERIVED, which is coherent — it is
			// simply missing an input the vendor demands. So the skip is
			// decided here, where the loss is still known.
			if len(dropped) > 0 {
				w.usedIDs[generated.ID] = false
				delete(w.usedIDs, generated.ID)
				w.skipped = append(w.skipped, Skip{
					Path:              path,
					Method:            strings.ToUpper(method),
					SourceOperationID: generated.SourceOperationID,
					Reason: "a required parameter could not be derived (" + strings.Join(dropped, ", ") +
						") — publishing it would offer an operation no call can satisfy",
				})
				continue
			}
			// Validated ONE AT A TIME, here, rather than only as a package at
			// the end. A vendor description of any size carries a few
			// malformed operations — GitLab's declares a path parameter
			// `issue_id` on a path templated `{epic_issue_id}` — and the
			// choice is between losing that one with a reason and losing all
			// 1200 of them. The skip is not silent: it reaches the caller in
			// the Report.
			if err := generated.ValidateStandalone(w.opts.ConnectorID, w.schemas); err != nil {
				w.usedIDs[generated.ID] = false
				delete(w.usedIDs, generated.ID)
				w.skipped = append(w.skipped, Skip{
					Path:              path,
					Method:            strings.ToUpper(method),
					SourceOperationID: generated.SourceOperationID,
					Reason:            err.Error(),
				})
				continue
			}
			domain := generated.Resource
			w.ops[domain] = append(w.ops[domain], generated)
		}
	}
	if len(w.usedIDs) == 0 {
		return fmt.Errorf("gen: no operation could be derived from the description (%d were skipped as malformed)", len(w.skipped))
	}
	return nil
}

// operation derives one operation, and reports the REQUIRED parameters it had
// to drop. A non-empty second result means the operation cannot be called as
// derived, so the caller skips it rather than publishing a façade.
func (w *walker) operation(path, method string, op map[string]any, shared []any) (spec.Operation, []string) {
	var dropped []string
	tags := strSlice(op, "tags")
	sourceID := str(op, "operationId")
	resource, verb := deriveName(tags, sourceID, method, path)
	id := w.uniqueID(w.opts.ConnectorID+"."+resource+".", verb, path, resource)

	out := spec.Operation{
		ID:                id,
		SourceOperationID: sourceID,
		Summary:           firstLine(str(op, "summary")),
		Description:       firstLine(str(op, "description")),
		Resource:          resource,
		Verb:              verb,
		Tags:              tags,
		HTTP:              spec.HTTPBinding{Method: strings.ToUpper(method), Path: path},
		Effect:            effectFor(method, verb),
		// Everything generated is deterministic by construction: an HTTP call
		// with declared arguments and a declared result. An operation becomes
		// non-deterministic only when an overlay says so — a vendor endpoint
		// that runs a model, for instance.
		Deterministic: true,
	}

	// The OPERATION's own parameters come first, because dedupParams keeps the
	// first of a duplicate and both formats give the operation precedence: a
	// path item's parameters "can be overridden at the operation level"
	// (OpenAPI 3.0.3, Path Item Object). Listing the shared ones first would
	// silently invert that — the override would lose to what it overrides,
	// and a request would carry the wrong default with nothing to notice it.
	params := append(append([]any{}, sliceAt(op, "parameters")...), shared...)
	for _, raw := range params {
		pm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		got := w.params(pm)
		if len(got) == 0 && boolAt(pm, "required") {
			// A REQUIRED parameter was dropped — an unresolvable `$ref`, or a
			// location iterion cannot build (a cookie). Recorded here so the
			// caller can skip the whole operation.
			//
			// Erasing it published an operation that cannot be called, with
			// ZERO skips reported: exactly the invisible gap the skip
			// mechanism exists to make impossible. A dropped OPTIONAL
			// parameter only narrows the operation, so it still ships.
			dropped = append(dropped, paramLabel(pm))
		}
		out.Params = append(out.Params, got...)
	}
	// The body encoding. Swagger 2 states it as the operation's `consumes`
	// (inherited from the root) and carries body members either as one
	// `in: body` schema or as `in: formData` parameters; OpenAPI 3 states it
	// as the requestBody's media type. Both land on one BodyEncoding, because
	// the executor needs to know which bytes to build, not which format said
	// so.
	if w.format == FormatOpenAPI3 {
		body, encoding, undeclared := w.requestBody(mapAt(op, "requestBody"))
		if undeclared != "" {
			// A body iterion could not derive at all. Joins the same drop list
			// as a missing required parameter, since the consequence is the
			// same: an operation that would be published looking callable and
			// would send nothing where the vendor demands a payload.
			dropped = append(dropped, undeclared)
		}
		out.Params = append(out.Params, body...)
		out.HTTP.RequestBody = encoding
		out.HTTP.ContentType = w.lastBodyMediaType
	} else if enc := w.swaggerBodyEncoding(op); enc != "" {
		out.HTTP.RequestBody = enc
	}
	out.Params = sortParams(dedupParams(out.Params))
	// An encoding is only meaningful when there IS a body. Swagger inherits
	// `consumes` from the root, so every operation of an API that declares one
	// — a GET included — would otherwise carry `request_body: json`, which is
	// noise in every file and a claim that is simply untrue.
	//
	// Only a VALID encoding is cleared. An unsupported one must survive
	// exactly BECAUSE its members did not: that pair — a body iterion could
	// not read, and therefore no parameters — is what validation turns into a
	// coverage gap, and clearing it here would publish the operation with an
	// empty body instead.
	if !out.HasBodyParams() && spec.ValidBodyEncoding(out.HTTP.RequestBody) {
		out.HTTP.RequestBody = ""
	}
	assignParamKeys(out.Params)

	out.Results, out.Errors = w.responses(mapAt(op, "responses"))
	out.Security, out.Anonymous = w.security(op)
	// A FILE parameter is not something iterion can send yet. The multipart
	// writer produces a text field, so an upload arrived as a part containing
	// whatever string the argument held — a pathname, or base64 text — with no
	// filename and no file content. The vendor then rejects it, or worse
	// creates a corrupt attachment.
	//
	// Refused rather than published, by the rule this package already applies
	// to every other shape it cannot encode: an operation that LOOKS executable
	// and would send the wrong bytes is the failure the coverage gap exists to
	// prevent. Publishing it because "most of it works" is the façade.
	//
	// Supporting it needs an authorized file-input model — which paths a
	// workflow may read, and how that survives a sandbox — and that is a
	// decision, not an oversight.
	for _, p := range out.Params {
		if p.Type == "file" {
			dropped = append(dropped, "the file parameter "+p.Name+" (iterion cannot yet send a file part)")
		}
	}
	return out, dropped
}

// paramLabel names a parameter for a skip reason. A `$ref` that did not
// resolve has no name of its own, so the reference is what identifies it —
// and it is also what the reader has to go and look at.
func paramLabel(pm map[string]any) string {
	if n := str(pm, "name"); n != "" {
		if in := str(pm, "in"); in != "" {
			return n + " (in: " + in + ")"
		}
		return n
	}
	if ref := str(pm, "$ref"); ref != "" {
		return ref
	}
	return "an unnamed parameter"
}

// security reads an operation's own requirements. An EMPTY `security: []` is
// the explicit "this operation needs no credential" in both formats — which
// is different from declaring nothing, where the root default applies — so
// the two are returned apart rather than collapsed into a nil slice.
func (w *walker) security(op map[string]any) ([]spec.SecurityRequirement, bool) {
	raw, present := op["security"]
	if !present {
		return nil, false
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	if len(list) == 0 {
		return nil, true
	}
	return w.requirements(list), false
}

// requirements converts a format-level security list — an array of maps from
// scheme name to required scopes — into iterion's alternatives-of-conjunctions
// shape. A scheme the package did not derive is dropped: naming it would
// produce a requirement nothing could ever satisfy.
func (w *walker) requirements(list []any) []spec.SecurityRequirement {
	var out []spec.SecurityRequirement
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		var req spec.SecurityRequirement
		for _, key := range sortedKeys(m) {
			id, ok := w.authIDBySourceKey[key]
			if !ok {
				continue
			}
			term := spec.SecurityTerm{SchemeID: id}
			if scopes, ok := m[key].([]any); ok {
				for _, s := range scopes {
					if str, ok := s.(string); ok {
						term.Scopes = append(term.Scopes, str)
					}
				}
			}
			req.Terms = append(req.Terms, term)
		}
		if len(req.Terms) > 0 {
			out = append(out, req)
		}
	}
	return out
}

// swaggerBodyEncoding reads Swagger 2's `consumes`, falling back to the root's.
// The list is ordered by the vendor's preference, so the FIRST media type
// iterion can build wins — Slack advertises form-urlencoded before JSON, and
// its Web API is the form one.
func (w *walker) swaggerBodyEncoding(op map[string]any) spec.BodyEncoding {
	consumes := strSlice(op, "consumes")
	if len(consumes) == 0 {
		consumes = strSlice(w.doc, "consumes")
	}
	for _, mt := range consumes {
		if e := spec.NormalizeMediaType(mt); e != "" {
			return e
		}
	}
	// A Swagger operation with body members and no `consumes` anywhere means
	// JSON by the format's own default.
	return spec.BodyJSON
}

// assignParamKeys gives every parameter its public key — the name a `.bot`
// writes. It is the WIRE name whenever that is unambiguous, and the location
// plus the wire name when it is not: an operation legitimately carries `name`
// in its path and another `name` in its body, and one flat map cannot hold
// both. Path parameters keep the bare name in a conflict, since a `.bot`
// author reads those off the URL.
func assignParamKeys(params []spec.Param) {
	taken := make(map[string]bool, len(params))
	for i := range params {
		key := params[i].Name
		if taken[key] {
			key = string(params[i].In) + "_" + params[i].Name
		}
		for n := 2; taken[key]; n++ {
			key = fmt.Sprintf("%s_%s_%d", params[i].In, params[i].Name, n)
		}
		taken[key] = true
		params[i].Key = key
	}
}

// params flattens ONE declared parameter into iterion's flat list. Swagger
// 2.0's `in: body` parameter is not one input but a whole object, so it
// expands into one param per member: a node author writes a single flat map
// and never has to mirror the vendor's request envelope.
func (w *walker) params(pm map[string]any) []spec.Param {
	// A $ref'd parameter (a shared "page" definition, say) is resolved here
	// so its shape reaches the flat list like any inline one.
	if ref := str(pm, "$ref"); ref != "" {
		if resolved := w.resolveRef(ref); resolved != nil {
			pm = resolved
		}
	}
	in := strings.ToLower(str(pm, "in"))
	name := str(pm, "name")
	if name == "" && in != "body" {
		return nil
	}

	switch in {
	case "body":
		// Swagger 2.0 only.
		schema := mapAt(pm, "schema")
		return w.bodyParams(schema)
	case "formdata":
		// Swagger 2's formData is a BODY member: whether it goes on the wire
		// as form-urlencoded or as multipart is the operation's `consumes`,
		// not the parameter's location. Slack's entire Web API is this case.
		return []spec.Param{w.scalarParam(pm, spec.InBody)}
	case "path":
		p := w.scalarParam(pm, spec.InPath)
		p.Required = true
		return []spec.Param{p}
	case "query":
		return []spec.Param{w.scalarParam(pm, spec.InQuery)}
	case "header":
		return []spec.Param{w.scalarParam(pm, spec.InHeader)}
	}
	return nil
}

// scalarParam reads a non-body parameter. Swagger 2.0 puts the type on the
// parameter itself; OpenAPI 3 puts it in a nested `schema`.
func (w *walker) scalarParam(pm map[string]any, in spec.ParamIn) spec.Param {
	p := spec.Param{
		Name:        str(pm, "name"),
		In:          in,
		Required:    boolAt(pm, "required"),
		Description: firstLine(str(pm, "description")),
	}
	src := pm
	if s := mapAt(pm, "schema"); len(s) > 0 {
		src = s
		if ref := str(s, "$ref"); ref != "" {
			if r := w.resolveRef(ref); r != nil {
				src = r
			}
		}
	}
	p.Type = str(src, "type")
	p.Format = str(src, "format")
	p.Enum = enumStrings(src)
	p.Default = src["default"]
	if items := mapAt(src, "items"); len(items) > 0 {
		p.Items = str(items, "type")
	}
	if p.Type == "" {
		p.Type = "string"
	}
	p.Style, p.Explode = w.serialization(pm, in, p.Type)
	return p
}

// serialization resolves how a non-scalar value reaches the wire.
//
// The two formats say it differently and both say it explicitly: OpenAPI 3
// carries `style`/`explode`, Swagger 2 carries `collectionFormat`. Dropping
// it is not a documentation loss — `labels=[a,b]` reaches a vendor as
// `labels=a,b`, as `labels=a&labels=b` or as `labels=a%20b` depending on the
// answer, and only one of those is the one the vendor parses.
//
// A scalar gets no style: there is nothing to serialize, and recording one
// would add bytes to every parameter of every package for no meaning.
func (w *walker) serialization(pm map[string]any, in spec.ParamIn, typ string) (spec.ParamStyle, *bool) {
	if typ != "array" && typ != "object" {
		return "", nil
	}
	if style := str(pm, "style"); style != "" {
		var explode *bool
		if v, ok := pm["explode"].(bool); ok {
			explode = &v
		}
		return spec.ParamStyle(style), explode
	}
	if v, ok := pm["explode"].(bool); ok {
		return defaultStyleFor(in), &v
	}
	switch str(pm, "collectionFormat") {
	case "csv":
		return defaultStyleFor(in), boolPtr(false)
	case "ssv":
		return spec.StyleSpaceDelimited, boolPtr(false)
	case "pipes":
		return spec.StylePipeDelimited, boolPtr(false)
	case "multi":
		// Repeated `k=v` pairs — only meaningful in a query or a form.
		return spec.StyleForm, boolPtr(true)
	case "tsv":
		// Tab-separated has no OpenAPI 3 equivalent and no executor support;
		// leaving it unset would silently comma-join. Named as unsupported so
		// the operation becomes a coverage gap rather than a wrong request.
		return spec.ParamStyle("tsv"), boolPtr(false)
	}
	// Nothing was said, so the FORMAT's own default applies — and the two
	// formats disagree. Swagger 2's default collectionFormat is `csv`;
	// OpenAPI 3's default explode for a form parameter is `true`, i.e.
	// repeated pairs. Applying the OpenAPI default to a Swagger parameter
	// sent `labels=a&labels=b` where the vendor documented `labels=a,b`, and
	// a vendor that reads only the first pair then acts on half the list.
	if w.format == FormatSwagger2 {
		return defaultStyleFor(in), boolPtr(false)
	}
	return defaultStyleFor(in), nil
}

// defaultStyleFor is each location's default per OpenAPI 3.0.3.
func defaultStyleFor(in spec.ParamIn) spec.ParamStyle {
	switch in {
	case spec.InPath, spec.InHeader:
		return spec.StyleSimple
	default:
		return spec.StyleForm
	}
}

func boolPtr(b bool) *bool { return &b }

// bodyParams expands a request-body object into one param per member.
// A body that is not an object (an array, a bare string) cannot be flattened
// and becomes a single `body` param carrying its schema, so the operation
// stays callable instead of silently losing its payload.
func (w *walker) bodyParams(schema map[string]any) []spec.Param {
	if len(schema) == 0 {
		return nil
	}
	resolved := schema
	refName := ""
	if ref := str(schema, "$ref"); ref != "" {
		refName = schemaNameFromRef(ref)
		if r := w.resolveRef(ref); r != nil {
			resolved = r
		}
	}
	props := mapAt(resolved, "properties")
	if len(props) == 0 {
		// The body has no members to flatten: it IS a value — a bare array, a
		// scalar, or an object the description does not describe. Marked as
		// such so the builder sends the value itself. Without the marker the
		// invented `body` name reached the wire and an endpoint expecting
		// `[1,2]` received `{"body":[1,2]}`.
		p := spec.Param{
			Name: "body", In: spec.InBody, Type: str(resolved, "type"),
			SchemaRef: refName, WholeBody: true,
		}
		if p.Type == "" {
			p.Type = "object"
		}
		if items := mapAt(resolved, "items"); len(items) > 0 {
			if ref := str(items, "$ref"); ref != "" {
				p.SchemaRef = schemaNameFromRef(ref)
				p.Items = "object"
			} else if t := str(items, "type"); t != "" {
				p.Items = t
			}
		}
		return []spec.Param{p}
	}
	required := map[string]bool{}
	for _, r := range strSlice(resolved, "required") {
		required[r] = true
	}
	var out []spec.Param
	for _, name := range sortedKeys(props) {
		pm, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		p := spec.Param{
			Name:        name,
			In:          spec.InBody,
			Required:    required[name],
			Description: firstLine(str(pm, "description")),
			Type:        str(pm, "type"),
			Format:      str(pm, "format"),
			Enum:        enumStrings(pm),
			Default:     pm["default"],
		}
		if ref := str(pm, "$ref"); ref != "" {
			p.SchemaRef = schemaNameFromRef(ref)
			p.Type = "object"
		}
		if items := mapAt(pm, "items"); len(items) > 0 {
			p.Type = "array"
			p.Items = str(items, "type")
			if ref := str(items, "$ref"); ref != "" {
				p.SchemaRef = schemaNameFromRef(ref)
				p.Items = "object"
			}
		}
		if p.Type == "" {
			p.Type = "string"
		}
		out = append(out, p)
	}
	return out
}

// requestBody is the OpenAPI 3 half of the same flattening, and it returns
// the ENCODING alongside the members — because in OpenAPI 3 the media type is
// the key of the content map, so it is known exactly here and nowhere else.
//
// A media type iterion cannot build returns an empty encoding rather than an
// empty parameter list. The difference matters: no params means "this
// operation has no body", which is a callable operation, while an unsupported
// encoding must make the operation a COVERAGE GAP — an operation that looks
// executable but would drop its payload is the failure this package refuses.
func (w *walker) requestBody(rb map[string]any) ([]spec.Param, spec.BodyEncoding, string) {
	if len(rb) == 0 {
		return nil, "", ""
	}
	if ref := str(rb, "$ref"); ref != "" {
		r := w.resolveRef(ref)
		if r == nil {
			// An unresolvable reference used to leave `rb` as the bare
			// `{$ref: …}` map, whose `content` is empty — so the operation was
			// published as one with NO BODY, which is a perfectly callable
			// shape. A POST that requires a payload became a POST that sends
			// none, and reported no gap.
			return nil, "", "its request body is $ref " + ref + ", which the description does not define"
		}
		rb = r
	}
	content := mapAt(rb, "content")
	if len(content) == 0 {
		return nil, "", ""
	}
	// Sorted so a description offering several media types always resolves to
	// the same one; JSON is preferred where offered because it is the shape
	// with the least encoding ambiguity.
	var chosen string
	var encoding spec.BodyEncoding
	for _, mt := range sortedKeys(content) {
		e := spec.NormalizeMediaType(mt)
		if e == "" {
			continue
		}
		if encoding == "" || e == spec.BodyJSON {
			chosen, encoding = mt, e
		}
		if e == spec.BodyJSON {
			break
		}
	}
	if encoding == "" {
		// Every media type this body offers is one iterion cannot build.
		return nil, spec.BodyEncoding(unsupportedBodyMarker(content)), ""
	}
	// The vendor's OWN media type is remembered when it is not the canonical
	// one for the encoding. `application/json-patch+json` is JSON on the wire,
	// so the encoding is right, but it is not `application/json` — and a
	// vendor that declares one refuses the other at the header, which made an
	// operation iterion encodes perfectly fail before it was read.
	w.lastBodyMediaType = ""
	if chosen != canonicalMediaType(encoding) {
		w.lastBodyMediaType = chosen
	}
	// `required: true` on the body itself is NOT propagated onto its members:
	// it says the envelope must be sent, while which members are mandatory is
	// the schema's own `required` list, which bodyParams already read.
	return w.bodyParams(mapAt(mapAt(content, chosen), "schema")), encoding, ""
}

// canonicalMediaType is the type an encoding implies, so only a DIVERGENCE is
// recorded and every existing package stays byte-identical.
func canonicalMediaType(e spec.BodyEncoding) string {
	switch e {
	case spec.BodyJSON:
		return "application/json"
	case spec.BodyForm:
		return "application/x-www-form-urlencoded"
	case spec.BodyMultipart:
		return "multipart/form-data"
	}
	return ""
}

// unsupportedBodyMarker names the media types a body offered, so the skip
// reason says WHICH encoding was refused instead of "unsupported". The value
// is never a valid BodyEncoding, so validation rejects it — which is the
// mechanism that turns it into a coverage gap.
func unsupportedBodyMarker(content map[string]any) string {
	return "unsupported:" + strings.Join(sortedKeys(content), ",")
}

// dedupParams keeps the first declaration of each (in, name). A path item's
// shared parameters and an operation's own list legitimately overlap, and the
// operation's is the one that was written for it — which is why the caller
// lists the operation's first.
func dedupParams(in []spec.Param) []spec.Param {
	seen := map[string]bool{}
	out := in[:0]
	for _, p := range in {
		key := string(p.In) + ":" + p.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// sortParams gives the final list a presentation order independent of the
// order precedence needed above: by location (path, query, header, body,
// form) then by name. Without it the operation-first precedence rule would
// also reshuffle how a package READS — path parameters scattered after body
// members — and a package's diff would move for reasons that are not change.
func sortParams(in []spec.Param) []spec.Param {
	rank := map[spec.ParamIn]int{
		spec.InPath: 0, spec.InQuery: 1, spec.InHeader: 2, spec.InBody: 3,
	}
	sort.SliceStable(in, func(i, j int) bool {
		ri, rj := rank[in[i].In], rank[in[j].In]
		if ri != rj {
			return ri < rj
		}
		return in[i].Name < in[j].Name
	})
	return in
}

// responses splits a response map into the success shape and the documented
// failures. The lowest 2xx is the success: a vendor listing both 200 and 201
// describes one operation whose normal answer is the first of them.
func (w *walker) responses(resp map[string]any) ([]spec.ResultCase, []spec.ErrorSpec) {
	var results []spec.ResultCase
	var errs []spec.ErrorSpec
	for _, code := range sortedKeys(resp) {
		rm, ok := resp[code].(map[string]any)
		if !ok {
			continue
		}
		if ref := str(rm, "$ref"); ref != "" {
			if r := w.resolveRef(ref); r != nil {
				rm = r
			}
		}
		status := 0
		if _, err := fmt.Sscanf(code, "%d", &status); err != nil || status == 0 {
			continue // "default" and friends carry no status to classify by
		}
		switch {
		case status >= 200 && status < 300:
			// EVERY success variant is kept, not just the lowest. A vendor
			// answering 201 for a created resource and 202 for one queued
			// describes two different things, and a workflow that must tell
			// "done" from "accepted, not finished" can only do so if the
			// package still carries both.
			results = append(results, w.result(status, rm))
		case status >= 400:
			errs = append(errs, spec.ErrorSpec{
				Status:      status,
				Class:       spec.ClassifyStatus(status),
				Description: firstLine(str(rm, "description")),
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Status < results[j].Status })
	sort.Slice(errs, func(i, j int) bool { return errs[i].Status < errs[j].Status })
	return results, errs
}

func (w *walker) result(status int, rm map[string]any) spec.ResultCase {
	// 202 Accepted means the work was taken, not finished. Marking it is the
	// difference between a workflow acting on a completed change and acting
	// on one that may still fail somewhere the caller cannot see.
	out := spec.ResultCase{Status: status, Description: firstLine(str(rm, "description")), Pending: status == 202}
	schema := mapAt(rm, "schema") // swagger 2
	if w.format == FormatOpenAPI3 {
		schema = mapAt(mapAt(mapAt(rm, "content"), "application/json"), "schema")
	}
	if len(schema) == 0 {
		return out
	}
	if ref := str(schema, "$ref"); ref != "" {
		out.SchemaRef = schemaNameFromRef(ref)
		return out
	}
	if str(schema, "type") == "array" {
		items := mapAt(schema, "items")
		out.Array = true
		out.SchemaRef = schemaNameFromRef(str(items, "$ref"))
	}
	return out
}

// uniqueID guards the derived namespace, disambiguating a collision with the
// PATH rather than with a counter.
//
// The path is the operation's real identity, so a path-derived suffix is
// stable: it changes only when the endpoint itself does. A numeric one is
// not — `create_3` becomes `create_4` the day the vendor adds an operation
// that sorts earlier, silently breaking every `.bot` that quoted it. Which
// matters here because a large spec collides constantly: GitLab derives the
// same `boards.create_lists` for the group-scoped and the project-scoped
// endpoint, and 1844 operations produce hundreds of such pairs.
//
// A counter remains as the last resort, for two operations that differ in
// nothing a name can carry.
func (w *walker) uniqueID(prefix, verb, path, resource string) string {
	if id := prefix + verb; !w.usedIDs[id] {
		w.usedIDs[id] = true
		return id
	}
	if scope := pathScope(path, verb, resource); scope != "" {
		if id := prefix + verb + "_" + scope; !w.usedIDs[id] {
			w.usedIDs[id] = true
			return id
		}
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s%s_%d", prefix, verb, n)
		if !w.usedIDs[candidate] {
			w.usedIDs[candidate] = true
			return candidate
		}
	}
}

// opsFiles renders the per-domain files, sorted, so a regeneration of an
// unchanged spec is byte-identical.
func (w *walker) opsFiles() []spec.OpsFile {
	domains := make([]string, 0, len(w.ops))
	for d := range w.ops {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	out := make([]spec.OpsFile, 0, len(domains))
	for _, d := range domains {
		ops := w.ops[d]
		sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
		out = append(out, spec.OpsFile{
			SchemaVersion: spec.SchemaVersion,
			Connector:     w.opts.ConnectorID,
			Domain:        d,
			Operations:    ops,
		})
	}
	return out
}
