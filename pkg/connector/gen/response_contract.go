package gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	exact "gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// Uncontracted records one success response that could NOT be given a v2
// response contract, and why.
//
// A response with no contract is not a defect — it is the honest outcome for a
// shape this closed vocabulary cannot represent. What would be a defect is
// emitting a contract that checks a fraction of what the vendor declared: the
// operator would believe the answer is validated. So the reason is reported
// per operation and status, and the response keeps its historical reading.
type Uncontracted struct {
	OperationID string
	Status      int
	Reason      string
}

// Contract generation bounds. The reader enforces its own limits; these stop a
// pathological description before it becomes a package nobody can load.
const (
	maxGeneratedContracts = 1024
	maxExactNodes         = 2_000_000
	maxContractDepth      = 64
)

// contractKeys is the CLOSED vocabulary of schema keywords generation
// understands. Anything outside it stops the contract.
//
// The allow-list is the whole design. The alternative — ignore what we do not
// model — is lenient in the safe direction for a bound like `minimum`, and
// silently empty for `allOf`: a schema that is nothing but composition becomes
// a contract that accepts everything, under a name that promises otherwise.
// And it never converges: each unrecognised keyword found in the wild is one
// more spelling to decide about. Refusing by default has no list to grow.
var contractKeys = map[string]bool{
	// Modelled.
	"$ref": true, "type": true, "nullable": true, "x-nullable": true,
	"required": true, "properties": true, "items": true, "enum": true,
	// Descriptive only: they constrain nothing a response must satisfy, so
	// ignoring them cannot make a contract refuse a body the vendor may send.
	"description": true, "title": true, "summary": true, "example": true,
	"examples": true, "externalDocs": true, "xml": true, "deprecated": true,
	"format": true, "default": true, "readOnly": true, "writeOnly": true,
}

// decodeExact re-reads the description with every number preserved VERBATIM.
//
// Separate from decode() on purpose: the legacy tree is float64, which is the
// right projection for the historical Data path and the wrong one for a
// contract — 9007199254740993 and ...992 are the same float64, so an enum
// derived through it would accept a value the vendor excluded. The generic
// decoder is left exactly as it was; this is a second pass over the same bytes.
func decodeExact(data []byte) (map[string]any, error) {
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var out map[string]any
		if err := dec.Decode(&out); err != nil {
			return nil, fmt.Errorf("gen: parse JSON description exactly: %w", err)
		}
		return out, nil
	}
	var root exact.Node
	if err := exact.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("gen: parse YAML description exactly: %w", err)
	}
	budget := maxExactNodes
	v, err := exactValue(&root, &budget)
	if err != nil {
		return nil, err
	}
	out, _ := v.(map[string]any)
	return out, nil
}

// exactValue converts a yaml.v3 node tree, keeping every scalar's SOURCE text
// for numbers. The budget is what stops an alias expansion from deciding this
// process's memory.
func exactValue(n *exact.Node, budget *int) (any, error) {
	if n == nil {
		return nil, nil
	}
	*budget--
	if *budget < 0 {
		return nil, fmt.Errorf("gen: description exceeds the exact-parse node budget")
	}
	switch n.Kind {
	case exact.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return exactValue(n.Content[0], budget)
	case exact.AliasNode:
		return exactValue(n.Alias, budget)
	case exact.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, err := exactValue(n.Content[i], budget)
			if err != nil {
				return nil, err
			}
			value, err := exactValue(n.Content[i+1], budget)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(key)] = value
		}
		return out, nil
	case exact.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := exactValue(c, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	switch n.Tag {
	case "!!null":
		return nil, nil
	case "!!bool":
		return n.Value == "true" || n.Value == "True" || n.Value == "TRUE", nil
	case "!!int", "!!float":
		// The SOURCE text, not a parsed value: that is the whole point.
		return json.Number(n.Value), nil
	}
	return n.Value, nil
}

