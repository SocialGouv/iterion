package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/scripthost/toolindex"
)

// codemodeDescription is the constant preamble of what the model reads about
// the tool; [codemodeToolDescription] appends the node's index to it.
//
// The tool array sits inside the cached prompt prefix (buildRequest marks it),
// so the composed description is a pure function of the node's
// script-callable tools: the preamble does not move, and the index is sorted
// and holds nothing per-turn — no limits, no counters, no workspace. Two turns
// of one node read the same bytes, which is what keeps the prefix cached.
//
// What it must say, and nothing more: that the input is a program, that the
// program's only capability is this node's own tools, how one is called and
// discovered, and that what comes back to the model is the last expression. A
// model told less writes a script that logs; a model told more reads a promise
// the host does not keep.
const codemodeDescription = `Run a JavaScript program that calls this node's other tools, so a loop, a filter or a large intermediate result stays out of the conversation. Return only what you need: the program's LAST EXPRESSION is what you get back, JSON-encoded.

call(name, input) runs one of this node's tools and returns {text, isError, truncated, remaining}; it THROWS on a failed call, with the same fields, so try/catch recovers. A tool that reports a structured result also carries it as structured, a JSON value; when that payload did not fit the result bound, structured is undefined and structuredTruncated/structuredBytes report the dropped size. The tools a program can call are listed below. Left out are the ones a program may not reach: anything that pauses the run or answers for the node (asking the operator, sending a message, filling the output schema), the iterion control surface, sub-agents, and codemode itself. Call those directly instead. call.describe(name) returns a tool's TypeScript declaration, and call.search(query) returns the tools that best match what you need, each {name, summary, declaration}; neither is a tool call. A program cannot open a file, a socket or a process, and holds no credential. call() is SYNCHRONOUS: it returns the result, not a promise. Do not write await, and do not declare anything async — there is no event loop to settle a promise, and await is a syntax error that costs you the whole program. There is no console, no require, no process, no timers, and no network.

call.parallel([{name, input}, …]) accepts up to four independent calls and returns entries in input order. Each entry has {status, text, isError, truncated, remaining}; an ok entry from a tool with a structured result also carries structured. status is "ok", "error", or "budget". Tool failures and budget refusals are entries, not exceptions. Every entry reports the same remaining budget after admitting the batch. Like call(), call.parallel() is synchronous. Only audited native glob and workspace_grep can overlap, where policy, hooks and limits allow it; other calls run exclusively. Cancellation does not undo effects that have already started.

Prefer this when the work is many calls, or one call whose output you only need a part of; call a tool directly when you need exactly one result.`

// codemodeDeferredComplement is appended to the description of a node that
// DEMOTED at least one tool to catalogue-only, after the constant preamble
// and before the index: some of the tools the index lists are not on the
// model's tool list. It says what is true and no more — they are not
// declared, and a program reaches them — and deliberately does not claim a
// direct call is impossible: the dispatcher's map is the union, so a
// tool_use naming one still executes. It is a constant, not a composed
// string — the description sits inside the cached prompt prefix, so the
// text is a pure function of the node's demoted set and must not move
// between turns.
const codemodeDeferredComplement = `Some of the tools below are not declared to you as tools on this turn: they live in this catalogue only. Reach them from a program — call.search finds one, call.describe returns its TypeScript declaration, and call(name, input) runs it.`

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
//
// `deferred` is the node's resolved defer state (CM-14). On, the author's
// tools are demoted HERE — markCatalogueOnly, between the one catalogue
// bound and the description — because both of its vetoes read what only
// this point holds: the bounded catalogue (a tool the bound does not keep
// whole would lose reach off the wire) and opts.Permission, the policy the
// dispatcher gates the direct path with. The description then gains the
// catalogue-only complement exactly when at least one tool was demoted, so
// a deferring node that demotes nothing pays no bytes for a sentence that
// would be false, and an undeferred node stays byte-identical to today's.
//
// `want` is the author's declared bounds, already resolved node-over-workflow
// by the executor; nil means nobody chose. It is passed THROUGH, not
// inspected: Limits.Clamp in the parent is what fills the unset fields from
// DefaultLimits and holds the rest under Ceiling, and a second opinion here
// would be a bound with two owners. OnClamp is how a reduction is reported;
// onWarn carries what the host could not do that leaves the verdict standing.
//
// The catalogue is taken HERE, from opts.Tools before codemode joins it: the
// list is final by now (the step budget pins on it right after), and it is the
// list the dispatcher's door is built from. The description's index and the
// catalogue each script receives both come from this one snapshot, through
// the same withheld predicate the door narrows by, so the index cannot list a
// tool the door refuses.
func installCodemode(opts *GenerationOptions, deferred bool, want *scripthost.Limits, reserve *scripthost.SubagentReserve, onClamp func(requested, applied scripthost.Limits), onWarn func(msg string)) error {
	return installCodemodeOn(opts, scripthost.Host{OnClamp: onClamp, OnWarn: onWarn}, deferred, want, reserve)
}

