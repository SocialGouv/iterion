package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/hooks"
	clawrt "github.com/SocialGouv/iterion/pkg/claw/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
)

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

// dispatcherNodeKey carries, on the node execution's own context, the
// dispatcher that context belongs to. It is what makes [toolDispatcher.dispatch]
// refuse a context it cannot vouch for — see the contract on that method.
type dispatcherNodeKey struct{}

// toolDispatcher is the one door a tool call goes through. It holds what
// dispatching a call needs — the node's tool map, its lifecycle runner, its
// permission policy, its secret materializer, its event callbacks — as a
// value instead of as six positional arguments threaded down a call chain.
//
// That is the point. A caller that composes calls on the node's behalf (the
// codemode script host, ADR pending) gets ONE object, and a call it makes
// takes the pipeline the model's own loop takes: the same lifecycle hooks,
// the same permission gate, the same secret materialization, the same events,
// the same `tool_search` view of the node. Every added dispatch path is
// another gate to keep in agreement, and the gate is the
// anti-prompt-injection boundary.
//
// Two things this door does NOT carry, named because a caller that assumes
// them is the defect:
//
//   - **De-materialization is not here.** The secret materializer runs on the
//     way IN; re-placeholdering a tool result that echoed its own input is the
//     model boundary's pass (unmaterializeEchoingResults, in the loop in
//     generation.go), and it needs two things no dispatcher field holds. A
//     caller that returns tool output into a model's context owes that pass
//     too.
//   - **Four per-node facts ride the CONTEXT, not this struct**, so the
//     contract on dispatch is a contract on its ctx, enforced below.
//
// # The census this comment is responsible for
//
// What is counted is a site that RUNS a tool's Execute closure in this
// process. That is a greppable class rather than a judgement: Execute is a
// struct field of GenerationTool and tool.ToolDef, so reaching it needs a
// selector, and the inventory in generation_toolexec_callers_test.go holds
// the count instead of this sentence. That inventory covers more than this
// paragraph does, because touching a closure has three roles: RUNNING one is
// a dispatch path, WRAPPING one is a gate (guardTool,
// scopeMCPServerNamingTool), and CARRYING one across a conversion must add no
// judgement at all. Two other sites RUN one today, each with a DIFFERENT gate:
//
//   - claw_backend.go — the launcher's handler for a call a SANDBOXED node
//     forwarded over IPC. It cannot reuse this door: the runner already ran
//     the in-process pipeline on its side, and what crosses is an envelope,
//     not a tool_use block. So it re-derives what it must hold on the
//     boundary: the advertised-tool check, tool.SandboxPlacementOf, the
//     permission gate re-applied on the tool's identity, and this same
//     context pin. The `allowed-tools:` narrowing is NOT a fifth item there,
//     and that is not an oversight — it is held where each surface can hold
//     it: the prompt-invoked narrowing by the advertised set the launcher
//     built, which that first check enforces; the model-invoked one
//     in-container, dispatch-side, because that scope is created there
//     mid-generation and reaches the launcher only on a pause
//     (tool.CommandScope says the same from the mechanism's own side).
//   - executor_tool.go — a DSL tool node's `command:` running a registry
//     tool. It honours the authoring allowlist (checkToolNodePolicy over
//     tool.ToolChecker), NOT permission.Policy, which that file does not
//     import; it pins no node set and fires no PreToolUse, because no model
//     reads tool_search there. So it is not a second gate on a model's call,
//     and the census says so rather than leaving a reader to assume it.
//
// The census of permission.Policy itself is not duplicated here: it already
// lives at ClawExecutor.resolvePermissionPolicy. Two censuses in two files
// drift; one plus a pointer cannot.
type toolDispatcher struct {
	toolMap       map[string]*GenerationTool
	onToolStarted func(ToolCallInfo)
	onToolCall    func(ToolCallInfo)
	runner        *hooks.Runner
	materialize   func(string) string
	policy        *permission.Policy
	parallel      bool
}