// contractGen derives response contracts for a generated package.
type contractGen struct {
	doc       map[string]any
	format    Format
	contracts map[string]spec.ResponseSchema
	// origin remembers the $ref each contract name was built from. The name is
	// the reference's LAST SEGMENT, which is not unique: a description carrying
	// both #/definitions/Thing and #/components/schemas/Thing — an ordinary
	// residue of a Swagger 2 → OAS 3 conversion — would otherwise merge two
	// different shapes under one contract, silently, and refuse the body the
	// vendor documents for whichever operation lost.
	origin map[string]string
	// staged lists the component contracts published during the CURRENT
	// top-level build, so a failure can take them back out.
	//
	// Without it a component that failed on an unrepresentable keyword still
	// left behind the components it had already built, and any of those holding
	// a reference back to it (A → B → A, which is what `Issue.user` /
	// `User.issues` is) left a DANGLING reference in the map. ValidateGenerated
	// then failed the whole generation as "generated package is invalid" — a
	// message that blames the generator and tells the operator nothing — and
	// whether it happened depended on the alphabetical order of the properties.
	staged []string
}

// rollback removes the contracts published during a build that then failed, so
// a partial shape never outlives the attempt that produced it.
func (g *contractGen) rollback() {
	for _, name := range g.staged {
		delete(g.contracts, name)
		delete(g.origin, name)
	}
	g.staged = g.staged[:0]
}

// attachResponseContracts derives a v2 contract for every success response it
// can represent, writes the reference onto the result case, and reports the
// rest.
func attachResponseContracts(data []byte, format Format, pkg *spec.Package) (map[string]spec.ResponseSchema, []Uncontracted, error) {
	doc, err := decodeExact(data)
	if err != nil {
		return nil, nil, err
	}
	// The tree the OPERATIONS were built from, re-read here as the oracle for
	// what the description actually declares. The two decoders are different
	// libraries (the generic pass expands YAML merge keys, the exact pass walks
	// Nodes and does not), so "the exact re-read found no body" and "the vendor
	// declared none" are different facts that must not be confused.
	generic, err := decode(data)
	if err != nil {
		return nil, nil, err
	}
	g := &contractGen{
		doc: doc, format: format,
		contracts: map[string]spec.ResponseSchema{},
		origin:    map[string]string{},
	}
	var uncontracted []Uncontracted
	paths := mapAt(doc, "paths")
	genericPaths := mapAt(generic, "paths")
	for fi := range pkg.Ops {
		ops := pkg.Ops[fi].Operations
		for oi := range ops {
			op := &ops[oi]
			item := mapAt(paths, op.HTTP.Path)
			method := mapAt(item, strings.ToLower(op.HTTP.Method))
			responses := mapAt(method, "responses")
			genericMethod := mapAt(mapAt(genericPaths, op.HTTP.Path), strings.ToLower(op.HTTP.Method))
			genericResponses := mapAt(genericMethod, "responses")
			for ri := range op.Results {
				result := &op.Results[ri]
				// The reader refuses a contract on a status that carries no
				// body, so generation must not offer one either.
				if result.Status == 204 || result.Status == 205 || op.HTTP.Method == "HEAD" {
					continue
				}
				name, reason := g.contractFor(op.ID, result.Status, responses, method)
				switch {
				case reason != "":
					uncontracted = append(uncontracted, Uncontracted{OperationID: op.ID, Status: result.Status, Reason: reason})
				case name != "":
					result.ResponseSchemaRef = name
				case declaresResponseBody(generic, format, genericResponses, result.Status):
					// contractFor said "the vendor declared no body", and the
					// tree the operations came from says otherwise. Something in
					// the description reads differently under the two decoders —
					// a YAML merge key is one way, and it is not worth
					// enumerating the others. What matters is that the response
					// leaves with NO contract, so the operator who asked for
					// validation must not read a silent gap as a clean bill.
					uncontracted = append(uncontracted, Uncontracted{
						OperationID: op.ID, Status: result.Status,
						Reason: "the exact re-read of the description does not find the body the operation was built from",
					})
				}
			}
		}
	}
	if len(g.contracts) == 0 {
		return nil, uncontracted, nil
	}
	if len(g.contracts) > maxGeneratedContracts {
		return nil, nil, fmt.Errorf("gen: the description yields %d response contracts, over the limit of %d", len(g.contracts), maxGeneratedContracts)
	}
	sort.Slice(uncontracted, func(i, j int) bool {
		if uncontracted[i].OperationID != uncontracted[j].OperationID {
			return uncontracted[i].OperationID < uncontracted[j].OperationID
		}
		return uncontracted[i].Status < uncontracted[j].Status
	})
	return g.contracts, uncontracted, nil
}

