package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// decodeSchemaJSON is the single UTF-8 and JSON decoding choke point for raw
// tool schemas, including Raw assigned by hand. UseNumber preserves numeric
// lexemes. Decode enforces encoding/json's 10000-container depth limit before
// any recursive schema or order traversal, including for opaque instance data.
func decodeSchemaJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return v, nil
}

// normalizedSchemaJSON visits only schema positions, never const/default,
// examples/enum members or unknown extensions. Unchanged UTF-8 schemas keep
// their exact bytes. encoding/json replaces each invalid UTF-8 byte with U+FFFD.
func normalizedSchemaJSON(raw []byte) (any, json.RawMessage, error) {
	root := &schemaPath{}
	v, err := decodeSchemaJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	switch v.(type) {
	case map[string]any, bool, nil:
	default:
		return nil, nil, &RequestValidationError{Field: "InputSchema", Reason: "root must be a JSON object or boolean schema"}
	}
	cleaner := schemaCleaner{}
	v, changed := cleaner.clean(v, root, root, false, "")
	changed = cleaner.addItemTails() || changed
	changed = cleaner.rewriteReferences() || changed
	if !changed && utf8.Valid(raw) {
		return v, raw, nil
	}
	// Only rewritten schemas need object order. Rebuild from the validated
	// input so migration routes and duplicate-key values share the same tree.
	root = &schemaPath{}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err = decodeOrderedSchema(d, root)
	if err != nil {
		return nil, nil, err
	}
	cleaner = schemaCleaner{}
	v, _ = cleaner.clean(v, root, root, false, "")
	cleaner.addItemTails()
	cleaner.rewriteReferences()
	out, err := marshalOrderedSchema(v, root)
	return v, out, err
}
func cleanSchemaJSON(raw json.RawMessage) (json.RawMessage, error) {
	_, out, err := normalizedSchemaJSON(raw)
	return out, err
}

// Invalid schema members become {} (the historical permissive policy). Invalid
// keyword values are omitted: their intended constraint is unknowable and is
// LOST, including unsupported legacy vocabulary semantics such as a boolean
// $recursiveAnchor. This is shape normalization, not a complete dialect engine.
// Empty anyOf/oneOf are instead false schemas, preserving their meaning.
// This policy is shared by tools and response formats, not provider-specific.
func (c *schemaCleaner) clean(v any, path, resource *schemaPath, legacyRefs bool, dialect string) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		if _, ok := v.(bool); ok {
			return v, false
		}
		return map[string]any{}, true
	}
	if declared, ok := m["$schema"].(string); ok {
		dialect = schemaDialect(declared)
		legacyRefs = false
		switch dialect {
		case "json-schema.org/draft-04/schema", "json-schema.org/draft-06/schema", "json-schema.org/draft-07/schema":
			legacyRefs = true
		}
	}
	changed := false
	if _, ok := m["$ref"].(string); ok && legacyRefs {
		// Drafts 04/06/07 ignore validation siblings of $ref. Preserve that
		// behavior when declaring 2020-12, where those siblings would apply.
		for key := range m {
			switch key {
			case "$ref", "$schema", "definitions", "$defs", "title", "description", "default", "examples", "$comment":
				continue
			}
			if schemaKeywordKind(key) != "" || key == "id" || key == "const" || key == "dependencies" || key == "nullable" || key == "additionalItems" {
				delete(m, key)
				changed = true
			}
		}
	}
	changed = convertLegacySchema(m, path, dialect) || changed
	if _, ok := m["$id"]; ok {
		resource = path
	}
	for _, key := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
		if ref, ok := m[key].(string); ok {
			c.refs = append(c.refs, schemaReference{m, key, ref, resource})
		}
	}
	for key, value := range m {
		kind := schemaKeywordKind(key)
		valid := true
		switch kind {
		case "schema":
			if value != nil {
				switch value.(type) {
				case map[string]any, bool:
				default:
					valid = false
				}
			}
			if valid {
				next, changedChild := c.clean(value, path.child(key), resource, legacyRefs, dialect)
				m[key] = next
				changed = changed || changedChild
			}
		case "map":
			children, ok := value.(map[string]any)
			valid = ok
			if ok {
				for name, child := range children {
					next, changedChild := c.clean(child, path.child(key).child(name), resource, legacyRefs, dialect)
					children[name] = next
					changed = changed || changedChild
				}
			}
		case "array":
			children, ok := value.([]any)
			valid = ok && len(children) > 0
			if ok && len(children) == 0 && (key == "anyOf" || key == "oneOf") {
				m["not"] = map[string]any{}
			}
			if valid {
				for i, child := range children {
					next, changedChild := c.clean(child, path.child(key).child(strconv.Itoa(i)), resource, legacyRefs, dialect)
					children[i] = next
					changed = changed || changedChild
				}
			}
		case "string":
			_, valid = value.(string)
		case "anchor":
			s, ok := value.(string)
			valid = ok && schemaAnchor.MatchString(s)
		case "id":
			s, ok := value.(string)
			valid = ok && (!strings.Contains(strings.TrimSuffix(s, "#"), "#"))
		case "number":
			_, valid = value.(json.Number)
		case "positive":
			n, ok := value.(json.Number)
			valid = ok && numberIs(n, false)
		case "integer":
			n, ok := value.(json.Number)
			valid = ok && numberIs(n, true)
		case "boolean":
			_, valid = value.(bool)
		case "type":
			valid = validSchemaType(value)
		case "strings":
			valid = uniqueStrings(value)
		case "requiredMap":
			children, ok := value.(map[string]any)
			valid = ok
			if ok {
				for name, child := range children {
					if !uniqueStrings(child) {
						delete(children, name)
						changed = true
					}
				}
			}
		case "vocabulary":
			children, ok := value.(map[string]any)
			valid = ok
			if ok {
				for name, child := range children {
					if _, ok := child.(bool); !ok {
						delete(children, name)
						changed = true
					}
				}
			}
		case "values":
			_, valid = value.([]any)
		}
		if !valid {
			delete(m, key)
			changed = true
		}
	}
	// Only surviving keywords matter; removed ones must not defer the tail
	// until a second normalization of the emitted document.
	if _, exists := m["unevaluatedItems"]; exists {
		c.hasUnevaluatedItems = true
	}
	// Defer compatibility tails until every schema position has been visited:
	// unevaluatedItems can apply through a reference or an allOf elsewhere.
	if _, prefix := m["prefixItems"]; prefix {
		if _, tail := m["items"]; !tail {
			c.openTails = append(c.openTails, m)
		}
	}
	return m, changed
}

