package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ResponseSchema is the closed response-contract vocabulary of package v2.
// It is intentionally independent of the pruned, descriptive Schema type.
// An omitted type imposes no type restriction; properties never imply object.
// Constraints not represented here cannot be silently copied into a contract.
type ResponseSchema struct {
	Type       string                    `json:"type,omitempty"`
	Nullable   bool                      `json:"nullable,omitempty"`
	Enum       []json.RawMessage         `json:"enum,omitempty"`
	Required   []string                  `json:"required,omitempty"`
	Properties map[string]ResponseSchema `json:"properties,omitempty"`
	Items      *ResponseSchema           `json:"items,omitempty"`
	Ref        string                    `json:"ref,omitempty"`
	// IntegerMode defaults to mathematical JSON integer values. "token"
	// preserves Swagger 2 / draft-04's lexical integer definition; unlike
	// the default it excludes a fraction or exponent in the response token.
	IntegerMode string `json:"integer_mode,omitempty"`
}

const (
	maxContractDepth = 64
	// maxContractNodes bounds ONE top-level contract's expansion, and is reset
	// for each. A counter shared by the whole package would not measure the
	// thing it exists to stop — a shape that expands without end — it would
	// measure how MANY contracts a package holds, which the document size cap
	// already bounds. The two limits would also contradict: maxContractList
	// admits 1024 contracts, so a shared 10000 affords about ten nodes each,
	// and an ordinary 25-property contract costs twenty-six. A merely large
	// description would fail to load, naming whichever contract sorted first.
	maxContractNodes  = 10000
	maxContractList   = 1024
	maxResponseVisits = 100000
	// maxResponseDepth bounds the DATA's nesting during validation, and is
	// separate from maxContractDepth on purpose: that one bounds the contract
	// GRAPH at pre-flight, where the memo stops at an already-checked component
	// and so measures a shorter path than the value walk takes. Sharing one
	// number made the walk refuse bodies the pre-flight had accepted — a
	// recursive contract spends two or three of it per level of real data, so
	// a tree nested past about twenty-five levels was refused. This ceiling is
	// far above any nesting an API returns and still low enough to be reached
	// deliberately, which is what keeps it a guard rather than a decoration.
	maxResponseDepth = 1000
)

// ValidateResponseContracts is the canonical package/pre-dispatch check.
// It deliberately does not consult the legacy descriptive Schemas map.
func (p *Package) ValidateResponseContracts(extra ...Operation) error {
	if p.Connector.SchemaVersion > SchemaVersion {
		return fmt.Errorf("connector response contracts require a newer reader: schema_version %d (supported %d)", p.Connector.SchemaVersion, SchemaVersion)
	}
	used := len(p.ResponseSchemas) > 0
	operations := append([]Operation(nil), extra...)
	for _, f := range p.Ops {
		operations = append(operations, f.Operations...)
	}
	for _, op := range operations {
		for _, result := range op.Results {
			if result.ResponseSchemaRef == "" {
				continue
			}
			used = true
			if result.Status < 200 || result.Status >= 300 || result.Status == 204 || result.Status == 205 || op.HTTP.Method == "HEAD" {
				return fmt.Errorf("operation %q: status %d cannot declare a response body contract", op.ID, result.Status)
			}
			if _, ok := p.ResponseSchemas[result.ResponseSchemaRef]; !ok {
				return fmt.Errorf("operation %q: response status %d references missing response contract %q", op.ID, result.Status, result.ResponseSchemaRef)
			}
		}
	}
	if !used {
		return nil
	}
	if p.Connector.SchemaVersion < ResponseContractsVersion {
		return fmt.Errorf("response contracts require schema_version %d", ResponseContractsVersion)
	}
	if len(p.ResponseSchemas) > maxContractList {
		return fmt.Errorf("too many response contracts (maximum %d)", maxContractList)
	}
	// checked is shared so a component reached from several contracts is walked
	// once; the budget is not, so a package pays per contract for its own shape
	// and never for its siblings'.
	checked := map[string]bool{}
	for _, name := range responseKeys(p.ResponseSchemas) {
		if checked[name] {
			continue
		}
		if len(name) == 0 || len(name) > 512 {
			return fmt.Errorf("response contract name is empty or too long")
		}
		visits := 0
		if err := p.checkResponseSchema(p.ResponseSchemas[name], 0, 0, map[string]int{name: 0}, checked, &visits); err != nil {
			return fmt.Errorf("response contract %q: %w", name, err)
		}
		checked[name] = true
	}
	return nil
}

func responseKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (p *Package) checkResponseSchema(s ResponseSchema, depth, progress int, active map[string]int, checked map[string]bool, visits *int) error {
	*visits++
	if depth > maxContractDepth || *visits > maxContractNodes {
		return fmt.Errorf("contract exceeds the depth or traversal limit")
	}
	if s.Ref != "" {
		if s.Type != "" || s.Nullable || s.Enum != nil || len(s.Required) > 0 || len(s.Properties) > 0 || s.Items != nil || s.IntegerMode != "" {
			return fmt.Errorf("a contract reference cannot carry sibling constraints")
		}
		target, ok := p.ResponseSchemas[s.Ref]
		if !ok {
			return fmt.Errorf("missing response contract reference %q", s.Ref)
		}
		if checked[s.Ref] {
			return nil
		}
		if at, exists := active[s.Ref]; exists {
			if progress <= at {
				return fmt.Errorf("reference cycle without a property or array item")
			}
			return nil
		}
		active[s.Ref] = progress
		err := p.checkResponseSchema(target, depth+1, progress, active, checked, visits)
		delete(active, s.Ref)
		if err == nil {
			checked[s.Ref] = true
		}
		return err
	}
	switch s.Type {
	case "", "object", "array", "string", "boolean", "number", "integer", "null":
	default:
		return fmt.Errorf("unsupported response type %q", s.Type)
	}
	if s.Nullable && s.Type == "" {
		return fmt.Errorf("nullable requires an explicit type")
	}
	if s.IntegerMode != "" && (s.Type != "integer" || s.IntegerMode != "token") {
		return fmt.Errorf("integer_mode is only supported as token on an integer")
	}
	if len(s.Properties) > maxContractList || len(s.Required) > maxContractList || len(s.Enum) > maxContractList {
		return fmt.Errorf("contract list exceeds the limit of %d", maxContractList)
	}
	if s.Enum != nil && len(s.Enum) == 0 {
		return fmt.Errorf("enum must not be empty")
	}
	for _, member := range s.Enum {
		v, err := decodeResponseJSON(member)
		if err != nil {
			return fmt.Errorf("enum contains an invalid JSON scalar")
		}
		if _, err := scalarIdentity(v); err != nil {
			return fmt.Errorf("enum contains an unsupported or oversized JSON scalar")
		}
		if number, ok := v.(json.Number); ok {
			if delivered, carried := DeliveredNumber(number); !carried {
				return fmt.Errorf("enum names the number %s, which a decoded body cannot carry: a workflow reading this field receives %s, so the contract would vouch for a value the run never delivers", number, delivered)
			}
		}
	}
	seen := map[string]bool{}
	for _, required := range s.Required {
		if len(required) > 512 || seen[required] {
			return fmt.Errorf("required contains a duplicate or oversized property name")
		}
		seen[required] = true
	}
	for _, name := range responseKeys(s.Properties) {
		if len(name) > 512 {
			return fmt.Errorf("property name exceeds the size limit")
		}
		if err := p.checkResponseSchema(s.Properties[name], depth+1, progress+1, active, checked, visits); err != nil {
			return err
		}
	}
	if s.Items != nil {
		return p.checkResponseSchema(*s.Items, depth+1, progress+1, active, checked, visits)
	}
	return nil
}

// ValidateResponse validates only an explicitly selected response contract.
// The validation decoder preserves exact numbers privately. This does not
// change the executor's historical Data projection or DSL arithmetic.
// An absent contract is reported separately from a JSON null or empty body.
func (p *Package) ValidateResponse(op Operation, status int, body []byte) (checked bool, err error) {
	var ref string
	for _, result := range op.Results {
		if result.Status == status {
			ref = result.ResponseSchemaRef
			break
		}
	}
	if ref == "" {
		return false, nil
	}
	schema, ok := p.ResponseSchemas[ref]
	if !ok {
		return true, fmt.Errorf("response contract is missing")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true, fmt.Errorf("response contract requires a JSON body; body is empty")
	}
	data, err := decodeResponseJSON(body)
	if err != nil {
		return true, fmt.Errorf("response contract requires one valid JSON value")
	}
	// The node budget is bounded by the body the transport ALREADY admitted: a
	// JSON tree cannot hold more nodes than it has bytes. A fixed ceiling
	// refused pages the vendor was allowed to send — measured, an ordinary
	// 2000-row page of sixty fields (1.2 MB) refused at row 1639, and on a
	// mutation that is a parked run rather than a failure a workflow can branch
	// on. maxResponseVisits stays as the floor for small bodies.
	walk := &responseWalk{budget: len(body)}
	if walk.budget < maxResponseVisits {
		walk.budget = maxResponseVisits
	}
	if err := p.validateResponseValue(schema, data, "$", 0, walk); err != nil {
		return true, err
	}
	return true, nil
}