// contractFor emits the contract for one status and returns the name to
// reference, or the reason no contract was emitted. Both empty means the
// vendor declared no body for this status, which is not a limitation.
func (g *contractGen) contractFor(opID string, status int, responses, method map[string]any) (string, string) {
	// Swagger 2 anchors the media type on the OPERATION, not on the response:
	// `schema: {type: string}` under `produces: [text/plain]` describes a RAW
	// body, and a contract there refuses every answer. OAS 3 needs no such check
	// because the schema is read from `content["application/json"]` already.
	//
	// Measured on the repository's reference description: eleven contracts sat
	// on `text/plain`, `text/html` and — plainly wrong — `application/zip`.
	if g.format == FormatSwagger2 {
		produces := sliceAt(method, "produces")
		if len(produces) == 0 {
			produces = sliceAt(g.doc, "produces")
		}
		if len(produces) > 0 && !anyJSONMedia(produces) {
			return "", "the operation does not produce JSON, so its body is not a shape a contract can state"
		}
	}
	rm := mapAt(responses, strconv.Itoa(status))
	if ref := str(rm, "$ref"); ref != "" {
		resolved, ok := g.resolve(ref)
		if !ok {
			return "", fmt.Sprintf("the response is a %s this generator does not resolve", refKind(ref))
		}
		rm = resolved
	}
	schema := mapAt(rm, "schema") // swagger 2
	if g.format == FormatOpenAPI3 {
		content := mapAt(rm, "content")
		schema = jsonContentSchema(content)
		// The vendor declared a body, just not one a contract can state. That
		// is the same case Swagger 2's `produces` gate reports, and saying
		// nothing here left the response with no contract AND no report line.
		if len(schema) == 0 && len(content) > 0 {
			return "", "the response declares no JSON media type, so its body is not a shape a contract can state"
		}
	}
	if len(schema) == 0 {
		return "", ""
	}
	// A response that is exactly a reference borrows the component's contract
	// rather than wrapping it: one contract per shape keeps responses.json a
	// diff a reviewer can read.
	// Every exit below goes through one of these two: a build that fails must
	// leave the map exactly as it found it.
	g.staged = g.staged[:0]
	if ref := str(schema, "$ref"); ref != "" && onlyDescriptiveSiblings(schema) {
		name, err := g.component(ref, 0)
		if err != nil {
			g.rollback()
			return "", err.Error()
		}
		if reason := g.readerRefuses(name); reason != "" {
			g.rollback()
			return "", reason
		}
		g.staged = g.staged[:0]
		return name, ""
	}
	name := opID + "#" + strconv.Itoa(status)
	if _, taken := g.contracts[name]; taken {
		return "", "a component of the same name already holds this contract slot"
	}
	built, err := g.schema(schema, 0)
	if err != nil {
		g.rollback()
		return "", err.Error()
	}
	g.contracts[name] = built
	g.staged = append(g.staged, name)
	if reason := g.readerRefuses(name); reason != "" {
		g.rollback()
		return "", reason
	}
	g.staged = g.staged[:0]
	return name, ""
}

// readerRefuses asks the READER whether the contract just built is one it will
// accept, and returns its reason when it will not.
//
// The generator's vocabulary check and the reader's admission check are two
// different lists, and whatever the second refuses that the first emitted comes
// back as "gen: generated package is invalid: …" — no package written at all,
// and the generator blamed for the vendor's data. A duplicate `required` name
// is one instance (legal JSON that no validator rejects); an over-long property
// name and the per-contract list and traversal ceilings are others. Asking the
// reader itself has no list to keep in step: it is the same code that will load
// the package, so a bound added there reports here instead of aborting.
func (g *contractGen) readerRefuses(name string) string {
	probe := &spec.Package{
		Connector:       spec.Connector{SchemaVersion: spec.ResponseContractsVersion, ID: "probe"},
		ResponseSchemas: g.contracts,
	}
	op := spec.Operation{
		ID:      "probe.contract.check",
		HTTP:    spec.HTTPBinding{Method: "GET", Path: "/"},
		Results: []spec.ResultCase{{Status: 200, ResponseSchemaRef: name}},
	}
	if err := probe.ValidateResponseContracts(op); err != nil {
		return "the contract this schema yields is one the reader refuses: " + err.Error()
	}
	return ""
}

