package runtime

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
}

// WithSimulation switches the engine's waits on the world to the
// simulation's answers. The zero value switches nothing.
func WithSimulation(s Simulation) EngineOption {
	return func(e *Engine) { e.simulation = s }
}

// Simulating reports whether any arm of a simulation is on — false for
// every production engine.
func (e *Engine) Simulating() bool { return e.simulation != (Simulation{}) }