// newToolDispatcher builds the node's door and returns the context that door
// vouches for. A caller dispatches with THAT context, or a descendant of it.
//
// Handing the context back, rather than letting a caller supply any context,
// is the whole guard: see [toolDispatcher.dispatch].
func newToolDispatcher(
	ctx context.Context,
	toolMap map[string]*GenerationTool,
	onToolStarted func(ToolCallInfo),
	onToolCall func(ToolCallInfo),
	runner *hooks.Runner,
	materialize func(string) string,
	policy *permission.Policy,
	auditedObservers ...bool,
) (*toolDispatcher, context.Context) {
	// Snapshot both the map and its definitions: capability, executable
	// identity and the discovery pin must agree for the entire tool round.
	frozen := make(map[string]*GenerationTool, len(toolMap))
	for name, def := range toolMap {
		if def != nil {
			copy := *def
			copy.InputSchema = bytes.Clone(def.InputSchema)
			frozen[name] = &copy
		}
	}
	toolHooks, parallel := runner.ConcurrentToolSnapshot()
	// A caller-supplied materializer has no concurrency contract.
	parallel = parallel && materialize == nil && ((onToolStarted == nil && onToolCall == nil) || (len(auditedObservers) == 1 && auditedObservers[0]))
	if parallel {
		runner = toolHooks
	}
	// Only runtime-owned observers may take this lock. Public callbacks run
	// on the serial path and never hold it, including on reentrant dispatch.
	if parallel {
		var mu sync.Mutex
		wrap := func(fn func(ToolCallInfo)) func(ToolCallInfo) {
			if fn == nil {
				return nil
			}
			return func(info ToolCallInfo) { mu.Lock(); defer mu.Unlock(); fn(info) }
		}
		onToolStarted, onToolCall = wrap(onToolStarted), wrap(onToolCall)
	}
	d := &toolDispatcher{
		toolMap:       frozen,
		onToolStarted: onToolStarted,
		onToolCall:    onToolCall,
		runner:        runner,
		materialize:   materialize,
		policy:        policy,
		parallel:      parallel,
	}
	return d, context.WithValue(ctx, dispatcherNodeKey{}, d)
}

// dispatch runs one tool_use block through the full pipeline.
//
// # The contract, and why it is enforced rather than documented
//
// ctx MUST be the context newToolDispatcher returned, or a descendant of it,
// because four per-node facts ride a context and not this struct — and three
// of them fail OPEN:
//
//   - the `allowed-tools:` command scope (#1717): absent, nothing is narrowed;
//   - the child-conversation Ask degradation (#2877): absent, a child's Ask
//     pauses the whole run instead of degrading to a refusal;
//   - the parent policy a child inherits: absent, a child spawned through the
//     `agent` tool is left ungated entirely;
//   - the parent loop shape that bounds a child's tools: absent, a child falls
//     back to the run's whole registry instead of this node's set.
//
// (tool.HostCapabilitiesFrom is the fifth and the exception: it fails CLOSED,
// refusing when nothing is held. That is the precedent this guard follows.)
//
// So the guard is one chokepoint, not four re-pinned facts. Re-pinning facts
// was tried and is WRONG in a way that passes the whole suite: writing a
// captured scope over a caller's live one erases a narrowing that was
// present, and writing a captured "asks allowed" over a child's context
// restores the run pause #2877 removes. A guard that refuses an unvouched
// context cannot widen anything, because it never writes a value at all.
//
// # What the guard does NOT check, stated because the difference matters
//
// It checks PROVENANCE, not content: newToolDispatcher vouches for whatever
// context it was handed, so a dispatcher built from a factless context
// vouches for a factless context. The guard therefore narrows the mis-wire
// from "any context" to "a context that went through this constructor" — it
// catches a host that dispatches with its own context, which is the shape
// that invites itself, and it cannot catch one that CONSTRUCTS from the wrong
// context. Supplying a node execution's context to the constructor is the
// caller's responsibility; on the loop's path GenerateText guarantees it for
// the scope (no generation runs scopeless, generation.go), and the suite
// proves the whole conduit: stripping the four facts inside dispatch while
// keeping the key reddens 17 tests across all four.
//
// That is also why the refusal says only what the guard knows. An earlier
// revision told the caller the narrowing and the child-ask degradation "would
// all be missing", which the guard never checked and which is false for the
// case it actually fires on most — a second dispatcher on a fully pinned
// context.
//
// The node's executable tool set is pinned here, derived from the same map
// executeOneTool resolves the call against, at the same instant: the pin
// cannot disagree with the lookup. A tool whose answer depends on that set —
// tool_search, which searches the registry's whole catalogue — must speak for
// THIS node (#2717), and tool.NodeExecutableToolsFrom deliberately reports
// "not pinned" rather than guessing. The table is frozen at construction;
// each call derives its pin by applying the caller's narrowing to that table.
func (d *toolDispatcher) dispatch(ctx context.Context, tu toolUseBlock) (api.ContentBlock, error) {
	return d.dispatchWithApproval(ctx, tu, nil)
}