// component builds (once) the contract for a local component schema and
// returns its name.
func (g *contractGen) component(ref string, depth int) (string, error) {
	resolved, ok := g.resolve(ref)
	if !ok {
		return "", fmt.Errorf("the schema is a %s this generator does not resolve", refKind(ref))
	}
	name := schemaNameFromRef(ref)
	if name == "" {
		return "", fmt.Errorf("the schema reference %q names nothing", ref)
	}
	// The name is only the last segment, so "already built" has to be checked
	// against the reference it was built FROM. Two sections holding the same
	// segment are two shapes, and reusing the first for the second refuses a
	// body the vendor documents — silently, since nothing in the package
	// records that a merge happened.
	if prev, known := g.origin[name]; known {
		if prev != ref {
			return "", fmt.Errorf("two schemas would share the contract name %q (%s and %s); a contract name must identify one shape", name, prev, ref)
		}
		if _, done := g.contracts[name]; done {
			return name, nil
		}
		// Already being built higher in this walk: the reference closes a
		// recursive shape, which the vocabulary represents natively.
		return name, nil
	}
	// Recorded BEFORE descending: that is what makes a recursive shape
	// terminate, since re-entry finds the name known and returns it above
	// instead of building it again.
	g.origin[name] = ref
	built, err := g.schema(resolved, depth+1)
	if err != nil {
		// The caller rolls the staged names back; this one never landed.
		delete(g.origin, name)
		return "", err
	}
	g.contracts[name] = built
	g.staged = append(g.staged, name)
	return name, nil
}

// schema converts one exact schema node into the closed vocabulary, or says
// why it cannot.
func (g *contractGen) schema(node map[string]any, depth int) (spec.ResponseSchema, error) {
	var out spec.ResponseSchema
	if depth > maxContractDepth {
		return out, fmt.Errorf("the schema nests deeper than %d levels", maxContractDepth)
	}
	for _, key := range sortedKeys(node) {
		if !contractKeys[key] && !isSpecExtension(key) {
			return out, fmt.Errorf("the schema uses %q, which a response contract does not represent", key)
		}
	}
	if ref := str(node, "$ref"); ref != "" {
		if !onlyDescriptiveSiblings(node) {
			return out, fmt.Errorf("the schema constrains a $ref with siblings, which a contract cannot carry")
		}
		name, err := g.component(ref, depth)
		if err != nil {
			return out, err
		}
		return spec.ResponseSchema{Ref: name}, nil
	}

	switch t := node["type"].(type) {
	case nil:
	case string:
		out.Type = t
	default:
		// OpenAPI 3.1 writes a nullable string as `type: [string, null]`. That
		// is a union, and a union is precisely what this vocabulary refuses to
		// approximate.
		return out, fmt.Errorf("the schema declares a union of types, which a contract cannot carry")
	}
	if out.Type != "" && !isContractType(out.Type) {
		return out, fmt.Errorf("the schema declares the type %q, which is not a JSON type", out.Type)
	}
	// Both spellings, because ignoring the vendor's means the contract REFUSES
	// a null the vendor documented — a false refusal is the one failure mode
	// worth engineering against here.
	if boolAt(node, "nullable") || boolAt(node, "x-nullable") {
		if out.Type == "" {
			return out, fmt.Errorf("the schema is nullable without a type, which constrains nothing")
		}
		out.Nullable = true
	}
	// Swagger 2 inherits JSON Schema draft-04, where `integer` is a LEXICAL
	// property of the token: 1.0 is not an integer. OpenAPI 3.0.4 settled the
	// opposite for OAS3 — the value is mathematical, so 1.0 and 1e3 are
	// integers. Same keyword, two dialects, and guessing either way silently
	// mis-validates a whole catalog.
	if out.Type == "integer" && g.format == FormatSwagger2 {
		out.IntegerMode = "token"
	}
	if raw, ok := node["enum"]; ok {
		members, ok := raw.([]any)
		if !ok || len(members) == 0 {
			return out, fmt.Errorf("the schema declares an enum that is not a non-empty list")
		}
		nullListed := false
		for _, member := range members {
			if member == nil {
				nullListed = true
			}
			if number, ok := member.(json.Number); ok {
				// The same rule the reader applies when a package is admitted,
				// asked here so the operator gets a reported response instead
				// of a generation that blames itself.
				if delivered, carried := spec.DeliveredNumber(number); !carried {
					return out, fmt.Errorf("the enum names %s, a value the decoded body delivers as %s, so a contract naming it would vouch for a number no run hands on", number, delivered)
				}
			}
			encoded, err := encodeScalar(member)
			if err != nil {
				return out, err
			}
			out.Enum = append(out.Enum, encoded)
		}
		// An enum is the exhaustive list of what the field may hold, and the
		// reader applies it to a null like any other value — deliberately, so
		// `enum: ["ok"]` cannot silently accept null. A vendor writing
		// `{type: string, enum: [open, closed], nullable: true}` — the
		// commonest shape in the wild, shipped by GitHub, Stripe and Forgejo —
		// means null IS permitted, so the contract has to say it in the only
		// place the reader looks.
		if out.Nullable && !nullListed {
			out.Enum = append(out.Enum, json.RawMessage("null"))
		}
		// And the same agreement read the other way. `{type: string, enum:
		// [open, closed, null]}` with no `nullable` is how JSON Schema and
		// OAS 3.1 say a field may be null, and a common Swagger→OAS conversion
		// residue. Carrying the member without the flag makes the type check
		// reject the null BEFORE the enum is consulted, so the contract holds a
		// member it can never accept — and refuses every answer where the
		// vendor sends one.
		if nullListed && out.Type != "" {
			out.Nullable = true
		}
	}

	properties := mapAt(node, "properties")
	if len(properties) > 0 {
		out.Properties = make(map[string]spec.ResponseSchema, len(properties))
		for _, name := range sortedKeys(properties) {
			child, ok := properties[name].(map[string]any)
			if !ok {
				return out, fmt.Errorf("the property %q declares no schema", name)
			}
			built, err := g.schema(child, depth+1)
			if err != nil {
				return out, err
			}
			out.Properties[name] = built
		}
	}
	for _, raw := range sliceAt(node, "required") {
		name, ok := raw.(string)
		if !ok {
			return out, fmt.Errorf("the schema lists a required property that is not a name")
		}
		// `writeOnly` means the vendor sends this field in a REQUEST and never
		// in a response. Copying it into the contract's required list would
		// demand a field the vendor must not send — a contract that refuses
		// EVERY valid answer for that operation.
		//
		// Read through one `$ref`: a `password` property is routinely written
		// as a reference to a component that carries the writeOnly, and testing
		// the property node alone sees nothing there.
		if g.writeOnlyProperty(mapAt(properties, name)) {
			continue
		}
		out.Required = append(out.Required, name)
	}

	if items, ok := node["items"]; ok {
		child, ok := items.(map[string]any)
		if !ok {
			// A tuple (`items: [A, B]`) is positional typing, which this
			// vocabulary does not carry.
			return out, fmt.Errorf("the schema declares tuple items, which a contract cannot carry")
		}
		built, err := g.schema(child, depth+1)
		if err != nil {
			return out, err
		}
		out.Items = &built
	}
	return out, nil
}

