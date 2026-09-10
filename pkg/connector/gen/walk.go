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
			s.Scopes = sortedKeys(mapAt(f, "scopes"))
		} else {
			s.AuthURL = str(sm, "authorizationUrl")
			s.TokenURL = str(sm, "tokenUrl")
			s.Scopes = sortedKeys(mapAt(sm, "scopes"))
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
			generated := w.operation(path, method, op, shared)
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

func (w *walker) operation(path, method string, op map[string]any, shared []any) spec.Operation {
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

	params := append(append([]any{}, shared...), sliceAt(op, "parameters")...)
	for _, raw := range params {
		pm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out.Params = append(out.Params, w.params(pm)...)
	}
	if w.format == FormatOpenAPI3 {
		out.Params = append(out.Params, w.requestBodyParams(mapAt(op, "requestBody"))...)
	}
	out.Params = dedupParams(out.Params)

	out.Result, out.Errors = w.responses(mapAt(op, "responses"))
	return out
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
		return []spec.Param{w.scalarParam(pm, spec.InForm)}
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
	return p
}

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
		p := spec.Param{Name: "body", In: spec.InBody, Type: str(resolved, "type"), SchemaRef: refName}
		if p.Type == "" {
			p.Type = "object"
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

// requestBodyParams is the OpenAPI 3 half of the same flattening.
func (w *walker) requestBodyParams(rb map[string]any) []spec.Param {
	if len(rb) == 0 {
		return nil
	}
	if ref := str(rb, "$ref"); ref != "" {
		if r := w.resolveRef(ref); r != nil {
			rb = r
		}
	}
	content := mapAt(rb, "content")
	media := mapAt(content, "application/json")
	if len(media) == 0 {
		// A form body is the other shape iterion can place on the wire; any
		// other media type is left to the overlay rather than mis-encoded.
		if form := mapAt(content, "multipart/form-data"); len(form) > 0 {
			out := w.bodyParams(mapAt(form, "schema"))
			for i := range out {
				out[i].In = spec.InForm
			}
			return out
		}
		return nil
	}
	// `required: true` on the body itself is NOT propagated onto its members:
	// it says the envelope must be sent, while which members are mandatory is
	// the schema's own `required` list, which bodyParams already read.
	return w.bodyParams(mapAt(media, "schema"))
}

// dedupParams keeps the first declaration of each (in, name). A path item's
// shared parameters and an operation's own list legitimately overlap, and the
// operation's is the one that was written for it.
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

// responses splits a response map into the success shape and the documented
// failures. The lowest 2xx is the success: a vendor listing both 200 and 201
// describes one operation whose normal answer is the first of them.
func (w *walker) responses(resp map[string]any) (spec.Result, []spec.ErrorSpec) {
	var result spec.Result
	var errs []spec.ErrorSpec
	best := 0
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
			if best != 0 && status >= best {
				continue
			}
			best = status
			result = w.result(status, rm)
		case status >= 400:
			errs = append(errs, spec.ErrorSpec{
				Status:      status,
				Class:       spec.ClassifyStatus(status),
				Description: firstLine(str(rm, "description")),
			})
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Status < errs[j].Status })
	return result, errs
}

func (w *walker) result(status int, rm map[string]any) spec.Result {
	out := spec.Result{Status: status, Description: firstLine(str(rm, "description"))}
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