func decodeResponseJSON(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("extra JSON value")
	}
	return v, nil
}

func responseViolation(path, reason string) error {
	// Paths contain only authored property names and array indexes, never
	// response values or unknown keys. Bound and quote them for diagnostics.
	if len(path) > 256 {
		path = path[:256] + "…"
	}
	return fmt.Errorf("response contract at %q: %s", path, reason)
}

// errWalkExhausted marks the budget running out, so the caller reports the
// traversal limit rather than blaming the package's enum.
var errWalkExhausted = errors.New("response walk budget exhausted")

// responseWalk carries the state of one body validation.
type responseWalk struct {
	// budget is a REMAINING node count, derived by the caller from the body the
	// transport already admitted. It is a termination backstop for a contract
	// shape that revisits value nodes, not a limit on how much a vendor returns.
	budget int
	// enums memoises each enum's scalar identities. Without it every value
	// compared re-decodes every member: O(values × members) JSON decodes on a
	// page the budget now lets run to the body's full length. The key is the
	// backing array of the member slice, which survives the copying of a
	// ResponseSchema value, so one enum is decoded once per body.
	enums map[*json.RawMessage]map[string]bool
}

// enumSet decodes an enum's members once and returns their identities. A member
// whose identity cannot be taken is left out rather than failing the body: the
// pre-flight check already refused such a contract, and a body is not the place
// to report a defect in the package validating it.
func (w *responseWalk) enumSet(members []json.RawMessage) (map[string]bool, error) {
	key := &members[0]
	if set, ok := w.enums[key]; ok {
		return set, nil
	}
	w.budget -= len(members)
	if w.budget < 0 {
		return nil, errWalkExhausted
	}
	set := make(map[string]bool, len(members))
	for _, raw := range members {
		member, err := decodeResponseJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("enum member is not one JSON scalar")
		}
		if id, err := scalarIdentity(member); err == nil {
			set[id] = true
		}
	}
	if w.enums == nil {
		w.enums = map[*json.RawMessage]map[string]bool{}
	}
	w.enums[key] = set
	return set, nil
}