// installCodemodeOn is installCodemode on a given host: production passes the
// host above, tests pass one whose child is the test binary — so the wiring a
// test drives is the wiring that ships, catalogue included.
func installCodemodeOn(opts *GenerationOptions, host scripthost.Host, deferred bool, want *scripthost.Limits, reserve *scripthost.SubagentReserve) error {
	var limits scripthost.Limits
	if want != nil {
		limits = *want
	}
	// The reserve arms the script route of the agent tool (CM-13): the
	// catalogue keeps the tool, the description tells the program the
	// bounds it draws against, and the door serves it. A node that declared
	// no reserve keeps the tool withheld exactly as before.
	catalog := scriptCatalog(opts.Tools, ScriptHostToolName, reserve != nil)
	// The ONE bound of this snapshot: the index is composed from it, the
	// deferral reads it, and Host.Run bounds the same catalogue with the
	// same deterministic function, so all three see one set.
	bounded, omitted, err := toolindex.Bound(catalog)
	if err != nil {
		return fmt.Errorf("%s: %w", ScriptHostToolName, err)
	}
	if deferred {
		markCatalogueOnly(opts.Tools, bounded, opts.Permission)
	}
	description := codemodeToolDescription(bounded, omitted, anyCatalogueOnly(opts.Tools))
	if reserve != nil {
		per := reserve.TokensPerAgent
		if per <= 0 {
			per = reserve.Tokens
		}
		concurrent := reserve.Concurrent
		if concurrent <= 0 {
			concurrent = 1
		}
		description += fmt.Sprintf("\n\nagent: a program may start sub-agents of this node. Each child admits against the node's reserve of %d tokens (a child books %d until it settles its actual usage) and at most %d may run at once. A program cannot choose a child's model — children run on the node's own model.", reserve.Tokens, per, concurrent)
	}
	if opts.codemodeState != nil {
		description += "\n\ncall.store(key, value) writes a JSON value to this run’s shared state; call.load(key) reads it. A missing key returns undefined, not null. Both are synchronous, count against the script call budget, and cannot dispatch tools. The bank is bounded; handle invalid input and quota errors without retrying the same write."
	}
	host.Catalog = catalog
	opts.Tools = append(opts.Tools, codemodeToolWithState(host, limits, description, opts.codemodeState))
	return nil
}

