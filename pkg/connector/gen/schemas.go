package gen

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// maxSchemaDepth bounds how deep an inline object is copied before the walk
// stops descending. A vendor schema can nest arbitrarily and can reference
// itself through inline members rather than through a $ref (a comment whose
// `replies` are comments); the depth guard is what keeps a package's size a
// function of its API rather than of the vendor's nesting habits, and it is
// what makes a self-referential type terminate at all.
const maxSchemaDepth = 6

// readSchemas copies the description's named object shapes into the package's
// shared schema map.
//
// Shared, not inlined per operation: on a real API the schemas are the bulk
// of the bytes and dozens of operations reuse the same handful. Pruned, not
// copied whole: iterion needs enough to validate an argument, render a form
// and type a result — a vendor's examples, extension keys and prose belong to
// its documentation, not to a package that ships in every runner image.
func (w *walker) readSchemas() {
	raw := w.rawSchemas()
	for _, name := range sortedKeys(raw) {
		sm, ok := raw[name].(map[string]any)
		if !ok {
			continue
		}
		w.schemas[name] = w.schema(sm, 0)
	}
}

// rawSchemas locates the named-schema map, which the two formats put in
// different places.
func (w *walker) rawSchemas() map[string]any {
	if w.format == FormatOpenAPI3 {
		return mapAt(mapAt(w.doc, "components"), "schemas")
	}
	return mapAt(w.doc, "definitions")
}

// schema prunes one vendor schema. A $ref becomes a NAME, never a copy: the
// reference graph stays a graph, so a type that refers to itself terminates
// instead of expanding forever.
func (w *walker) schema(sm map[string]any, depth int) spec.Schema {
	if ref := str(sm, "$ref"); ref != "" {
		return spec.Schema{Ref: schemaNameFromRef(ref)}
	}
	out := spec.Schema{
		Type:        str(sm, "type"),
		Format:      str(sm, "format"),
		Description: firstLine(str(sm, "description")),
		Enum:        enumStrings(sm),
		Required:    strSlice(sm, "required"),
	}
	// allOf is how both formats express composition. Merging the members'
	// properties is the only reading that keeps a composed type usable
	// without teaching every consumer about composition; the members are
	// shallow by construction (they are themselves refs or small objects).
	for _, member := range sliceAt(sm, "allOf") {
		mm, ok := member.(map[string]any)
		if !ok {
			continue
		}
		merged := w.schema(mm, depth)
		if merged.Ref != "" {
			// A composed ref cannot be merged without resolving it, and
			// resolving it here would re-inline what the map exists to share.
			// Recorded as the base type instead, which is what a reader needs
			// to follow the chain.
			if out.Ref == "" {
				out.Ref = merged.Ref
			}
			continue
		}
		if out.Type == "" {
			out.Type = merged.Type
		}
		out.Required = append(out.Required, merged.Required...)
		for k, v := range merged.Properties {
			if out.Properties == nil {
				out.Properties = map[string]spec.Schema{}
			}
			out.Properties[k] = v
		}
	}
	if depth >= maxSchemaDepth {
		return out
	}
	if items := mapAt(sm, "items"); len(items) > 0 {
		it := w.schema(items, depth+1)
		out.Items = &it
		if out.Type == "" {
			out.Type = "array"
		}
	}
	props := mapAt(sm, "properties")
	if len(props) > 0 {
		if out.Properties == nil {
			out.Properties = make(map[string]spec.Schema, len(props))
		}
		for _, name := range sortedKeys(props) {
			pm, ok := props[name].(map[string]any)
			if !ok {
				continue
			}
			out.Properties[name] = w.schema(pm, depth+1)
		}
		if out.Type == "" {
			out.Type = "object"
		}
	}
	return out
}

// resolveRef follows a local `#/…` reference in the description itself. It is
// used while WALKING (to flatten a $ref'd body or parameter), which is a
// different job from the schema map: there the reference is kept as a name.
//
// Only local references resolve. A remote `$ref` would make generation depend
// on fetching a second document at an unknown URL — which is a network call
// hidden inside a parse, and a supply-chain surface besides.
func (w *walker) resolveRef(ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	cur := any(w.doc)
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		// JSON Pointer escapes, which appear in any path used as a key.
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		cur, ok = m[seg]
		if !ok {
			return nil
		}
	}
	out, _ := cur.(map[string]any)
	return out
}

// schemaNameFromRef takes the last segment of a `$ref`, which is the name the
// shared schema map is keyed by in both formats
// (`#/definitions/Issue`, `#/components/schemas/Issue`).
func schemaNameFromRef(ref string) string {
	if ref == "" {
		return ""
	}
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