func (d *toolDispatcher) dispatchWithApproval(ctx context.Context, tu toolUseBlock, approval *exactDispatchApproval) (api.ContentBlock, error) {
	if owner, _ := ctx.Value(dispatcherNodeKey{}).(*toolDispatcher); owner != d {
		return *refusalOutcome(fmt.Sprintf(
			"tool %q: refused before dispatch — this context did not come from this node's dispatcher. "+
				"Dispatch with the context newToolDispatcher returned, or a descendant of it",
			tu.Name), tu, d.onToolCall), nil
	}
	if err := ctx.Err(); err != nil {
		return api.ContentBlock{}, err
	}
	// The pin is the set THIS CALLER may run, which stopped being the node's
	// whole set when a caller gained the ability to be narrowed: a script host
	// is withheld the tools that pause the run, answer for the node, or are
	// exempt from the permission gate. tool_search reads this pin, so without
	// honouring the narrowing it would tell a script `ask_user` is available
	// right before the server refused it — two answers to one question.
	//
	// The door applies a filter the caller DECLARED; it knows nothing about
	// what a script is. A caller that pinned its own set instead would be a
	// second source of truth for what the node can run.
	keep, narrowed := tool.ExecutableToolFilterFrom(ctx)
	if narrowed {
		key := generationToolKey(d.toolMap, tu.Name)
		if _, known := d.toolMap[key]; known && !keep(key) {
			return *refusalOutcome(fmt.Sprintf("tool %q is not available to this caller", tu.Name), tu, d.onToolCall), nil
		}
	}
	names := make([]string, 0, len(d.toolMap))
	catalogue := make([]string, 0, len(d.toolMap))
	for name, t := range d.toolMap {
		if narrowed && !keep(name) {
			continue
		}
		names = append(names, name)
		if t.catalogueOnly {
			catalogue = append(catalogue, name)
		}
	}
	ctx = tool.WithNodeExecutableTools(ctx, names)
	// The deferral split, pinned from the SAME loop the executable set is
	// derived from — the narrowing included, so a caller whose filter
	// excludes a catalogue-only tool reads it as unavailable like any
	// other. Only a deferring node pins anything, so a node without the
	// split answers as it always has.
	if len(catalogue) > 0 {
		ctx = tool.WithCatalogueOnlyTools(ctx, catalogue)
	}
	return executeOneToolWithApproval(ctx, tu, d.toolMap, d.onToolStarted, d.onToolCall, d.runner, d.materialize, d.policy, approval)
}

// executeToolsDirect runs each tool_use block and builds tool_result content blocks.
//
// When runner is non-nil, the function fires PreToolUse before each
// Execute (a Block decision short-circuits to a synthetic refusal
// tool_result carrying the decision Reason), then either PostToolUse
// (success) or PostToolUseFailure (error) afterwards.
//
// A non-nil error return signals that the tool loop must abort and the
// caller should propagate the error up. The only case currently using
// this is *delegate.ErrAskUser (claw-code-go's native ask_user tool
// asking iterion to pause the run and surface the question to the dev).
// In every other failure mode the error is rendered into an isError=true
// tool_result and execution continues, so the LLM can recover.
func executeToolsDirect(
	ctx context.Context,
	toolUses []toolUseBlock,
	toolMap map[string]*GenerationTool,
	onToolStarted func(ToolCallInfo),
	onToolCall func(ToolCallInfo),
	runner *hooks.Runner,
	materialize func(string) string,
	policy *permission.Policy,
	auditedObservers ...bool,
) ([]api.ContentBlock, error) {
	results := make([]api.ContentBlock, 0, len(toolUses))
	d, ctx := newToolDispatcher(ctx, toolMap, onToolStarted, onToolCall, runner, materialize, policy, auditedObservers...)

	for _, tu := range toolUses {
		block, abort := d.dispatch(ctx, tu)
		if abort != nil {
			return results, abort
		}
		results = append(results, block)
	}

	return results, nil
}

// executeOneTool runs a single tool_use block through the full pipeline —
// lookup, input validation, lifecycle PreToolUse, permission gate,
// execution, and result shaping. Every failure mode is rendered into the
// returned isError tool_result so the LLM can recover; a non-nil abort
// error (currently only *delegate.ErrAskUser, from the native ask_user
// tool or a permission-gate Ask) signals the tool loop must stop and
// propagate it, discarding the block.
func executeOneTool(
	ctx context.Context,
	tu toolUseBlock,
	toolMap map[string]*GenerationTool,
	onToolStarted func(ToolCallInfo),
	onToolCall func(ToolCallInfo),
	runner *hooks.Runner,
	materialize func(string) string,
	policy *permission.Policy,
) (api.ContentBlock, error) {
	return executeOneToolWithApproval(ctx, tu, toolMap, onToolStarted, onToolCall, runner, materialize, policy, nil)
}

