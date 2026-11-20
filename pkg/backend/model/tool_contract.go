package model

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// The tool-node contract
//
// A tool node's answer is its stdout, and the next node's body is built from
// that answer. Two holes in that chain run a command on a value nobody
// produced: a recipe that declares `output:` and prints something else than
// a JSON object, and a body whose `{{outputs.<node>.<field>}}` names a field
// the producer declared and did not supply. Each is refused at its node,
// before the next command runs, and each has a host-side dial that restores
// the lenient reading.
// ---------------------------------------------------------------------------

// ToolContract is what the host holds a tool node to beyond its exit status.
// The zero value holds it to nothing: a host that does not state the
// contract (WithToolContract) keeps the lenient readings.
type ToolContract struct {
	// JSONStdout fails a `command:` / `script:` recipe that declares
	// `output:` and whose stdout is non-empty and not a JSON object
	// (ToolStdoutNotJSONError), instead of publishing the `{"result": …}`
	// text wrapper. The lines the engine consumes as directives are not
	// part of that stdout (declaredAnswer).
	JSONStdout bool
	// OutputRefs fails a tool node before its body runs when the body holds
	// an `{{outputs.<node>.<field>}}` the producer's own `output:` schema
	// declares and its output of this run does not carry
	// (UnresolvedOutputRefError), instead of rendering the hole.
	OutputRefs bool
}

// EnvStrictOutputValidation and EnvStrictOutputRefs name the two host dials
// the refusals below point their reader at. The host reads them; this
// package only spells them in its messages.
const (
	EnvStrictOutputValidation = "ITERION_STRICT_OUTPUT_VALIDATION"
	EnvStrictOutputRefs       = "ITERION_STRICT_OUTPUT_REFS"
)

type toolContractKey struct{}

// WithToolContract returns a derived ctx carrying the contract the host
// holds this node's execution to.
func WithToolContract(ctx context.Context, c ToolContract) context.Context {
	return context.WithValue(ctx, toolContractKey{}, c)
}

// ToolContractFromContext returns the contract attached by WithToolContract,
// the zero value when none is.
func ToolContractFromContext(ctx context.Context) ToolContract {
	c, _ := ctx.Value(toolContractKey{}).(ToolContract)
	return c
}

// ToolContractViolation is a tool node refused for what its recipe printed
// or for what its body reads — never for its exit status. The same workflow
// on the same inputs is refused the same way, so a host classifies it as a
// permanent tool failure: no retry, no redelivery.
type ToolContractViolation interface {
	error
	toolContractViolation()
}

// ToolStdoutNotJSONError is a recipe that declares `output:` and printed
// something else than a JSON object. It names the schema and the size of
// what was printed, never the bytes: a tool's stdout is data, and this
// message is persisted on the run.
type ToolStdoutNotJSONError struct {
	Schema string
	// Bytes is the length of the stdout that did not parse.
	Bytes int
	// DrainCut reports that the bound on the drain of the recipe's output
	// cut the copy short: the bytes in hand may be a prefix of the answer.
	DrainCut bool
}

func (e *ToolStdoutNotJSONError) toolContractViolation() {}

func (e *ToolStdoutNotJSONError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stdout is not a JSON object, and the node declares `output: %s` (%d bytes printed, not quoted here)", e.Schema, e.Bytes)
	if e.DrainCut {
		b.WriteString(" — the wait for its output was cut short, so those bytes may be a prefix of the answer")
	}
	fmt.Fprintf(&b, ": print the schema's fields as ONE JSON object and send everything else to stderr. The engine's operator restores the {\"result\": …} text wrapper with %s=0, and the run of before only with %s=0 beside it — a node reading a declared field of the wrapper is refused otherwise; both are settings of the engine's process, which no bot variable, tenant setting or project `.env` can set", EnvStrictOutputValidation, EnvStrictOutputRefs)
	return b.String()
}

// OwedOutputRef is one `{{outputs.<node>.<field>}}` a body reads and the
// producer owes: the reference as the resolver reads it, the producer, its
// schema and the declared field its output does not carry.
type OwedOutputRef struct {
	Ref      string
	Producer string
	Schema   string
	Field    string
}

// UnresolvedOutputRefError is a tool node whose body — Surface names it —
// reads output fields their producers declared and did not supply. It names
// references and producers, never a value.
type UnresolvedOutputRefError struct {
	Surface string
	Owed    []OwedOutputRef
}

func (e *UnresolvedOutputRefError) toolContractViolation() {}

func (e *UnresolvedOutputRefError) Error() string {
	parts := make([]string, 0, len(e.Owed))
	producers := make([]string, 0, len(e.Owed))
	for _, o := range e.Owed {
		parts = append(parts, fmt.Sprintf("{{%s}} resolves to nothing — node %q ran and its `output: %s` declares %q, which its output does not carry", o.Ref, o.Producer, o.Schema, o.Field))
		if !slices.Contains(producers, o.Producer) {
			producers = append(producers, o.Producer)
		}
	}
	rewind := "`iterion rewind --run-id <run> --node <producer>`"
	if len(producers) > 0 {
		rewind = fmt.Sprintf("`iterion rewind --run-id <run> --node %s`", producers[0])
	}
	return fmt.Sprintf("%s not run: %s. The producer has to run again and answer with the field: a resume re-executes only this node, against the same output — rewind to the producer first (%s). The engine's operator restores the rendering of an unresolved reference with %s=0, a setting of the engine's process which no bot variable, tenant setting or project `.env` can set",
		e.Surface, strings.Join(parts, "; "), rewind, EnvStrictOutputRefs)
}

