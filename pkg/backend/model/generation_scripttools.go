package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

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
	d   *toolDispatcher
	ctx context.Context

	// withheld is the tool the script host itself is bound to, withheld so a
	// script cannot nest another script host. Empty while no DSL attribute
	// exists, which is CM-1b's deliberate state.
	withheld string

	// seq mints the tool_use id. Atomic because the field is read by the pump
	// goroutine; the counter is per-script, so the ids a run's events carry
	// say which call of which script they belong to.
	seq atomic.Int64
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
	s := &scriptToolServer{d: d, withheld: strings.TrimSpace(withheld)}

	// The narrowing is declared ONCE, on the ctx every nested dispatch
	// carries, so the set tool_search answers from is the set this server
	// will actually serve. Derived from the same predicate that refuses —
	// not a second list — so the two cannot drift: a tool added to the
	// withheld class disappears from discovery in the same edit.
	// withAsksDenied is the OTHER half of the rule, and round 5 shipped only
	// the first. Withholding the ask TOOLS stops a program from asking on
	// purpose; it does nothing about the ask VERDICT, which an ordinary tool
	// raises from inside a program when a permission rule matches it. An
	// adversarial round drove exactly that: `call("Bash", …)` under an `ask`
	// policy tore the program down mid-flight — with its earlier calls
	// already committed — and paused the whole run on the codemode tool_use.
	//
	// The degradation is the one subagent.go states for the identical
	// problem (#2877): the gate answers a refusal tool_result the program can
	// catch, instead of raising ErrAskUser from a place no resume can return
	// to. A program is the node's own turn, so there is no parent to carry
	// the question — which is why the only honest answer is "call it
	// directly".
	s.ctx = tool.WithExecutableToolFilter(withAsksDenied(ctx), func(name string) bool {
		_, withheldHere := s.withhold(name)
		return !withheldHere
	})
	return s, nil
}

// CallTool implements [scripthost.ToolServer].
//
// The ctx argument is the PUMP's, which carries the parent's script deadline;
// the dispatch uses the NODE's, because that is the one the door vouches for.
// Both matter: the pump's ending is what stops the serving loop, and the
// node's is what the tool runs under. Swapping them would either make every
// call refuse (a ctx the door cannot vouch for) or let a call outlive the
// script's wall bound.
func (s *scriptToolServer) CallTool(_ context.Context, name string, input json.RawMessage) (scripthost.ToolOutcome, error) {
	if refusal, ok := s.withhold(name); ok {
		return scripthost.ToolOutcome{Text: refusal, IsError: true}, nil
	}

	// A tool_use id a reader can attribute: the run's events carry it, so a
	// nested call is visible as what it is rather than as an unexplained extra
	// call on the step.
	tu := toolUseBlock{
		ID:          fmt.Sprintf("codemode_%d", s.seq.Add(1)),
		Name:        name,
		PartialJSON: string(normaliseInput(input)),
	}

	block, abort := s.d.dispatch(s.ctx, tu)
	if abort != nil {
		// The funnel's abort contract, unchanged: today the only case is a
		// tool whose verdict needs the operator. A script cannot wait for an
		// answer, so the abort travels OUT to the caller of Host.Run rather
		// than becoming a refusal the script could retry in a loop.
		return scripthost.ToolOutcome{}, abort
	}
	return scripthost.ToolOutcome{Text: toolResultText(block), IsError: block.IsError}, nil
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
	case s.withheld != "" && strings.EqualFold(strings.TrimSpace(name), s.withheld):
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