func (c *schemaCleaner) addItemTails() bool {
	changed := false
	if !c.hasUnevaluatedItems {
		for _, m := range c.openTails {
			m["items"] = map[string]any{}
			changed = true
		}
	}
	return changed
}

var schemaAnchor = regexp.MustCompile(`^[A-Za-z_][-A-Za-z0-9._]*$`)

// The keyword shapes mirror the standard 2020-12 meta-schema vocabularies.
// Unknown keywords are annotations and must remain opaque, even when null.
func schemaKeywordKind(key string) string {
	switch key {
	case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
		return "map"
	case "items", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "propertyNames", "contains", "not", "if", "then", "else", "contentSchema":
		return "schema"
	case "allOf", "anyOf", "oneOf", "prefixItems":
		return "array"
	case "$schema", "$ref", "$dynamicRef", "$recursiveRef", "$comment", "title", "description", "format", "contentEncoding", "contentMediaType", "pattern":
		return "string"
	case "$id":
		return "id"
	case "$anchor", "$dynamicAnchor", "$recursiveAnchor":
		return "anchor"
	case "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum":
		return "number"
	case "multipleOf":
		return "positive"
	case "maxLength", "minLength", "maxItems", "minItems", "maxContains", "minContains", "maxProperties", "minProperties":
		return "integer"
	case "uniqueItems", "readOnly", "writeOnly", "deprecated":
		return "boolean"
	case "type":
		return "type"
	case "required":
		return "strings"
	case "dependentRequired":
		return "requiredMap"
	case "$vocabulary":
		return "vocabulary"
	case "enum", "examples":
		return "values"
	}
	return ""
}
func uniqueStrings(v any) bool {
	a, ok := v.([]any)
	if !ok {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		s, ok := v.(string)
		if !ok || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}
func simpleSchemaType(s string) bool {
	switch s {
	case "object", "array", "string", "number", "integer", "boolean", "null":
		return true
	}
	return false
}
func validSchemaType(v any) bool {
	if s, ok := v.(string); ok {
		return simpleSchemaType(s)
	}
	if !uniqueStrings(v) {
		return false
	}
	a := v.([]any)
	if len(a) == 0 {
		return false
	}
	for _, s := range a {
		if !simpleSchemaType(s.(string)) {
			return false
		}
	}
	return true
}

// Inspect the decimal lexeme without expanding exponents (1e1000000000
// must cost no more than its input length). The decoder already checked syntax.
func numberIs(n json.Number, integer bool) bool {
	s := string(n)
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	coefficient, exponent, hasExponent := strings.Cut(strings.ToLower(s), "e")
	digits := strings.ReplaceAll(coefficient, ".", "")
	if strings.Trim(digits, "0") == "" {
		return integer
	}
	if negative {
		return false
	}
	if !integer {
		return true
	}
	fraction := 0
	if i := strings.IndexByte(coefficient, '.'); i >= 0 {
		fraction = len(coefficient) - i - 1
	}
	trailing := len(digits) - len(strings.TrimRight(digits, "0"))
	var exp int64
	if hasExponent {
		var err error
		exp, err = strconv.ParseInt(exponent, 10, 64)
		if err != nil {
			return !strings.HasPrefix(exponent, "-")
		}
	}
	return exp >= int64(fraction-trailing)
}

func convertLegacySchema(m map[string]any, path *schemaPath, dialect string) bool {
	changed := false
	// Draft-04 booleans qualify the inclusive bound. No bound means no
	// constraint; false keeps the inclusive bound, true moves it unchanged.
	for _, pair := range [][2]string{{"minimum", "exclusiveMinimum"}, {"maximum", "exclusiveMaximum"}} {
		if exclusive, ok := m[pair[1]].(bool); ok {
			delete(m, pair[1])
			changed = true
			if bound, ok := m[pair[0]].(json.Number); ok && exclusive {
				m[pair[1]] = bound
				delete(m, pair[0])
			}
		}
	}
	if tuple, ok := m["items"].([]any); ok {
		path.move("items", "prefixItems")
		if _, ok := m["additionalItems"]; ok {
			path.move("additionalItems", "items")
		}
		// A mixed-draft prefixItems cannot also express this tuple. Intersect the
		// two prefix constraints through allOf instead of overwriting either.
		if prefix, exists := m["prefixItems"]; exists {
			index := appendSchemaConjunct(m, map[string]any{"prefixItems": prefix})
			original := path.inputChild("prefixItems")
			original.output = []string{"allOf", strconv.Itoa(index), "prefixItems"}
			path.child("allOf").child(strconv.Itoa(index)).route("prefixItems", original)
		}
		if len(tuple) > 0 {
			m["prefixItems"] = tuple
		} else {
			delete(m, "prefixItems")
		}
		delete(m, "items")
		if tail, ok := m["additionalItems"]; ok {
			m["items"] = tail
			delete(m, "additionalItems")
		}
		changed = true
	}
	if deps, exists := m["dependencies"]; exists {
		if deps, ok := deps.(map[string]any); ok {
			for _, name := range path.child("dependencies").orderedKeys(deps) {
				dep := deps[name]
				key := "dependentSchemas"
				if _, ok := dep.([]any); ok {
					key = "dependentRequired"
					if !uniqueStrings(dep) {
						continue
					}
				}
				target, _ := m[key].(map[string]any)
				if target == nil {
					target = map[string]any{}
					m[key] = target
				}
				if old, ok := target[name]; ok {
					// Both declarations apply: allOf preserves the conjunction, including
					// dependentRequired collisions, without silently dropping either.
					index := appendSchemaConjunct(m, map[string]any{key: map[string]any{name: old}})
					original := path.inputChild(key).inputChild(name)
					// Replace the already emitted container segment as well as name.
					original.replaceParent = true
					original.output = []string{"allOf", strconv.Itoa(index), key, name}
					path.child("allOf").child(strconv.Itoa(index)).child(key).route(name, original)
				}
				target[name] = dep
				original := path.inputChild("dependencies")
				original.output = []string{}
				member := original.inputChild(name)
				member.output = []string{key, name}
				path.child(key).route(name, member)
			}
		}
		delete(m, "dependencies")
		changed = true
	}
	if nullable, exists := m["nullable"]; exists && dialect == "" {
		// OpenAPI 3.0 §4.7.24.2: add null ONLY to an explicit type; enum and
		// other constraints still apply (they may themselves exclude null).
		if nullable == true {
			if typ, ok := m["type"].(string); ok && simpleSchemaType(typ) && typ != "null" {
				m["type"] = []any{typ, "null"}
			}
		}
		delete(m, "nullable")
		changed = true
	}
	if dialect, ok := m["$schema"].(string); ok {
		switch schemaDialect(dialect) {
		case "json-schema.org/draft-04/schema", "json-schema.org/draft-06/schema", "json-schema.org/draft-07/schema", "json-schema.org/draft/2019-09/schema":
			m["$schema"] = "https://json-schema.org/draft/2020-12/schema"
			changed = true
		}
	}
	if id, ok := m["id"].(string); ok && (dialect == "" || dialect == "json-schema.org/draft-04/schema") {
		if _, exists := m["$id"]; !exists {
			base, anchor, hasFragment := strings.Cut(id, "#")
			if base != "" || !hasFragment {
				m["$id"] = base
			}
			if hasFragment && schemaAnchor.MatchString(anchor) {
				if _, exists := m["$anchor"]; !exists {
					m["$anchor"] = anchor
				}
			}
			// Non-anchor fragments cannot be represented by 2020-12 $id. Their
			// reference target is LOST; keep the base URI when it is representable.
			delete(m, "id")
			changed = true
		}
	}
	if id, ok := m["$id"].(string); ok && (dialect == "" || dialect == "json-schema.org/draft-06/schema" || dialect == "json-schema.org/draft-07/schema") {
		base, anchor, hasFragment := strings.Cut(id, "#")
		if hasFragment && schemaAnchor.MatchString(anchor) {
			if base == "" {
				delete(m, "$id")
			} else {
				m["$id"] = base
			}
			if _, exists := m["$anchor"]; !exists {
				m["$anchor"] = anchor
			}
			changed = true
		} else if hasFragment && anchor != "" && base != "" {
			// The unrepresentable anchor target is lost, but local pointers
			// must still resolve within this resource rather than its parent.
			m["$id"] = base
			changed = true
		}
	}
	return changed
}
func appendSchemaConjunct(m map[string]any, schema any) int {
	all, _ := m["allOf"].([]any)
	m["allOf"] = append(all, schema)
	return len(all)
}

// propertyView builds typed accessors directly from the one decoded tree.
// Only public roots retain Raw, so decode time and retention are O(input size).
func propertyView(v any) Property {
	m, ok := v.(map[string]any)
	if !ok {
		return Property{}
	}
	var p Property
	p.Type, _ = m["type"].(string)
	if types, ok := m["type"].([]any); ok {
		p.Types = make([]string, 0, len(types))
		for _, v := range types {
			if s, ok := v.(string); ok {
				p.Types = append(p.Types, s)
				if p.Type == "" && s != "null" {
					p.Type = s
				}
			}
		}
	}
	if ref, ok := m["$ref"].(string); ok {
		p.Ref = &ref
	}
	p.Description, _ = m["description"].(string)
	p.Enum, _ = m["enum"].([]any)
	p.Properties = propertyMapView(m["properties"])
	if required, ok := m["required"].([]any); ok {
		p.Required = make([]string, 0, len(required))
		for _, v := range required {
			if s, ok := v.(string); ok {
				p.Required = append(p.Required, s)
			}
		}
	}
	if item, exists := m["items"]; exists {
		view := propertyView(item)
		p.Items = &view
	}
	return p
}
func propertyMapView(v any) map[string]Property {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]Property, len(m))
	for name, v := range m {
		out[name] = propertyView(v)
	}
	return out
}

