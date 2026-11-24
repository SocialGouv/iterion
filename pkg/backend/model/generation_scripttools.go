package model

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// ---------------------------------------------------------------------------
// The script host's tool server: a nested call IS a direct call.
// ---------------------------------------------------------------------------

// ScriptHostToolName is the name the codemode tool will carry when a later
// slice registers it on a node.
//
// Declared now, and already honoured by [keepsPlaceholders] and by the
// withheld class, because the two defects it guards are both the kind that
// pass unnoticed: a materialised secret in the script source, and a script
// nesting another script host. Naming it here means the slice that registers
// the tool inherits both rather than rediscovering them — and a constant one
// `grep` finds beats a string literal in three files.
//
// No tool answers to it yet. CM-1b is driven by tests, which is why nothing
// model-facing opens with it.
const ScriptHostToolName = "codemode"

// scriptToolServer answers a script's tool call by dispatching it through the
// NODE'S OWN DOOR.
//
// That is the whole security content of the slice, and it is a property of the
// code rather than of a convention: the only thing this type can do is call
// [toolDispatcher.dispatch], which is [executeOneTool] with a provenance guard
// and the node's executable-tool pin in front. So every guarantee a direct
// call carries — the PreToolUse and PostToolUse hooks, the CommandScope, the
// permission gate, the secret materialisation and its de-materialisation, the
// run events, the budget accounting — reaches a nested call because it is the
// same function on the same ctx, not because this file remembered to do it.
//
// It lives in package model because it has to: dispatcherNodeKey and
// toolDispatcher are unexported, the dispatcher's six fields are unexported
// and typed on unexported-adjacent packages, and the guard
// TestEveryToolFunnelCallerIsOnTheRecord watches this package for callers of
// the funnel. A broker outside would need the funnel widened, which is the
// same thing as giving the funnel a second door.
type scriptToolServer struct {
	d     *toolDispatcher
	ctx   context.Context
	state CodemodeState

	// budget is the script's per-result bound ([scripthost.Limits.ToolResultBytes],
	// already clamped), armed on every nested call's context so a tool that
	// caps its own output can re-cap to what the script may receive (CM-8).
	// Zero means the arming caller had no budget to give — reachable only
	// from tests that build a server by hand, since the codemode tool always
	// clamps `want` at install.
	budget int64

	// withheld is the tool the script host itself is bound to, withheld so a
	// script cannot nest another script host. Empty while no DSL attribute
	// exists, which is CM-1b's deliberate state.
	withheld string

	// A nested call's tool_use id is idPrefix, unique per program so two
	// programs cannot collide in the run-wide event sinks, then the call's
	// frame ID — program order, as the child minted it, never the order
	// workers reached this server in.
	idPrefix string

	// parent is the tool_use id of the codemode call this program runs
	// under, which every nested call's events name. Empty when the program
	// was not started through the door, and its calls then read as
	// top-level ones.
	parent string

	// subagentsArmed mirrors the node's armed reserve at server build time:
	// the one fact the withhold verdict, the concurrency answer and the
	// catalogue all read, snapshotted before any worker starts.
	subagentsArmed bool
}

