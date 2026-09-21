package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Simulation names what a dry run answers in the world's place — each arm
// an option of its own, off in every production launch, which a test holds.
// The graph, the computes, the edges and the loops run as they do; only the
// waits on the world are answered at once.
type Simulation struct {
	// AnswerHumans runs a human node through the executor whatever its
	// interaction mode, instead of pausing the run: the dry-run executor
	// answers with a schema-shaped output, and the run goes on.
	AnswerHumans bool
	// EventsArrive resolves a wait node at once — with the emitted payload
	// when the event was emitted before it, an empty one otherwise.
	EventsArrive bool
	// AnswersArrive collects an await_answers node at once, with no answer.
	AnswersArrive bool
	// BranchesRunToTheirEnd keeps a failed branch from cancelling its
	// siblings: every branch of a fan-out reaches its own end, so the
	// fan-out's verdict reads all of them — a dry run wants every death and
	// every ceiling, where a real run saves the tokens.
	BranchesRunToTheirEnd bool
	// Invented tells an expression failure that rests on a value the
	// simulation made up — a shape with no defined type — from one that
	// rests on the program's own values. The first decided nothing about
	// the program: its stand-in takes the expression's place and the pass
	// goes on; the second is the program's death, as ever. Nil: every
	// failure is the program's.
	Invented InventedValues
}

// ExpressionFailure is one expression the engine could not evaluate,
// handed to Simulation.Invented: the node it belongs to (a compute node,
// or the node whose outgoing edge carries the `when:`), the compute field
// it fills or the edge's target, the source text, the references it reads
// and the evaluator's error. Field and EdgeTo both empty: an iteration's
// collection (a fan_out_each `over:`, a foreach's collection) that could
// not be read as an array — NodeID is then the router, or the foreach's
// name.
type ExpressionFailure struct {
	NodeID string
	// Field is the compute field the expression fills; empty for an edge
	// or a collection.
	Field string
	// EdgeTo is the target of the edge whose `when:` failed; empty for a
	// compute field or a collection.
	EdgeTo string
	// Collection is "over" for a fan_out_each's `over:` and "foreach" for a
	// foreach's collection — the two iteration surfaces whose coercion can
	// fail — empty for every other failure. A foreach and a node can share
	// a name, so the surface is carried from the failing site, not guessed
	// from the name.
	Collection string
	Source     string
	Refs       []expr.Ref
	Err        error
}

// InventedValues is a simulation's knowledge of the values it made up in
// the program's place, asked when an expression fails. It answers whether
// the failure rests on one of them — then the expression decided nothing
// about the program, and standIn takes its result's place: a compute
// field's value, an edge's verdict — or on values the program produced,
// in which case ok is false and the failure is the program's.
type InventedValues interface {
	Inconclusive(f ExpressionFailure) (standIn any, ok bool)
}

// WithSimulation switches the engine's waits on the world to the
// simulation's answers. The zero value switches nothing.
func WithSimulation(s Simulation) EngineOption {
	return func(e *Engine) { e.simulation = s }
}

// Simulating reports whether any arm of a simulation is on — false for
// every production engine.
func (e *Engine) Simulating() bool {
	s := e.simulation
	return s.AnswerHumans || s.EventsArrive || s.AnswersArrive || s.BranchesRunToTheirEnd || s.Invented != nil
}

// exprRefsOf converts template references to the expression references the
// Invented seam reads.
func exprRefsOf(refs []*ir.Ref) []expr.Ref {
	out := make([]expr.Ref, 0, len(refs))
	for _, r := range refs {
		if r != nil {
			out = append(out, expr.Ref{Namespace: r.Kind.String(), Path: r.Path})
		}
	}
	return out
}

// inconclusiveExpression is the one site that reads Simulation.Invented:
// it hands an expression failure to the simulation and returns the
// stand-in it answers with, or ok false when no simulation is on or the
// failure rests on the program's own values.
func (e *Engine) inconclusiveExpression(f ExpressionFailure) (any, bool) {
	if e.simulation.Invented == nil {
		return nil, false
	}
	return e.simulation.Invented.Inconclusive(f)
}