// validateResponseValue walks the contract and the decoded body together.
//
// maxResponseDepth bounds the DATA's nesting, and is deliberately not
// maxContractDepth: that one bounds the contract graph at pre-flight, where the
// memo stops at an already-checked component and therefore measures a shorter
// path than this walk takes. Reusing it here refused bodies the pre-flight had
// accepted. This ceiling sits far above what encoding/json will decode, so it
// is a stack backstop rather than a bound on how deeply a vendor may nest.
func (p *Package) validateResponseValue(s ResponseSchema, value any, path string, depth int, walk *responseWalk) error {
	walk.budget--
	if depth > maxResponseDepth || walk.budget < 0 {
		return responseViolation(path, "validation traversal limit exceeded")
	}
	if s.Ref != "" {
		target, ok := p.ResponseSchemas[s.Ref]
		if !ok {
			return responseViolation(path, "contract reference is missing")
		}
		return p.validateResponseValue(target, value, path, depth+1, walk)
	}
	// A declared null on a nullable schema skips the type check; everything
	// else has to match the declared type.
	if s.Type != "" && (value != nil || !s.Nullable) {
		valid := false
		switch s.Type {
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "null":
			valid = value == nil
		case "number", "integer":
			if n, ok := value.(json.Number); ok {
				_, integer, err := normalizeResponseNumber(n)
				if err != nil {
					return responseViolation(path, "number exceeds validation limits")
				}
				valid = s.Type == "number" || integer
				if s.IntegerMode == "token" && strings.ContainsAny(n.String(), ".eE") {
					valid = false
				}
			}
		}
		if !valid {
			return responseViolation(path, "expected "+s.Type)
		}
	}
	// Deliberately NOT relaxed by Nullable: an enum is the exhaustive list of
	// what this field may hold, null included when the vendor allows it. A
	// contract that permits null alongside an enum says so by listing null as
	// a member — which is what the generator emits. Relaxing it here instead
	// would make `enum: ["ok"]` silently accept null on every nullable field.
	if s.Enum != nil {
		id, err := scalarIdentity(value)
		if err != nil {
			return responseViolation(path, "value is outside the declared scalar enum")
		}
		set, err := walk.enumSet(s.Enum)
		switch {
		case errors.Is(err, errWalkExhausted):
			return responseViolation(path, "validation traversal limit exceeded")
		case err != nil:
			return responseViolation(path, "enum contract is invalid")
		}
		if !set[id] {
			return responseViolation(path, "value is outside the declared scalar enum")
		}
	}
	if object, ok := value.(map[string]any); ok {
		for _, name := range s.Required {
			if _, exists := object[name]; !exists {
				return responseViolation(path+"."+name, "required property is absent")
			}
		}
		for _, name := range responseKeys(s.Properties) {
			if member, exists := object[name]; exists {
				if err := p.validateResponseValue(s.Properties[name], member, path+"."+name, depth+1, walk); err != nil {
					return err
				}
			}
		}
	}
	if array, ok := value.([]any); ok && s.Items != nil {
		for i, member := range array {
			if err := p.validateResponseValue(*s.Items, member, path+"["+strconv.Itoa(i)+"]", depth+1, walk); err != nil {
				return err
			}
		}
	}
	return nil
}

func scalarIdentity(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "null", nil
	case bool:
		return "boolean:" + strconv.FormatBool(v), nil
	case string:
		if len(v) > 65536 {
			return "", fmt.Errorf("scalar too large")
		}
		return "string:" + v, nil
	case json.Number:
		n, _, err := normalizeResponseNumber(v)
		return "number:" + n, err
	default:
		return "", fmt.Errorf("not a JSON scalar")
	}
}

// deliveredNumber reports whether a number a contract NAMES survives the
// projection a workflow actually receives, and what arrives in its place when
// it does not.
//
// Validation reads the body exactly; Result.Data is decoded through
// encoding/json's untyped float64. The two agree on almost every number and
// disagree past 2^53: a contract naming 9007199254740993 certifies the value
// the vendor sent while the node hands on ...992 — the very value that same
// contract refuses. A `.bot` chaining a delete or an update on that id acts on
// another object, with the contract's blessing.
//
// So an exact value the run cannot deliver is refused where a package is
// ADMITTED. Refusing at the call instead would turn a legitimate large id into
// a failed mutation with its body erased, which is the expensive direction; and
// certifying it would be an assurance false by construction. The type check is
// untouched — "an integer" stays true of both values, and only naming one of
// them is the claim that cannot hold.
func DeliveredNumber(number json.Number) (json.Number, bool) {
	f, err := number.Float64()
	if err != nil {
		return "", false
	}
	delivered := json.Number(strconv.FormatFloat(f, 'g', -1, 64))
	want, _, err := normalizeResponseNumber(number)
	if err != nil {
		return delivered, false
	}
	got, _, err := normalizeResponseNumber(delivered)
	if err != nil {
		return delivered, false
	}
	return delivered, want == got
}

// normalizeResponseNumber compares decimal values without float64 or expanding
// exponents into enormous integers. Equality is sign + coefficient + exponent.
func normalizeResponseNumber(number json.Number) (string, bool, error) {
	s := number.String()
	if len(s) > 4096 {
		return "", false, fmt.Errorf("number too large")
	}
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	exponent := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.Atoi(s[i+1:])
		if err != nil || exponent < -1000000 || exponent > 1000000 {
			return "", false, fmt.Errorf("exponent too large")
		}
		s = s[:i]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		exponent -= len(s) - i - 1
		s = s[:i] + s[i+1:]
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0", true, nil
	}
	trimmed := strings.TrimRight(s, "0")
	exponent += len(s) - len(trimmed)
	s = trimmed
	if negative {
		s = "-" + s
	}
	return s + "e" + strconv.Itoa(exponent), exponent >= 0, nil
}