func executeOneToolWithApproval(
	ctx context.Context,
	tu toolUseBlock,
	toolMap map[string]*GenerationTool,
	onToolStarted func(ToolCallInfo),
	onToolCall func(ToolCallInfo),
	runner *hooks.Runner,
	materialize func(string) string,
	policy *permission.Policy,
	approval *exactDispatchApproval,
) (api.ContentBlock, error) {
	gt, ok := lookupGenerationTool(toolMap, tu.Name)
	if !ok {
		return unknownToolResult(tu, onToolCall), nil
	}

	// Validate that PartialJSON is well-formed JSON before either
	// firing hooks with stale/empty input or invoking Execute with
	// a payload its decoder can't parse.
	var hookInput map[string]any
	if jsonErr := json.Unmarshal([]byte(tu.PartialJSON), &hookInput); jsonErr != nil {
		return malformedInputResult(tu, jsonErr, onToolCall), nil
	}

	if refusal := preToolUseRefusal(ctx, runner, tu, hookInput, onToolCall); refusal != nil {
		return *refusal, nil
	}

	// A workspace command's `allowed-tools:` narrowing (#1717) binds every
	// dispatch that follows its invocation: refuse BEFORE the permission
	// gate, whose Ask would pause the run for an approval that cannot widen
	// the bound. The refusal is a synthetic tool_result like the gate's —
	// the model reads which command narrowed and adapts.
	if scope := tool.CommandScopeFrom(ctx); scope != nil {
		if err := scope.CheckAllowed(tu.Name); err != nil {
			block := api.ToolResult{
				ToolUseID: tu.ID,
				Content:   err.Error(),
				IsError:   true,
			}.ToContentBlock()
			if onToolCall != nil {
				info := toolCallInfo(tu)
				info.Error = err
				onToolCall(info)
			}
			return block, nil
		}
	}

	if approval != nil {
		if err := approval.authorize(ctx, tu, gt, policy, hookInput, materialize); err != nil {
			return api.ContentBlock{}, err
		}
		// Dispatch the same bytes that were checked, not a second materialization.
		materialize = nil
	} else if refusal, abort := permissionGateOutcome(ctx, policy, tu, hookInput, onToolCall); abort != nil {
		return api.ContentBlock{}, abort
	} else if refusal != nil {
		return *refusal, nil
	}

	if err := ctx.Err(); err != nil {
		return api.ContentBlock{}, err
	}
	output, err := runToolExecution(ctx, gt, tu, materialize, onToolStarted, onToolCall)
	// The tool returned, so its verdict is known: an ordinary error shapes
	// into the is_error result the model reads. Only a dispatch that never
	// returned — a cancelled context, or a tool that asked for a pause
	// instead of answering — is ambiguous.
	if approval != nil && err != nil {
		interrupted := ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		var ask *delegate.ErrAskUser
		if errors.As(err, &ask) {
			interrupted = true
		}
		if interrupted {
			return api.ContentBlock{}, &approvedDispatchInterrupted{cause: err}
		}
	}
	return shapeToolOutcome(ctx, runner, tu, hookInput, output, err, gt.imageResult)
}

// lookupGenerationTool resolves a tool_use name against the tool map. A
// bot prompt may name an MCP/board tool in the claude_code
// double-underscore FQN convention ("mcp__server__tool") while the claw
// in-process loop advertises it sanitized ("mcp_server_tool"); the second
// lookup bridges the two so the call dispatches.
func lookupGenerationTool(toolMap map[string]*GenerationTool, name string) (*GenerationTool, bool) {
	gt, ok := toolMap[generationToolKey(toolMap, name)]
	return gt, ok
}

func generationToolKey(toolMap map[string]*GenerationTool, name string) string {
	if _, exact := toolMap[name]; exact {
		return name
	}
	return canonicalMCPToolName(name)
}

// unknownToolResult renders the isError tool_result for a call naming a
// tool the map cannot resolve, and reports it on onToolCall — with the
// call's id, like every other refusal, so a program's call to a tool that
// does not exist is attributable to that program.
func unknownToolResult(tu toolUseBlock, onToolCall func(ToolCallInfo)) api.ContentBlock {
	block := api.ToolResult{
		ToolUseID: tu.ID,
		Content:   fmt.Sprintf("unknown tool: %s", tu.Name),
		IsError:   true,
	}.ToContentBlock()
	if onToolCall != nil {
		info := toolCallInfo(tu)
		info.Error = fmt.Errorf("unknown tool: %s", tu.Name)
		onToolCall(info)
	}
	return block
}

// malformedInputResult renders the isError tool_result for a tool_use
// whose PartialJSON does not parse — either a truncated stream the
// upstream aggregateStream missed, or a provider that emits invalid JSON.
// Both warrant a tool_result-isError, not a silent empty-args call that
// the LLM would never recover from cleanly.
func malformedInputResult(tu toolUseBlock, jsonErr error, onToolCall func(ToolCallInfo)) api.ContentBlock {
	block := api.ToolResult{
		ToolUseID: tu.ID,
		Content:   fmt.Sprintf("malformed tool input: %v", jsonErr),
		IsError:   true,
	}.ToContentBlock()
	if onToolCall != nil {
		info := toolCallInfo(tu)
		info.Error = fmt.Errorf("malformed tool input: %w", jsonErr)
		onToolCall(info)
	}
	return block
}

// preToolUseRefusal fires the PreToolUse lifecycle hook and, on a Block
// decision, renders the synthetic refusal tool_result carrying the
// decision Reason (defaulting to "blocked by lifecycle hook"). A nil
// return means the call may proceed. The callback info carries the call's
// id, so a blocked nested call is attributable to its program.
func preToolUseRefusal(ctx context.Context, runner *hooks.Runner, tu toolUseBlock, hookInput map[string]any, onToolCall func(ToolCallInfo)) *api.ContentBlock {
	dec, _ := runner.Fire(ctx, hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  tu.Name,
		ToolInput: hookInput,
	})
	if dec.Action != hooks.ActionBlock {
		return nil
	}
	reason := dec.Reason
	if reason == "" {
		reason = "blocked by lifecycle hook"
	}
	block := api.ToolResult{
		ToolUseID: tu.ID,
		Content:   fmt.Sprintf("tool refused: %s", reason),
		IsError:   true,
	}.ToContentBlock()
	if onToolCall != nil {
		info := toolCallInfo(tu)
		info.Error = fmt.Errorf("blocked by hook: %s", reason)
		onToolCall(info)
	}
	return &block
}

