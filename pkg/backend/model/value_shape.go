package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// The declared shape of a substituted value
//
// A `json` var and a `string[]` var both reach a substitution site as the
// same Go value — []any — so the renderer alone cannot tell an argv list
// from a document. The DECLARATION can, and it is the only thing that can:
// this file carries it to the one place a value becomes shell text.
// ---------------------------------------------------------------------------

// ValueShape is what the workflow declares about the slot a substituted
// value came from.
type ValueShape uint8

const (
	// ShapeUndeclared is a site no declaration reaches: an `outputs.*`
	// field, a key an edge delivers to a node with no `input:` schema, a
	// drilled path into a structured value. The value's own shape decides
	// (shellEscapeValue's heuristic).
	ShapeUndeclared ValueShape = iota
	// ShapeJSON is a `json` var or schema field: a document the author's
	// program parses. It renders as its compact JSON text, always ONE
	// shell token — `[]` for an empty list.
	ShapeJSON
	// ShapeWords is a `string[]` var or schema field: an argv position.
	// It renders as one shell-escaped word per element, and an empty list
	// renders as NO token at all.
	ShapeWords
)

// Shapes answers what the workflow declares for each reference a tool body
// substitutes. One reading for the executor and for a dry run, so a dry run
// cannot show a token the run will not produce.
//
// Three namespaces carry a declaration a tool node can reach:
// `vars.<name>` (the workflow's `vars:` block), `input.<field>` (the node's
// own `input:` schema) and `outputs.<node>.<field>` (the producer's
// `output:` schema). A drilled path — `input.a.b`, `outputs.n.f.g` — is the
// leaf of a structured value, which no schema types: it is undeclared, and
// says so.
//
// vars and outputs are workflow-wide and computed once; fields belong to
// one node and are layered on with WithInputSchema. Every map is written at
// construction and only read afterwards, so one Shapes serves every node of
// a run concurrently.
type Shapes struct {
	vars    map[string]ir.VarType
	outputs map[string]map[string]ir.FieldType
	fields  map[string]ir.FieldType
}

// WorkflowShapes reads the declarations that do not depend on which node
// renders: the `vars:` block, and the `output:` schema of every node an
// `{{outputs.…}}` reference can name.
func WorkflowShapes(wf *ir.Workflow) *Shapes {
	if wf == nil {
		return nil
	}
	s := &Shapes{}
	for name, v := range wf.Vars {
		if v == nil {
			continue
		}
		if v.Type == ir.VarJSON || v.Type == ir.VarStringArray {
			if s.vars == nil {
				s.vars = make(map[string]ir.VarType, len(wf.Vars))
			}
			s.vars[name] = v.Type
		}
	}
	for id, node := range wf.Nodes {
		fields := listFieldsOf(wf.Schemas[ir.NodeOutputSchema(node)])
		if fields == nil {
			continue
		}
		if s.outputs == nil {
			s.outputs = make(map[string]map[string]ir.FieldType, len(wf.Nodes))
		}
		s.outputs[id] = fields
	}
	if s.vars == nil && s.outputs == nil {
		return nil
	}
	return s
}

// WithInputSchema layers a node's own `input:` schema onto s. The receiver
// is not modified — a run renders several nodes at once, and they do not
// share an input schema.
func (s *Shapes) WithInputSchema(schema *ir.Schema) *Shapes {
	fields := listFieldsOf(schema)
	if fields == nil {
		return s
	}
	out := &Shapes{fields: fields}
	if s != nil {
		out.vars, out.outputs = s.vars, s.outputs
	}
	return out
}

// listFieldsOf keeps the list-typed fields of a schema, nil when it has
// none — the two types are the only ones a shape separates.
func listFieldsOf(schema *ir.Schema) map[string]ir.FieldType {
	if schema == nil {
		return nil
	}
	var out map[string]ir.FieldType
	for _, f := range schema.Fields {
		if f == nil || (f.Type != ir.FieldTypeJSON && f.Type != ir.FieldTypeStringArray) {
			continue
		}
		if out == nil {
			out = make(map[string]ir.FieldType, len(schema.Fields))
		}
		out[f.Name] = f.Type
	}
	return out
}