// resolve follows a LOCAL JSON pointer. A remote reference is refused rather
// than fetched: a parse must not become a network call.
func (g *contractGen) resolve(ref string) (map[string]any, bool) {
	return resolveIn(g.doc, ref)
}

// resolveIn follows one local JSON pointer inside any decoded description. It
// takes the tree as an argument because the exact re-read and the generic tree
// must be navigated by the SAME rules for a comparison between them to mean
// anything.
func resolveIn(doc map[string]any, ref string) (map[string]any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	cur := any(doc)
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	out, ok := cur.(map[string]any)
	return out, ok
}

// declaresResponseBody reports whether a decoded description shows a body
// schema for one status. It is contractFor's navigation with the emission
// removed, so the same question can be asked of either tree.
func declaresResponseBody(doc map[string]any, format Format, responses map[string]any, status int) bool {
	rm := mapAt(responses, strconv.Itoa(status))
	if ref := str(rm, "$ref"); ref != "" {
		resolved, ok := resolveIn(doc, ref)
		if !ok {
			return false
		}
		rm = resolved
	}
	schema := mapAt(rm, "schema")
	if format == FormatOpenAPI3 {
		schema = jsonContentSchema(mapAt(rm, "content"))
	}
	return len(schema) > 0
}

func refKind(ref string) string {
	if strings.HasPrefix(ref, "#/") {
		return "reference to " + ref + ", which the description does not define"
	}
	return "remote reference (" + ref + ")"
}

// writeOnlyProperty reports whether a property is response-absent, following a
// local reference once.
//
// One indirection is the whole of it: a `$ref` whose target is itself a `$ref`
// is a shape this vocabulary refuses anyway, so there is nothing further to
// chase — and a bounded read cannot loop on a cyclic description.
func (g *contractGen) writeOnlyProperty(node map[string]any) bool {
	if boolAt(node, "writeOnly") {
		return true
	}
	if ref := str(node, "$ref"); ref != "" {
		if target, ok := g.resolve(ref); ok {
			return boolAt(target, "writeOnly")
		}
	}
	return false
}