// permissionGateOutcome evaluates the tool-permission gate (the
// anti-prompt-injection boundary). Evaluated AFTER the lifecycle hook (so
// hooks still observe every call) and BEFORE execution. Deny → a synthetic
// refusal tool_result the model can adapt to. Ask → a *delegate.ErrAskUser
// abort so the run pauses for human approval (mirrors claude_code's
// PreToolUse permission hook for cross-backend parity) — unless the loop is
// a child conversation's (asksDenied, #2877): a child cannot pause the run,
// so the Ask degrades to a refusal that names the rule and points at the
// parent's intact ask channel. Allow — and a nil/disabled policy — returns
// (nil, nil): fall through to execution.
func permissionGateOutcome(ctx context.Context, policy *permission.Policy, tu toolUseBlock, hookInput map[string]any, onToolCall func(ToolCallInfo)) (*api.ContentBlock, error) {
	if !policy.Enabled() {
		return nil, nil
	}
	switch dec, rule := policy.Evaluate(tu.Name, hookInput); dec {
	case permission.Deny:
		return refusalOutcome(permission.DenyMessage(tu.Name, hookInput, rule), tu, onToolCall), nil
	case permission.Ask:
		if asksDenied(ctx) {
			return refusalOutcome(permission.UndelegatableAskMessage(tu.Name, hookInput, rule), tu, onToolCall), nil
		}
		// The approval channel (announced pre-task by the launcher): the
		// Ask forwards across the IPC instead of pausing in-process — the
		// runner blocks on the correlated decision, which carries the real
		// tool result on allow. A runner without the announcement never
		// reaches this branch: the bootstrap twin refused the ask-capable
		// policy before any model call.
		if fwd := sandboxApprovalForwarderFrom(ctx); fwd != nil {
			return sandboxApprovalAskOutcome(ctx, fwd, tu, hookInput, rule, onToolCall)
		}
		// Suspend the run for operator approval. The captured
		// question guides the model after the operator answers
		// (and grants), so on resume the re-issued call passes the
		// now-updated gate. Stamp the pending tool_use ID exactly
		// like the ask_user suspension in shapeToolOutcome.
		exact, err := permission.CanonicalApprovalInput(json.RawMessage(tu.PartialJSON))
		if err != nil {
			return refusalOutcome("cannot request approval: "+err.Error(), tu, onToolCall), nil
		}
		return nil, &delegate.ErrAskUser{
			Question:         permission.AskPrompt(tu.Name, hookInput, rule),
			PendingToolUseID: tu.ID,
			PermissionMarker: permission.Marker(tu.Name, hookInput, rule),
			PermissionInput:  exact,
		}
	}
	// permission.Allow falls through to execution.
	return nil, nil
}

// refusalOutcome renders a gate refusal as the synthetic isError tool_result
// the model can adapt to, and reports it on onToolCall.
func refusalOutcome(reason string, tu toolUseBlock, onToolCall func(ToolCallInfo)) *api.ContentBlock {
	block := api.ToolResult{
		ToolUseID: tu.ID,
		Content:   reason,
		IsError:   true,
	}.ToContentBlock()
	if onToolCall != nil {
		info := toolCallInfo(tu)
		info.Error = errors.New(reason)
		onToolCall(info)
	}
	return &block
}

// runToolExecution reports the call on onToolStarted, executes the tool,
// and reports the outcome on onToolCall. Secret placeholders are
// materialised into the input the tool actually executes with; the
// placeholder form (tu.PartialJSON) is what the callbacks, hooks, and
// event log persist, so the real secret never reaches the store — only
// the live tool call (Layer 1).
//
// Two things are armed on the context Execute receives, both per call: the
// call's identity — its tool_use id, which a tool that makes calls of its own
// hands them as their parent, and its parent's id, which a tool forwarding the
// call sends along — and a report sink whose content rides the completion
// event (a codemode program's bounds and verdict).
func runToolExecution(ctx context.Context, gt *GenerationTool, tu toolUseBlock, materialize func(string) string, onToolStarted, onToolCall func(ToolCallInfo)) (string, error) {
	if onToolStarted != nil {
		info := toolCallInfo(tu)
		info.Input = json.RawMessage(tu.PartialJSON)
		onToolStarted(info)
	}

	execInput := json.RawMessage(tu.PartialJSON)
	if materialize != nil && !keepsPlaceholders(tu.Name) {
		execInput = json.RawMessage(secretguard.MaterializeJSON([]byte(tu.PartialJSON), materialize))
		if string(execInput) != tu.PartialJSON {
			// A command naming a secret is not compressed: the compressor
			// runs it (rtk records every command it runs, value included).
			ctx = rewrite.WithMode(ctx, rewrite.Off)
		}

	}
	report := &toolCallReport{}
	ctx = withToolCallReport(withExecutingToolCall(ctx, tu.ID, tu.Parent), report)
	start := time.Now()
	var output string
	err := ctx.Err()
	if err == nil {
		output, err = gt.Execute(ctx, execInput)
	}
	dur := time.Since(start)

	if onToolCall != nil {
		info := toolCallInfo(tu)
		info.Output = output
		info.Duration = dur
		info.Error = err
		info.Codemode = report.codemodeReport()
		onToolCall(info)
	}
	return output, err
}