// A path trie records renamed positions during the same cleaning walk. Local
// JSON Pointer references are rewritten afterwards in O(reference length), so
// moving tuples/dependencies does not leave dangling or differently-bound refs.
// The trie tracks the resource root of $id scopes, including draft-04 id.
type schemaPath struct {
	order  []string
	input  map[string]*schemaPath
	routes map[string]*schemaPath
	output []string
	// A dependency collision moves one member out of its original container.
	replaceParent bool
}

func (p *schemaPath) inputChild(key string) *schemaPath {
	if p.input == nil {
		p.input = map[string]*schemaPath{}
	}
	if p.input[key] == nil {
		p.input[key] = &schemaPath{}
	}
	return p.input[key]
}
func (p *schemaPath) child(key string) *schemaPath {
	if next := p.routes[key]; next != nil {
		return next
	}
	return p.inputChild(key)
}
func (p *schemaPath) route(key string, target *schemaPath) {
	if p.routes == nil {
		p.routes = map[string]*schemaPath{}
	}
	p.routes[key] = target
}
func (p *schemaPath) move(from, to string) {
	node := p.inputChild(from)
	node.output = []string{to}
	p.route(to, node)
}

type schemaReference struct {
	object   map[string]any
	key, ref string
	resource *schemaPath
}
type schemaCleaner struct {
	refs                []schemaReference
	openTails           []map[string]any
	hasUnevaluatedItems bool
}

func (c *schemaCleaner) rewriteReferences() bool {
	changed := false
	for _, r := range c.refs {
		if !strings.HasPrefix(r.ref, "#/") {
			continue
		}
		fragment, err := url.PathUnescape(r.ref[1:])
		if err != nil {
			continue
		}
		tokens := strings.Split(fragment[1:], "/")
		path := r.resource
		out := make([]string, 0, len(tokens))
		moved := false
		for _, token := range tokens {
			key := strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
			if path != nil {
				path = path.input[key]
			}
			if path != nil && path.output != nil {
				if path.replaceParent && len(out) > 0 {
					out = out[:len(out)-1]
				}
				out = append(out, path.output...)
				moved = true
			} else {
				out = append(out, key)
			}
		}
		if moved {
			for i, token := range out {
				out[i] = strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
			}
			u := url.URL{Fragment: "/" + strings.Join(out, "/")}
			r.object[r.key] = u.String()
			changed = true
		}
	}
	return changed
}

// Match only standard dialect URIs, not custom dialects containing a familiar
// draft name in their path. Custom vocabularies remain opaque to this helper.
func schemaDialect(uri string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(uri, "https://"), "http://"), "#")
}