// Of answers the shape declared for ref. A nil Shapes — and any reference
// no declaration reaches — is ShapeUndeclared.
func (s *Shapes) Of(ref *ir.Ref) ValueShape {
	if s == nil || ref == nil || len(ref.Path) == 0 {
		return ShapeUndeclared
	}
	switch ref.Kind {
	case ir.RefVars:
		if len(ref.Path) != 1 {
			return ShapeUndeclared
		}
		return shapeOfVar(s.vars[ref.Path[0]])
	case ir.RefInput:
		if len(ref.Path) != 1 {
			return ShapeUndeclared
		}
		return shapeOfField(s.fields, ref.Path[0])
	case ir.RefOutputs:
		// The node id is the LONGEST dotted prefix that names a node: a
		// group instance is `prefix.name`, which collides with the dotted
		// reference grammar. The walk starts at the FULL path and stops at
		// the first node it names, exactly as outputsTemplateValue does —
		// starting one segment in would have shaped `{{outputs.a.b}}` by
		// node `a`'s field `b` where the runtime resolves node `a.b`'s
		// whole output, and the two would render one reference two ways.
		// The reference must then name exactly one field — the whole
		// output is a map, and a deeper path a leaf, which no schema types.
		for n := len(ref.Path); n >= 1; n-- {
			fields, ok := s.outputs[strings.Join(ref.Path[:n], ".")]
			if !ok {
				continue
			}
			if len(ref.Path)-n != 1 {
				return ShapeUndeclared
			}
			return shapeOfField(fields, ref.Path[n])
		}
	}
	return ShapeUndeclared
}

func shapeOfVar(vt ir.VarType) ValueShape {
	switch vt {
	case ir.VarJSON:
		return ShapeJSON
	case ir.VarStringArray:
		return ShapeWords
	}
	return ShapeUndeclared
}

func shapeOfField(fields map[string]ir.FieldType, name string) ValueShape {
	ft, ok := fields[name]
	if !ok {
		return ShapeUndeclared
	}
	switch ft {
	case ir.FieldTypeJSON:
		return ShapeJSON
	case ir.FieldTypeStringArray:
		return ShapeWords
	}
	return ShapeUndeclared
}

// shellJSONToken renders a `json`-declared value as its compact JSON text,
// shell-quoted as ONE token: `[]` for an empty list, `{}` for an empty
// object. An author who declares `json` writes a program that parses JSON
// (`KEY={{input.langs}} python3 -c "json.loads(os.environ['KEY'])"`), and a
// document split into shell words is neither the document nor an error —
// sh runs its second word as a command.
//
// Text passes through unquoted-by-JSON: a `json` var whose value is not
// JSON keeps the author's own string (`CoerceVarValue` leaves it as text)
// rather than gaining a pair of quotes it never had.
func shellJSONToken(val any) string {
	if s, ok := val.(string); ok {
		return shellEscape(s)
	}
	// A DECLARED slot holding JSON null holds a value, and `null` is its
	// text. Rendering nothing would leave `{{…}}` in the command, which is
	// the rule for a reference nobody wired — a different thing.
	b, err := json.Marshal(val)
	if err != nil {
		return shellEscape(fmt.Sprint(val))
	}
	return shellEscape(string(b))
}

// shellWords renders a `string[]`-declared value as argv words: one
// shell-escaped token per element, so each survives sh's re-tokenization as
// its own argument (`git add -- {{input.files}}`).
//
// An EMPTY list renders as no token at all. That is the declared meaning of
// an empty argv list, and it is why a `string[]` must not be written where
// one token is required: `cmd {{input.files}}` loses its argument and the
// next one shifts into its place. Declare the slot `json` when the site
// needs a token whatever the length.
//
// A non-slice value takes the single-word path through scalarWord: a
// `string[]` var still holding text has not been read by CoerceVarValue
// yet, and one token is the honest rendering of one string — while a map
// that reached a `string[]` slot anyway keeps its JSON, because Go's
// `map[k:v]` debug syntax is not something any program parses.
func shellWords(val any) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case []string:
		if len(v) == 0 {
			return ""
		}
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = shellEscape(s)
		}
		return strings.Join(parts, " ")
	case []any:
		if len(v) == 0 {
			return ""
		}
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = shellEscape(scalarWord(e))
		}
		return strings.Join(parts, " ")
	default:
		return shellEscape(scalarWord(val))
	}
}

// scalarWord renders one element of an argv list. A `string[]` holds
// strings, so fmt.Sprint is the whole answer for every value the type
// admits; an element that is a map or a slice anyway becomes its own JSON
// token rather than Go's `map[k:v]` syntax, keeping the promise the list
// makes — one word per element.
func scalarWord(e any) string {
	switch e.(type) {
	case map[string]any, []any, []string, []map[string]any:
		if b, err := json.Marshal(e); err == nil {
			return string(b)
		}
	}
	return fmt.Sprint(e)
}

// nodeShapes reads the declarations a tool node's bodies substitute from:
// the workflow's `vars:` block and the node's own `input:` schema. The one
// place the executor answers the question, so `command:`, `script:`,
// `postcondition:` and an action's params cannot disagree about what a
// value is.
func (e *ClawExecutor) nodeShapes(node *ir.ToolNode) *Shapes {
	var schema *ir.Schema
	if node != nil && node.InputSchema != "" {
		schema = e.schemas[node.InputSchema]
	}
	return e.wfShapes.WithInputSchema(schema)
}