// newScriptToolServer takes the dispatcher and the context from the ctx of the
// tool call that is asking.
//
// It takes them TOGETHER and from the same place on purpose. dispatch compares
// the ctx's owner against itself by POINTER and refuses a mismatch, so a
// server built from one call's dispatcher and another call's ctx would refuse
// every call — silently, as an isError tool_result. Pulling both from one ctx
// makes that pairing impossible to get wrong.
//
// A nil dispatcher is an ERROR and not a server that refuses: a caller outside
// a node's tool execution has no node to borrow, and a script host handed a
// server that refuses everything would look like a tool-less node rather than
// a wiring mistake.
//
// The ctx must not outlive the Execute call it came from: a dispatcher's
// lifetime is ONE model step (buildToolMap is outside the step loop,
// newToolDispatcher inside it), so a server cached across steps would hold a
// pointer the next step's ctx does not vouch for.
func newScriptToolServer(ctx context.Context, withheld string) (*scriptToolServer, error) {
	d, _ := ctx.Value(dispatcherNodeKey{}).(*toolDispatcher)
	if d == nil {
		return nil, fmt.Errorf(
			"model: a script host can only borrow a node's tool door from the context of that node's own tool call; " +
				"this context carries none")
	}
	s := &scriptToolServer{
		d:        d,
		withheld: strings.TrimSpace(withheld),
		idPrefix: "codemode_" + rand.Text(),
		parent:   executingToolUseIDFrom(ctx),
		// The reserve arms the script route of the agent tool (CM-13): read
		// from the node's spend accumulator on this dispatch context, so a
		// server built for an unarmed node keeps the tool withheld, whatever
		// `withheld` names.
		subagentsArmed: func() bool {
			spend := SubagentSpendFromContext(ctx)
			if spend == nil {
				return false
			}
			_, armed := spend.ArmedSubagentReserve()
			return armed
		}(),
	}

	// Snapshot any inherited narrowing before workers start. Dispatch pins
	// its actual table through this same filter; Ask verdicts refuse rather
	// than pausing a script the operator could not resume.
	inherit, narrowed := tool.ExecutableToolFilterFrom(ctx)
	allowed := make(map[string]bool, len(d.toolMap))
	for name := range d.toolMap {
		if _, denied := s.withhold(name); !denied && (!narrowed || inherit(name)) {
			allowed[name] = true
		}
	}
	s.ctx = tool.WithExecutableToolFilter(withAsksDenied(ctx), func(name string) bool {
		return allowed[name]
	})
	// The script admission lives HERE and nowhere else (CM-13): nested
	// dispatches derive from this context, so a program's agent call admits
	// against the reserve, while a DIRECT agent call from the model's own
	// turn — dispatched from a context this server never touches — stays on
	// today's route (ADR-161).
	if spend := SubagentSpendFromContext(ctx); spend != nil {
		if _, armed := spend.ArmedSubagentReserve(); armed {
			s.ctx = WithSubagentScriptAdmission(s.ctx, spend)
		}
	}
	return s, nil
}

// ConcurrentSafe answers from the same frozen definition dispatch executes.
// A false verdict is an exclusive barrier, not just a lower pool priority.
func (s *scriptToolServer) ConcurrentSafe(name string) bool {
	if !s.d.parallel {
		return false
	}
	if s.subagentsArmed && isAgentTool(name) {
		// A scripted child is a long synchronous call with no shared state
		// outside the mutex'd spend accumulator; the reserve's Concurrent
		// cap, enforced at admission, is the real width bound — wider here
		// would only queue refusals past it.
		return true
	}
	if _, denied := s.withhold(name); denied {
		return false
	}
	key := generationToolKey(s.d.toolMap, name)
	def, known := s.d.toolMap[key]
	keep, narrowed := tool.ExecutableToolFilterFrom(s.ctx)
	return known && def.concurrentSafe && (!narrowed || keep(key))
}