// declaredAnswer is the output of a recipe that succeeded, read against what
// the node declares: res.output as it stands, unless the host holds the node
// to a JSON answer, the node declares `output:`, and its stdout is not a JSON
// object.
//
// Then the lines the engine consumes as directives are read out of the
// answer first (withoutDirectiveLines): a node that prints
// `[iterion] attachment=report.md` and then its object answers with the
// object, and one that prints directives alone answered nothing. What is
// left decides: nothing keeps the lenient reading — res.output, the text
// wrapper of the stdout as printed; a JSON object is the answer; anything
// else is refused (ToolStdoutNotJSONError).
//
// A node without `output:` and an object missing declared fields keep their
// own readings — the wrapper, and the schema validation of the host.
func declaredAnswer(ctx context.Context, node *ir.ToolNode, res recipeResult) (map[string]any, error) {
	if !ToolContractFromContext(ctx).JSONStdout || node.OutputSchema == "" || !res.notObject {
		return res.output, nil
	}
	answer := withoutDirectiveLines(res.stdout)
	if strings.TrimSpace(answer) == "" {
		return res.output, nil
	}
	if answer != res.stdout {
		if object, wrapped := parseToolNodeOutputWrapped(answer, ""); !wrapped && object != nil {
			return object, nil
		}
	}
	return nil, &ToolStdoutNotJSONError{Schema: node.OutputSchema, Bytes: len(res.stdout), DrainCut: res.drainCut}
}

// owedOutputRefs judges the references of ONE body of node before it runs:
// nil unless the host holds the node to its output references and one of
// refs is an `outputs.<node>.<field>` the producer owes (outputRefOwed).
//
// The four bodies of a tool node that render this namespace ask it, each for
// its own references, where the body is about to run: a `command:` and a
// `script:` (shellRecipe, scriptRecipe — the Verified Action ladder runs
// the same two), a `postcondition:` (runPostcondition) and an action's
// `params:` (renderActionParams).
//
// What it refuses is a subset of what the rendering leaves as a hole
// (resolveTemplateWith's missing-value rule): a declared field absent from
// the output, or null where the declaration is not `json`, is unresolved on
// every surface. Every other hole keeps that rule's rendering.
func (e *ClawExecutor) owedOutputRefs(ctx context.Context, surface string, refs []*ir.Ref) error {
	if !ToolContractFromContext(ctx).OutputRefs {
		return nil
	}
	td := TemplateDataFromContext(ctx)
	if td == nil {
		return nil
	}
	var owed []OwedOutputRef
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref == nil || ref.Kind != ir.RefOutputs {
			continue
		}
		o, ok := e.outputRefOwed(td, ref.Path)
		if !ok || seen[o.Ref] {
			continue
		}
		seen[o.Ref] = true
		owed = append(owed, o)
	}
	if len(owed) == 0 {
		return nil
	}
	sort.Slice(owed, func(i, j int) bool { return owed[i].Ref < owed[j].Ref })
	return &UnresolvedOutputRefError{Surface: surface, Owed: owed}
}

// outputRefOwed reports whether `outputs.<segs>` names a field its producer
// owes: the producer is a declared TOOL node that HAS produced an output in
// this run, its `output:` schema declares the field, and the output does not
// carry it — absent, or null where the field is not declared `json` (a
// `json` field that is present and null holds a value, JSON null; so are
// `""`, `false`, `0` and `[]`).
//
// A tool node, because its `output:` is a promise its own recipe keeps or
// breaks — a Verified Action included, whichever rung answered: an
// idempotent skip answers with its postcondition's stdout, and a skip that
// must feed a reader prints the fields there. The other producers may leave
// a declared field out by design, and their readers keep the rendering: a
// human gate answered in part (a blank non-string field, a quick action, an
// optional file), a compute expression that resolves to an absence, a subbot
// whose answer is another workflow's — made of any of these. An agent, a
// judge and an `llm` router are validated where they answer, so the question
// does not arise for them.
//
// Everything else is not owed either: a producer that has not run on this
// path, a producer without a schema, the whole output (`outputs.<node>`), a
// field the schema does not declare — a runtime-injected `_`-prefixed one,
// the item a `fan_out_each` stamps on its router — and the inside of a field
// that is present, which no schema types.
func (e *ClawExecutor) outputRefOwed(td *TemplateData, segs []string) (OwedOutputRef, bool) {
	id, fields, declared := outputsRefProducer(td, segs)
	if !declared || len(fields) == 0 {
		return OwedOutputRef{}, false
	}
	if _, tool := td.Nodes[id].(*ir.ToolNode); !tool {
		return OwedOutputRef{}, false
	}
	out := td.Outputs[id]
	if out == nil {
		return OwedOutputRef{}, false
	}
	schemaName := ir.NodeOutputSchema(td.Nodes[id])
	schema := e.schemas[schemaName]
	if schema == nil {
		return OwedOutputRef{}, false
	}
	var field *ir.SchemaField
	for _, f := range schema.Fields {
		if f != nil && f.Name == fields[0] {
			field = f
			break
		}
	}
	if field == nil {
		return OwedOutputRef{}, false
	}
	if v, present := out[fields[0]]; present && (v != nil || field.Type == ir.FieldTypeJSON) {
		return OwedOutputRef{}, false
	}
	return OwedOutputRef{
		Ref:      "outputs." + strings.Join(segs, "."),
		Producer: id,
		Schema:   schemaName,
		Field:    fields[0],
	}, true
}