// scriptCatalog is the node's tools a program can call, as the catalogue
// entries discovery answers from.
func scriptCatalog(tools []GenerationTool, self string, subagentsArmed bool) []toolindex.Tool {
	out := make([]toolindex.Tool, 0, len(tools))
	for _, t := range tools {
		if isAgentTool(t.Name) {
			// Armed or not, the agent tool never reaches scriptWithheld:
			// that predicate refuses it UNCONDITIONALLY (the reserve refusal
			// is one of its cases), so falling through would drop the tool
			// from the catalogue of an armed node whose door serves it.
			if !subagentsArmed {
				continue
			}
		} else if _, withheld := scriptWithheld(t.Name, self); withheld {
			continue
		}
		out = append(out, toolindex.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// markCatalogueOnly demotes, on a deferring node, every AUTHOR-DECLARED tool
// the deferral can take off the wire WITHOUT removing reach — buildRequest's
// loop is the flag's only reader; the tool map, the door and the catalogue
// keep serving the tool. A tool is demoted only when all three hold:
//
//   - it is author-declared (resolveToolsForNode's marker: the author's own
//     entry, and not a name the runtime appends — runtimeAppends);
//   - the bounded catalogue keeps it WHOLE — present, schema not omitted.
//     toolindex.Bound leaves out the tools past MaxTools and the over-long
//     names, and drops oversized schemas; demoting one of those would take
//     it off the wire AND out of the index and call.search, or leave a
//     program to call it blind. Catalogue membership also carries the
//     withheld veto: scriptCatalog keeps no withheld tool, so a tool that
//     pauses the run, answers for the node or nests a script is never in
//     `bounded`, and never demoted;
//   - the node's permission policy can never Ask about it
//     ((*permission.Policy).CanAskAbout, asked with the name the dispatcher
//     evaluates a direct call with — GenerationTool.Name is the tool_use
//     name). Inside a program an Ask degrades to a refusal, so a demoted
//     Ask-able tool would have no usable call at all: it keeps its direct
//     one. `policy` is opts.Permission, the object the dispatcher gates
//     with, not a re-parse.
//
// Every flag is assigned, so a tool that fails a veto is explicitly
// declared rather than left at whatever it held.
func markCatalogueOnly(tools []GenerationTool, bounded []toolindex.Tool, policy *permission.Policy) {
	whole := make(map[string]bool, len(bounded))
	for _, t := range bounded {
		if t.SchemaOmitted == 0 {
			whole[t.Name] = true
		}
	}
	for i := range tools {
		t := &tools[i]
		t.catalogueOnly = t.authorDeclared &&
			whole[strings.TrimSpace(t.Name)] &&
			!policy.CanAskAbout(t.Name)
	}
}

// anyCatalogueOnly reports whether the node demoted at least one tool off
// the wire — the fact the catalogue-only complement states.
func anyCatalogueOnly(tools []GenerationTool) bool {
	for _, t := range tools {
		if t.catalogueOnly {
			return true
		}
	}
	return false
}

// codemodeToolDescription is the preamble and the node's index, composed
// from the catalogue as [toolindex.Bound] bounded it — the bound the child's
// catalogue gets too, so the index a model reads and the catalogue a program
// searches are the same set. demoted appends the catalogue-only complement
// between preamble and index: a pure function of the node's demoted set, so
// the cached prefix stays byte-stable within one node.
func codemodeToolDescription(bounded []toolindex.Tool, omitted int, demoted bool) string {
	description := codemodeDescription
	if demoted {
		description += "\n\n" + codemodeDeferredComplement
	}
	return description + "\n\nTools a program can call:\n" + toolindex.New(bounded, omitted).Lines()
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
// under Ceiling. The DSL surface that fills it is the `codemode:` block
// (ADR-165), resolved node-over-workflow field by field, with C347 at compile
// where a declared value is above the ceiling — OnClamp stays the seam that
// refuses to replace an operator's explicit choice in silence, and is now
// reachable by an author rather than only by a future lot.
func codemodeToolWithState(host scripthost.Host, want scripthost.Limits, description string, state CodemodeState) GenerationTool {
	// The budget a nested tool re-caps its own output to (CM-8), taken from
	// the CLAMPED limits: `want` is what the author declared and Clamp is
	// what runs, so a declared 4 MiB held to the 1 MiB ceiling must not arm
	// 4 MiB on a call context. Clamped once here rather than per call —
	// Clamp is deterministic and `want` does not move after install. The
	// same pair is what each program's report states (CodemodeReport).
	effective, _ := want.Clamp()
	return GenerationTool{
		Name:        ScriptHostToolName,
		Description: description,
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
			srv.state = state
			srv.budget = effective.ToolResultBytes
			h.Tools = srv

			// The node's deadline bounds the program before the wall bound
			// does when it is nearer: Host.Run's context inherits it.
			var effectiveWall time.Duration
			if deadline, ok := ctx.Deadline(); ok {
				if left := time.Until(deadline); left > 0 && left < effective.Wall {
					effectiveWall = left
				}
			}
			resp, err := h.Run(ctx, args.Source, want)
			// The program's report rides its call's completion event once
			// the host was handed the program, whichever way it ended from
			// there: Run fills the channel's count on an error return too.
			// The two returns above it — an input that does not decode, a
			// context with no door — come before any program ran, and carry
			// no report.
			reportCodemode(ctx, newCodemodeReport(want, effective, args.Source, resp, effectiveWall))
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