// CallTool implements [scripthost.ToolServer]. The worker supplies a
// deadline, cancellation and the call's frame. Values always descend from the
// attested node.
func (s *scriptToolServer) CallTool(worker context.Context, name string, input json.RawMessage) (scripthost.ToolOutcome, error) {
	if err := worker.Err(); err != nil {
		return scripthost.ToolOutcome{}, err
	}
	// The id is minted FIRST, before any verdict: a call the server refuses
	// is still a call the program made, and its refusal is reported under
	// the same id and parent as a call that ran.
	//
	// The frame is the pump's (scripthost.WithCallFrame). A context without
	// one did not come from a program, and is refused rather than numbered
	// from a counter: a number minted here would be goroutine order
	// presented as program order.
	frame, ok := scripthost.CallFrameFrom(worker)
	if !ok {
		return scripthost.ToolOutcome{}, fmt.Errorf("model: the script host served call %q without its call frame; "+
			"a nested call is dispatched by the script host's tool channel only", name)
	}
	tu := s.nestedToolUse(frame, name, input)
	if refusal, ok := s.withhold(name); ok {
		block := refusalOutcome(refusal, tu, s.d.onToolCall)
		return scripthost.ToolOutcome{Text: toolResultText(*block), IsError: true}, nil
	}

	parent := s.ctx
	if deadline, ok := worker.Deadline(); ok {
		var cancelDeadline context.CancelFunc
		parent, cancelDeadline = context.WithDeadline(parent, deadline)
		defer cancelDeadline()
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stop := context.AfterFunc(worker, func() { cancel(context.Cause(worker)) })
	defer stop()
	// AfterFunc starts asynchronously, even when the worker is already done.
	if err := worker.Err(); err != nil {
		return scripthost.ToolOutcome{}, err
	}
	// The rich-result sink (CM-7) and the script's own result budget (CM-8)
	// are armed on the PER-CALL context, not on the server's: two calls of
	// one batch derive two contexts here, so parallel workers never share a
	// sink and the budget arms exactly where a tool reads it. After dispatch
	// returns, the sink holds the call's structured content — the Read
	// auto-retry's overwrite semantics live in the sink — and the outcome
	// carries it beside the text.
	sink := tool.NewRichResultSink()
	ctx = tool.WithRichResultSink(ctx, sink)
	if s.budget > 0 {
		ctx = scripthost.WithScriptResultBudget(ctx, s.budget)
	}
	block, abort := s.d.dispatch(ctx, tu)
	if abort != nil {
		if err := worker.Err(); err != nil && errors.Is(abort, ctx.Err()) {
			return scripthost.ToolOutcome{}, err
		}
		// The funnel's abort contract, unchanged: today the only case is a
		// tool whose verdict needs the operator. A script cannot wait for an
		// answer, so the abort travels OUT to the caller of Host.Run rather
		// than becoming a refusal the script could retry in a loop.
		return scripthost.ToolOutcome{}, abort
	}
	if err := worker.Err(); err != nil {
		return scripthost.ToolOutcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return scripthost.ToolOutcome{}, err
	}
	out := scripthost.ToolOutcome{Text: toolResultText(block), IsError: block.IsError}
	if raw, ok := sink.Result(); ok {
		out.Structured = raw
	}
	return out, nil
}

// RefusedOnBudget implements [scripthost.BudgetRefusalObserver]: a call the
// parent refused on the spent budget was never dispatched, and is reported
// here as the refusal it is, under its own id and parent.
func (s *scriptToolServer) RefusedOnBudget(frame scripthost.CallFrame, name string, input json.RawMessage, text string) {
	refusalOutcome(text, s.nestedToolUse(frame, name, input), s.d.onToolCall)
}

// nestedToolUse is a program's call as the tool_use block the door
// dispatches: an id a reader can attribute — the run's events carry it, so a
// nested call is visible as what it is rather than as an unexplained extra
// call on the step — the program's call as its parent, and the batch slot it
// came in.
func (s *scriptToolServer) nestedToolUse(frame scripthost.CallFrame, name string, input json.RawMessage) toolUseBlock {
	return toolUseBlock{
		ID:          fmt.Sprintf("%s_%d", s.idPrefix, frame.ID),
		Name:        name,
		PartialJSON: string(normaliseInput(input)),
		Parent:      s.parent,
		Batch:       frame.Batch,
	}
}

// withhold reports the tools a script may not call, by CLASS and not by a list
// of fully-qualified names.
//
// A name list is bypassable by spelling: the same tool reaches the funnel as
// `ask_user`, `mcp__iterion__ask_user`, `mcp_iterion_ask_user` and
// `mcp.iterion.ask_user` depending on the backend, and a list would have to
// carry every one. [permission.IsInfrastructureTool] keys on the NAMESPACE
// instead — a structural invariant every internal server upholds — and
// normalises the separator spellings, so it cannot be walked around.
//
// The class is withheld for one reason each, and none of them is "it felt
// risky":
//
//   - ask_user and the async ask family PAUSE THE RUN for an operator's
//     answer. A script cannot wait: the plan's own rule is that an Ask inside
//     a script is a typed refusal, never a pause, because a deferred tool
//     reachable only by script and refusing inside a script is a dead end the
//     model cannot escape.
//   - send_user_message is EXEMPT from the permission gate
//     (permission.go's infrastructure branch). Without this, a script's only
//     bound on messaging the operator would be the call budget.
//   - structured_output is how a node RETURNS its result. A script writing it
//     would answer for the node.
//   - the mcp_iterion family is the control surface — the board, the run
//     watch, the capability tools. Scripting it is its own slice, with its own
//     quota.
//   - the script host's own tool, so a script cannot nest another.
func (s *scriptToolServer) withhold(name string) (string, bool) {
	if s.subagentsArmed && isAgentTool(name) {
		// The reserve is armed: the script route exists, the tool is served
		// and admitted against the node's reserve by its own executor.
		return "", false
	}
	return scriptWithheld(name, s.withheld)
}

// scriptWithheld is the withheld class as a predicate of the tool name and the
// script host's own name: the ONE rule the server refuses by, the dispatch pin
// narrows by, and the codemode description and catalogue are composed by
// (CM-3). A second copy would let the index list a tool the door refuses.
func scriptWithheld(name, self string) (string, bool) {
	switch {
	// The ask channel, from the predicate subagent.go already derives from
	// the spellings the runtime appends these under — never a fresh list.
	// permission.IsInfrastructureTool does NOT reach it: it matches
	// `ask_user` by name and the rest by the `mcp_iterion` namespace, while
	// claw registers `ask_user_async` and `await_answers` as NATIVE tools,
	// whose bare names carry no namespace. An adversarial round reached both
	// from inside a program, which ADR-165 forbids in the plainest words it
	// uses: a script "cannot pause the run".
	case isRunPausingAskTool(name):
		return fmt.Sprintf(
			"tool %q pauses the run to ask the operator, which a script cannot do: the question has to "+
				"reach the operator from the node's own turn, not from inside a program. Call it directly "+
				"instead.", name), true
	case permission.IsInfrastructureTool(name):
		return fmt.Sprintf(
			"tool %q is iterion's own interaction surface and is not callable from a script: "+
				"it either pauses the run for the operator, answers for the node, or is exempt from the "+
				"permission gate. Call it directly instead.", name), true
	case isAgentTool(name):
		return fmt.Sprintf(
			"tool %q starts a sub-agent, which a script cannot do: a sub-agent spends a budget of its own "+
				"and must be admitted against the node's reserve. Call it directly instead.", name), true
	case self != "" && strings.EqualFold(strings.TrimSpace(name), self):
		return fmt.Sprintf(
			"tool %q is the script host itself: a script cannot start another script.", name), true
	}
	return "", false
}

// isAgentTool matches the sub-agent tool in both spellings a backend uses.
//
// Not folded into IsInfrastructureTool: that predicate means "iterion's own
// plumbing, always allowed by the gate", and the agent tool is neither. Two
// predicates because they answer two questions.
func isAgentTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "agent", "task":
		return true
	}
	return false
}

// toolResultText reads the text out of a tool_result block.
//
// api.ToolResult.ToContentBlock always builds exactly ONE text block, even for
// an empty string — but the funnel's abort path returns the ZERO ContentBlock,
// whose Content is nil. So the length check is not defensive padding: it is
// the one shape the type allows that the happy path never produces, and
// indexing [0] would panic the pump goroutine on it.
//
// A block that carries several text parts is joined rather than truncated to
// the first: "the first one wins" is a verdict reconstructed from order, which
// this repo has paid for.
func toolResultText(block api.ContentBlock) string {
	if len(block.Content) == 0 {
		return ""
	}
	if len(block.Content) == 1 {
		return block.Content[0].Text
	}
	parts := make([]string, 0, len(block.Content))
	for _, c := range block.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

// normaliseInput makes an empty or absent input the empty object.
//
// A tool's input validation reads PartialJSON as JSON; an empty string is not
// JSON, and a script calling a no-argument tool would otherwise get a parse
// error attributed to the tool.
func normaliseInput(in json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(in))) == 0 {
		return json.RawMessage("{}")
	}
	return in
}
