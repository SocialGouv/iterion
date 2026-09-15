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
	// pending names a component whose contract is being built, so a recursive
	// schema terminates instead of re-entering itself.
	pending map[string]bool
}

// attachResponseContracts derives a v2 contract for every success response it
// can represent, writes the reference onto the result case, and reports the
// rest.
func attachResponseContracts(data []byte, format Format, pkg *spec.Package) (map[string]spec.ResponseSchema, []Uncontracted, error) {
	doc, err := decodeExact(data)
	if err != nil {
		return nil, nil, err
	}
	g := &contractGen{doc: doc, format: format, contracts: map[string]spec.ResponseSchema{}, pending: map[string]bool{}}
	var uncontracted []Uncontracted
	paths := mapAt(doc, "paths")
	for fi := range pkg.Ops {
		ops := pkg.Ops[fi].Operations
		for oi := range ops {
			op := &ops[oi]
			item := mapAt(paths, op.HTTP.Path)
			method := mapAt(item, strings.ToLower(op.HTTP.Method))
			responses := mapAt(method, "responses")
			for ri := range op.Results {
				result := &op.Results[ri]
				// The reader refuses a contract on a status that carries no
				// body, so generation must not offer one either.
				if result.Status == 204 || result.Status == 205 || op.HTTP.Method == "HEAD" {
					continue
				}
				name, reason := g.contractFor(op.ID, result.Status, responses)
				switch {
				case reason != "":
					uncontracted = append(uncontracted, Uncontracted{OperationID: op.ID, Status: result.Status, Reason: reason})
				case name != "":
					result.ResponseSchemaRef = name
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
func (g *contractGen) contractFor(opID string, status int, responses map[string]any) (string, string) {
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
		schema = mapAt(mapAt(mapAt(rm, "content"), "application/json"), "schema")
	}
	if len(schema) == 0 {
		return "", ""
	}
	// A response that is exactly a reference borrows the component's contract
	// rather than wrapping it: one contract per shape keeps responses.json a
	// diff a reviewer can read.
	if ref := str(schema, "$ref"); ref != "" && onlyDescriptiveSiblings(schema) {
		name, err := g.component(ref, 0)
		if err != nil {
			return "", err.Error()
		}
		return name, ""
	}
	name := opID + "#" + strconv.Itoa(status)
	if _, taken := g.contracts[name]; taken {
		return "", "a component of the same name already holds this contract slot"
	}
	built, err := g.schema(schema, 0)
	if err != nil {
		return "", err.Error()
	}
	g.contracts[name] = built
	return name, ""
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
	if _, done := g.contracts[name]; done {
		return name, nil
	}
	if g.pending[name] {
		// Already being built higher in this walk: the reference closes a
		// recursive shape, which the vocabulary represents natively.
		return name, nil
	}
	g.pending[name] = true
	built, err := g.schema(resolved, depth+1)
	delete(g.pending, name)
	if err != nil {
		return "", err
	}
	g.contracts[name] = built
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
		if !contractKeys[key] {
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
		for _, member := range members {
			encoded, err := encodeScalar(member)
			if err != nil {
				return out, err
			}
			out.Enum = append(out.Enum, encoded)
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
		// every valid answer.
		if boolAt(mapAt(properties, name), "writeOnly") {
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
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	cur := any(g.doc)
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

func refKind(ref string) string {
	if strings.HasPrefix(ref, "#/") {
		return "reference to " + ref + ", which the description does not define"
	}
	return "remote reference (" + ref + ")"
}

// onlyDescriptiveSiblings reports whether a `$ref` node carries nothing that
// would change what the reference means.
func onlyDescriptiveSiblings(node map[string]any) bool {
	for key := range node {
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