// shapeToolOutcome converts an execution outcome into the tool_result
// block for the conversation, firing the observational post-hooks.
//
// Special case: ask_user requested by the LLM. The suspension aborts the
// loop and propagates up so the backend can surface the question to
// iterion's pause/resume flow. The PostToolUseFailure hook is
// intentionally NOT fired — this isn't a tool failure, it's a suspension
// request. The pending tool_use ID is stamped so the backend can craft a
// tool_result block on resume.
//
// imageResult (the tool's GenerationTool.imageResult) has a successful
// output decoded into the description and image blocks it encodes, so the
// model receives the image itself; an output that does not decode, or an
// image too large to send, shapes into an is_error result naming it.
func shapeToolOutcome(ctx context.Context, runner *hooks.Runner, tu toolUseBlock, hookInput map[string]any, output string, err error, imageResult bool) (api.ContentBlock, error) {
	if err != nil {
		var askErr *delegate.ErrAskUser
		if errors.As(err, &askErr) {
			askErr.PendingToolUseID = tu.ID
			return api.ContentBlock{}, askErr
		}
		if diagnostic, ok := diagnosticShellFailureOutput(tu.Name, output, err); ok {
			_, _ = runner.Fire(ctx, hooks.Context{
				Event:      hooks.PostToolUse,
				ToolName:   tu.Name,
				ToolInput:  hookInput,
				ToolResult: diagnostic,
			})
			return api.ToolResult{
				ToolUseID: tu.ID,
				Content:   diagnostic,
			}.ToContentBlock(), nil
		}
		// Post-tool fires are observational; the runner logs any
		// handler error itself, so we discard the (Decision, error)
		// return on purpose.
		_, _ = runner.Fire(ctx, hooks.Context{
			Event:     hooks.PostToolUseFailure,
			ToolName:  tu.Name,
			ToolInput: hookInput,
			ToolError: err,
		})
		return api.ToolResult{
			ToolUseID: tu.ID,
			Content:   fmt.Sprintf("tool error: %v", err),
			IsError:   true,
		}.ToContentBlock(), nil
	}
	_, _ = runner.Fire(ctx, hooks.Context{
		Event:      hooks.PostToolUse,
		ToolName:   tu.Name,
		ToolInput:  hookInput,
		ToolResult: output,
	})
	if imageResult {
		content, decodeErr := tool.ImageResultContent(output)
		if decodeErr != nil {
			return api.ToolResult{
				ToolUseID: tu.ID,
				Content:   fmt.Sprintf("tool error: %s returned an image result the model cannot receive: %v", tu.Name, decodeErr),
				IsError:   true,
			}.ToContentBlock(), nil
		}
		return api.ContentBlock{Type: "tool_result", ToolUseID: tu.ID, Content: content}, nil
	}
	return api.ToolResult{
		ToolUseID: tu.ID,
		Content:   output,
	}.ToContentBlock(), nil
}

const diagnosticShellExitErrorPrefix = "command exited with error:"

