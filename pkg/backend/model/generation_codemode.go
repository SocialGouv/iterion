package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// codemodeDescription is what the model reads about the tool.
//
// It is a CONSTANT, and that is load-bearing rather than tidy: the tool array
// sits inside the cached prompt prefix (buildRequest marks it), so a
// description composed per node — from its tool list, its limits, its
// workspace — would move bytes the cache is keyed on and re-bill the whole
// prefix on every turn of every node. CM-3 composes a per-node index with a
// byte-stable composer for exactly that reason; until it lands, the honest
// thing is text that does not depend on the node.
//
// What it must say, and nothing more: that the input is a program, that the
// program's only capability is this node's own tools, how one is called, and
// that what comes back to the model is the last expression. A model told less
// writes a script that logs; a model told more reads a promise the host does
// not keep.
const codemodeDescription = `Run a JavaScript program that calls this node's other tools, so a loop, a filter or a large intermediate result stays out of the conversation. Return only what you need: the program's LAST EXPRESSION is what you get back, JSON-encoded.

call(name, input) runs one of this node's tools and returns {text, isError, truncated, remaining}; it THROWS on a failed call, with the same fields, so try/catch recovers. The tool names are the ones you already have, MINUS the ones a program may not reach: anything that pauses the run or answers for the node (asking the operator, sending a message, filling the output schema), the iterion control surface, sub-agents, and codemode itself. Call those directly instead. A program cannot open a file, a socket or a process, and holds no credential: tools are its whole world. There is no console, no require, no process, no timers, and no network.

Prefer this when the work is many calls, or one call whose output you only need a part of; call a tool directly when you need exactly one result.`

// codemodeInputSchema is the one argument: the program.
//
// `source` rather than `script`: the model is writing a program, and the
// parameter the API shows it is the only place that word appears.
var codemodeInputSchema = json.RawMessage(`{
    "type": "object",
    "properties": {
        "source": {
            "type": "string",
            "description": "The JavaScript program. Its last expression is the result."
        }
    },
    "required": ["source"]
}`)

// installCodemode registers the codemode tool on a node whose resolved
// codemode is on (ADR-165).
//
// The decision was made before this: [ClawExecutor.codemodePolicy] walked the
// precedence and refused a declared `on` the backend cannot serve, so a
// task arriving here with Codemode set is a task that may have it. This
// function asks no policy question of its own — a second opinion here is how
// a capability ends up granted on a path the resolver refuses.
//
// Nothing else about the node changes: the tool joins opts.Tools, which is
// the same list a direct call is resolved against, so the node's own door is
// what a program reaches.
func installCodemode(opts *GenerationOptions, onClamp func(requested, applied scripthost.Limits)) {
	opts.Tools = append(opts.Tools, codemodeTool(scripthost.Host{OnClamp: onClamp}, scripthost.Limits{}))
}

// codemodeTool is the tool whose input is a program.
//
// The host is COPIED per call inside Execute before its Tools field is set.
// That copy is the whole of it: one Host serves many scripts concurrently,
// but the tool server closes over the ctx of the dispatch that is running,
// and a server shared between two dispatches would answer one node's program
// through another node's door. The closure below captures `host` itself, so
// the copy cannot be hoisted here — an adversarial round found exactly that
// defect, and TestTwoDispatchesOfTheSameToolDoNotCrossDoors holds it on the
// `-race` leg.
//
// want is the limits the caller chose; the zero value means "nobody chose",
// and Clamp fills every unset field from DefaultLimits and holds the rest
// under Ceiling. There is no DSL surface for these yet, which is why the zero
// value is what [installCodemode] passes and why OnClamp — the seam that
// refuses to replace an operator's explicit choice in silence — is wired
// before anything can produce a clamp.
func codemodeTool(host scripthost.Host, want scripthost.Limits) GenerationTool {
	return GenerationTool{
		Name:        ScriptHostToolName,
		Description: codemodeDescription,
		InputSchema: codemodeInputSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Source string `json:"source"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("%s: decode args: %w", ScriptHostToolName, err)
			}

			// The node's door, from the ctx of THIS dispatch. The script host
			// tool itself is withheld from it, so a program cannot nest a
			// program — and the withholding narrows what tool_search answers
			// from, not only what the server runs, because both read the same
			// predicate (newScriptToolServer).
			//
			// An error here means the ctx carries no dispatcher, which the
			// dispatch guard makes unreachable on the loop's path. It is
			// returned rather than swallowed: a node that quietly served
			// programs with no door would be the one defect this whole slice
			// exists to make impossible.
			srv, err := newScriptToolServer(ctx, ScriptHostToolName)
			if err != nil {
				return "", fmt.Errorf("%s: %w", ScriptHostToolName, err)
			}
			// The per-call COPY. `host` is captured by the closure, and one
			// closure serves every call of the single tool installCodemode
			// appends — so assigning Tools on `host` itself writes state two
			// concurrent dispatches share, and the `-race` leg says so at
			// this line. A crossing there would answer one node's program
			// through another node's door.
			h := host
			h.Tools = srv

			resp, err := h.Run(ctx, args.Source, want)
			if err != nil {
				// THREE classes, not two, and scripthost separates all three
				// on purpose — its own Run doc enumerates them: the caller's
				// context ended, the binary could not be started, the
				// deployment answered something this code does not
				// understand. A refusal (below) is the fourth and is the
				// program's own doing. One shape lands in the wrong bucket
				// and is left there knowingly: a child that dies while the
				// parent is mid-write on the tool channel surfaces as an
				// EPIPE, which is neither a context error nor a broken
				// deployment, and is labelled one — a race whose cost is a
				// misleading sentence, not a wrong verdict.
				//
				// The tool_result text is the only place any of this reaches
				// the one party that would otherwise rewrite the program
				// until the node's step budget is gone — shapeToolOutcome
				// renders every error identically — so each class says what
				// it is. A first draft labelled all of them a deployment
				// failure, and a second adversarial round showed a cancelled
				// run being told "no other program will run either", which
				// is false: the run ended, that is all.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return "", fmt.Errorf("%s: %w — the run or this node's deadline ended while the program was running. Not your program, and not the deployment", ScriptHostToolName, err)
				}
				return "", fmt.Errorf("%s: %w — this is a DEPLOYMENT failure, not your program: re-running it will not help, and no other program will run either. Do the work with direct tool calls", ScriptHostToolName, err)
			}
			if !resp.OK() {
				// A bound that bit, or the program's own exception. It comes
				// back as a tool ERROR because that is what the model must
				// see to write a smaller or a correct program: shapeToolOutcome
				// turns it into an is_error tool_result the loop carries on
				// from, exactly as it does for any other tool that failed.
				return "", fmt.Errorf("%s refused (%s): %s", ScriptHostToolName, resp.Refusal, resp.Message)
			}
			return resp.Value, nil
		},
	}
}