// anyJSONMedia reports whether a `produces` list contains a JSON media type.
// Matched on the structured suffix as well, so `application/vnd.api+json` and
// `application/problem+json` count — they are JSON bodies whatever the vendor
// registered them as.
// jsonContentSchema picks an OAS 3 response's JSON schema, preferring the exact
// media type and otherwise accepting the structured `+json` suffix.
//
// Matching only `application/json` left a response declared solely under
// `application/vnd.api+json` — or hal+json, or a vendor's own — with no
// contract AND no line in the report, because the divergence guard applies this
// same rule and so saw no body either. An operator then reads a silent gap as a
// clean bill. It also made the two dialects disagree: anyJSONMedia already
// accepts the suffix for Swagger 2's `produces`.
func jsonContentSchema(content map[string]any) map[string]any {
	if schema := mapAt(mapAt(content, "application/json"), "schema"); len(schema) > 0 {
		return schema
	}
	for _, media := range sortedKeys(content) {
		if isJSONMedia(media) {
			if schema := mapAt(mapAt(content, media), "schema"); len(schema) > 0 {
				return schema
			}
		}
	}
	return nil
}

// isJSONMedia reports whether a media type carries a JSON body, by the one rule
// both dialects use.
func isJSONMedia(media string) bool {
	media = strings.ToLower(strings.TrimSpace(media))
	if i := strings.IndexByte(media, ';'); i >= 0 {
		media = strings.TrimSpace(media[:i])
	}
	return media == "application/json" || strings.HasSuffix(media, "+json")
}

func anyJSONMedia(produces []any) bool {
	for _, raw := range produces {
		media, _ := raw.(string)
		if isJSONMedia(media) {
			return true
		}
	}
	return false
}

// isSpecExtension reports whether a key is an OpenAPI specification extension.
//
// Those are NON-NORMATIVE by definition — the specification reserves the `x-`
// prefix for data it promises carries no meaning to a consumer — so ignoring
// them is one rule read off the spec, not one more spelling on a list that
// grows every time another generator is met. That distinction is the whole
// reason this is safe to ignore where `allOf` is not: `allOf` constrains and we
// cannot represent it; `x-go-package` constrains nothing, anywhere, ever.
//
// Measured on the repository's reference description (Forgejo, swagger 2):
// 335 of 339 refused responses were refused for `x-go-package` alone, a
// go-swagger metadata tag. Without this the feature produced six usable
// contracts out of 521 declared results.
//
// An extension this generator DOES model — `x-nullable`, Swagger 2's spelling
// of `nullable` — is listed in contractKeys and is read before this ever runs.
func isSpecExtension(key string) bool { return strings.HasPrefix(key, "x-") }

// onlyDescriptiveSiblings reports whether a `$ref` node carries nothing that
// would change what the reference means.
func onlyDescriptiveSiblings(node map[string]any) bool {
	for key := range node {
		// `x-nullable` is Swagger 2's spelling of `nullable`, and this
		// generator MODELS it rather than ignoring it. Skipping it here as a
		// non-normative extension would drop the null the vendor documented
		// while the OAS 3 spelling beside the same `$ref` is refused — the two
		// dialects would disagree about one shape. Reported as uncontracted,
		// like `nullable`, because a reference carries no siblings: the
		// vocabulary cannot say "this reference may also be null".
		if key == "x-nullable" {
			return false
		}
		if isSpecExtension(key) {
			continue
		}
		switch key {
		case "$ref", "description", "title", "summary", "example", "examples",
			"externalDocs", "xml", "deprecated", "readOnly", "writeOnly":
		default:
			return false
		}
	}
	return true
}

func isContractType(t string) bool {
	switch t {
	case "object", "array", "string", "boolean", "number", "integer", "null":
		return true
	}
	return false
}

// encodeScalar re-encodes one enum member, keeping a number's source text.
func encodeScalar(v any) (json.RawMessage, error) {
	switch v.(type) {
	case nil, bool, string, json.Number:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("the schema declares an enum member that cannot be encoded")
		}
		return raw, nil
	}
	return nil, fmt.Errorf("the schema declares a non-scalar enum member, which a contract cannot compare")
}