// diagnosticShellFailureOutput exposes a completed diagnostic command's
// captured output even when it exits non-zero. claw's public executor returns
// that pair for an exit-status failure, while the usual tool result path would
// otherwise discard the output. This is deliberately fail-closed: timeouts,
// cancellation, validation failures, empty output, unknown errors, and every
// ordinary bash invocation retain the hard-error path above.
//
// The upstream executor already limits output to 10 KiB and marks head-only
// truncation with "[output truncated]". Keep that marker verbatim rather than
// inventing unavailable tail output; Copi can issue a narrower diagnostic.
func diagnosticShellFailureOutput(toolName, output string, err error) (string, bool) {
	if toolName != "diagnostic_shell" || output == "" || err == nil {
		return "", false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	errText := err.Error()
	if strings.HasPrefix(errText, "command timed out after ") || !strings.HasPrefix(errText, diagnosticShellExitErrorPrefix) {
		return "", false
	}
	header := fmt.Sprintf("Diagnostic command failed (%s); inspect and fix it before continuing:", errText)
	if strings.Contains(output, "[output truncated]") {
		header += " Output is partial; run a narrower diagnostic before deciding."
	}
	return header + "\n" + output, true
}

// compactionConfig sizes threshold compaction for a model spec. claw's
// registry knows vendor ids, never a routing prefix — "anthropic/claude-…"
// would miss it and fall to the unknown-model threshold — so it is asked
// about the route's capability id: the wire id of a vendor route. A gateway
// route has none, and gets claw's unknown-model threshold rather than a
// vendor's window matched on the alias's spelling.
func compactionConfig(ctx context.Context, model string, ratio float64, preserveRecent int) clawrt.CompactionConfig {
	route := modelroute.Parse(model)
	if route.Gateway() {
		return gatewayCompactionConfig(ctx, route.Wire, ratio, preserveRecent)
	}
	return clawrt.DefaultCompactionConfigForModel(route.CapabilityID(), ratio, preserveRecent)
}

// gatewayCompactionConfig sizes a gateway route's compaction from its
// catalog window: the trigger sits at window × effectiveRatio (the authored
// ratio when > 0, else 0.85), in the normal and pause paths alike — they
// both come through compactionConfig. An unknown window keeps claw's
// default floor: a guess the catalog's provenance admits to, never a
// vendor's window borrowed by name.
func gatewayCompactionConfig(ctx context.Context, wireModel string, ratio float64, preserveRecent int) clawrt.CompactionConfig {
	if ratio <= 0 {
		ratio = 0.85
	}
	if preserveRecent <= 0 {
		preserveRecent = clawrt.DefaultCompactionPreserveRecent
	}
	cfg := clawrt.CompactionConfig{
		PreserveRecentMessages: preserveRecent,
		MaxEstimatedTokens:     clawrt.DefaultCompactionConfig().MaxEstimatedTokens,
	}
	// The closure idiom (#2808): the seam's reader bound to the
	// invocation's ctx, handed as a value.
	getenv := func(name string) string { return envGetenv(ctx, name) }
	res := compatgw.ResolveCatalog(wireModel, modelspecs.Default(), getenv)
	if res.Known() && res.Spec.ContextWindow > 0 {
		cfg.MaxEstimatedTokens = int(float64(res.Spec.ContextWindow) * ratio)
	}
	return cfg
}

// maybeCompact runs claw's pure-function compactor with a config sized
// to the given model's context window (default trigger at 85% of the
// window, last 4 messages kept verbatim). The ratio and preserveRecent
// arguments override those defaults; pass 0 to keep them.
//
// It is a no-op for short transcripts (returns the input unchanged with
// `compacted=false`) and a bounded summarisation for long ones — the
// last preserveRecent turns are kept verbatim. The shared wrapper also
// removes any tool protocol half-pair exposed by the raw message boundary.
func maybeCompact(ctx context.Context, messages []api.Message, model string, ratio float64, preserveRecent int) (out []api.Message, info CompactInfo, compacted bool) {
	cfg := compactionConfig(ctx, model, ratio, preserveRecent)
	res := compactMessagesToolSafe(messages, cfg, nil)
	if res == nil {
		return messages, CompactInfo{}, false
	}
	return res.CompactedMessages, CompactInfo{
		BeforeMessages:      len(messages),
		AfterMessages:       len(res.CompactedMessages),
		RemovedMessageCount: res.RemovedMessageCount,
	}, true
}

// maybeCompactPause is the pause-aware compaction path. It preserves exactly
// the tool_use named by the suspension while pruning any other incomplete
// calls before the conversation is checkpointed.
func maybeCompactPause(ctx context.Context, messages []api.Message, model string, ratio float64, preserveRecent int, pendingToolUseID string) []api.Message {
	allowedPending := map[string]struct{}{}
	if pendingToolUseID != "" {
		allowedPending[pendingToolUseID] = struct{}{}
	}
	cfg := compactionConfig(ctx, model, ratio, preserveRecent)
	if res := compactMessagesToolSafe(messages, cfg, allowedPending); res != nil {
		messages = res.CompactedMessages
	}
	messages, _ = sanitizeToolPairs(messages, allowedPending, false)
	return messages
}

// maxContextCompactRetries bounds the reactive force-compaction that
// runs when the backend REJECTS a request for exceeding its real context
// window. Threshold compaction (maybeCompact) sizes itself to the model's
// ADVERTISED window (e.g. gpt-5's 1.05M), but the active backend may
// enforce a smaller one — most notably an OpenAI model driven through the
// ChatGPT-forfait endpoint, whose effective context is far below the
// API's. In that case the estimate stays under the advertised window so
// threshold compaction never fires, and without this reactive pass the
// tool loop dies mid-run with context_length_exceeded.
const maxContextCompactRetries = 4

// contextRetryTargets are the shrinking force-compaction token budgets
// tried, in order, on a context-window rejection — stepped well below
// common forfait caps so a compacted retry fits even when the backend's
// real window is unknown.
var contextRetryTargets = []int{256_000, 128_000, 64_000, 32_000}

// contextWindowMarkers identifies a backend's rejection of a request
// for exceeding the model's context window. claw's markers live in an
// internal package, so we mirror them here (provider-agnostic).
var contextWindowMarkers = []string{
	"context_length_exceeded", "maximum context length", "context window",
	"context length", "too many tokens", "prompt is too long",
	"input is too long", "request is too large",
}

// isContextWindowError reports whether err is the backend rejecting a
// request for exceeding the model's context window.
func isContextWindowError(err error) bool {
	if err == nil {
		return false
	}
	return strutil.ContainsAnyFold(err.Error(), contextWindowMarkers)
}

// forceCompactToTokens force-compacts messages to a target token budget,
// independent of the model's advertised window. Returns the compacted
// slice and true only when it actually shrank the history (so the caller
// stops retrying once the transcript can't get any smaller).
func forceCompactToTokens(messages []api.Message, targetTokens, preserveRecent int) ([]api.Message, bool) {
	if preserveRecent <= 0 {
		preserveRecent = clawrt.DefaultCompactionPreserveRecent
	}
	res := compactMessagesToolSafe(messages, clawrt.CompactionConfig{
		PreserveRecentMessages: preserveRecent,
		MaxEstimatedTokens:     targetTokens,
	}, nil)
	if res == nil || len(res.CompactedMessages) == 0 || len(res.CompactedMessages) >= len(messages) {
		return messages, false
	}
	return res.CompactedMessages, true
}

// forceCompactLadder walks the reactive retry targets from largest to
// smallest and returns the first compaction that actually shrank the
// transcript. Every rung is an ESTIMATE-anchored target: when the provider
// counts far more tokens than the local estimator (cached turns the provider
// still bills against the window, tokenizer drift), even the 256k rung can
// be a no-op on a transcript the provider just refused — the review-pool
// incident died on exactly that no-op (253,953 real input tokens estimated
// under 256,000; first rung a no-op; error surfaced). Giving up at the first
// no-op rung makes one token over the ceiling fatal; walking down recovers
// the call.
func forceCompactLadder(messages []api.Message, preserveRecent int) (compacted []api.Message, target int, ok bool) {
	for _, target := range contextRetryTargets {
		if out, shrunk := forceCompactToTokens(messages, target, preserveRecent); shrunk {
			return out, target, true
		}
	}
	return messages, 0, false
}

// callWithContextRetry runs one model call and, on a context-window
// rejection, force-compacts the (pointer-shared) history down the shrinking
// ladder and retries, up to maxContextCompactRetries. It mutates
// *messages in place so the compaction persists into the rest of the
// tool loop. Non-context errors and exhausted retries surface unchanged.
func callWithContextRetry(ctx context.Context, client api.APIClient, opts GenerationOptions, messages *[]api.Message, toolChoice *api.ToolChoice) (*aggregatedResponse, error) {
	for attempt := 0; ; attempt++ {
		// Heal persisted/compacted history in place so both this request and
		// any later session capture see the same valid conversation.
		*messages, _ = sanitizeToolPairs(*messages, nil, false)
		req, err := buildRequest(opts, *messages, nil, toolChoice)
		if err != nil {
			return nil, err
		}
		fireOnRequest(opts, len(*messages))
		agg, callErr := callAndAggregate(ctx, client, req, opts)
		e := callErr
		if e == nil && agg != nil {
			e = agg.err
		}
		if e == nil {
			return agg, nil
		}
		if !isContextWindowError(e) || attempt >= maxContextCompactRetries {
			return agg, e
		}
		compacted, target, ok := forceCompactLadder(*messages, opts.CompactPreserveRecent)
		if !ok {
			return agg, e // no rung shrank the transcript → surface the original error
		}
		*messages = compacted
		// Same reseed as the threshold path: after a forced squeeze the
		// model must re-read its todo list before continuing.
		if hasTodoTool(opts.Tools) {
			*messages = append(*messages, todoReseedMessage())
		}
		if opts.OnContextCompactRetry != nil {
			opts.OnContextCompactRetry(attempt+1, e, len(compacted), target)
		}
	}
}

// keepsPlaceholders: the tools whose input iterion or claw keeps rather than
// executes — a question to the operator the run pauses on, a board issue or a
// run query the store keeps and shows, the session's todo list written to disk.
// Like claude_code's interception of ask_user, they see the placeholder form.
//
// The script host joins them, and for a reason that is the inverse of the
// others': its input is not kept, it is a PROGRAM that in turn calls tools.
// Each of those nested calls comes back through runToolExecution and is
// materialised there, at its own dispatch, with rewrite.Off set for that call
// alone. Materialising the script SOURCE instead would substitute the
// cleartext value into the text that crosses the pipe into a child built to
// hold no credential — the environment would still be empty and the claim
// would still be false — and it would turn rewrite off for the whole script
// rather than for the one call that names a secret.
func keepsPlaceholders(name string) bool {
	switch name {
	case "ask_user", delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName, "todo_write",
		MemoryWriteToolName, "privacy_filter", ScriptHostToolName:
		return true
	}
	return strings.HasPrefix(canonicalMCPToolName(name), "mcp_iterion_")
}
