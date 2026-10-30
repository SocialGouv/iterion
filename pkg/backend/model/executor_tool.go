package model

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// ---------------------------------------------------------------------------
// Tool node execution
//
// Extracted from executor.go to keep that file focused on Execute /
// resolveBackend / executeBackend flow. Same package, so no API change.
// ---------------------------------------------------------------------------

// executeToolNode runs a tool node (direct command, no LLM).
// The tool policy is checked before execution; denied tools produce an
// explicit error with the tool_called hook fired (Error != nil).
func (e *ClawExecutor) executeToolNode(ctx context.Context, node *ir.ToolNode, input map[string]any) (map[string]any, error) {
	// A connector action's invariants are enforced HERE, before anything
	// else can claim the node.
	//
	// The compiler already refuses these combinations (C261/C262), and that
	// refusal is where an author meets them. It is not where they matter: a
	// compile-time guard certifies the text a person wrote, while this
	// function runs whatever IR it is handed — hand-built, restored from a
	// checkpoint, or produced by a future path nobody has written yet.
	// Without the check here, an action node carrying a postcondition took
	// the Verified Action ladder below, whose first rung is an
	// idempotent-skip: the node reported SUCCESS with no HTTP request ever
	// made. A determinism promise that only holds for well-formed input is
	// not a promise.
	if node.Action != "" {
		if err := checkActionInvariants(node); err != nil {
			return nil, err
		}
		return e.executeToolNodeAction(ctx, node, input)
	}
	// Verified Action (ADR-044): a node with a postcondition runs through
	// the escalation ladder (idempotent-skip → recipe → self-repair →
	// agent recovery → policy), keying success on the postcondition rather
	// than the recipe's exit code. Nodes without a postcondition fall
	// through to the unchanged recipe-only path below (full backward compat).
	if node.Postcondition != "" {
		return e.executeVerifiedToolNode(ctx, node, input)
	}
	return e.executeToolNodeRecipe(ctx, node, input)
}

// checkActionInvariants re-states at RUNTIME what the compiler refuses at
// C261/C262, for the reason given above: the two guards protect different
// things, and only this one protects the run.
//
// Both refusals exist because the ADR-044 recovery ladder ends in an LLM
// agent and can conclude success without executing the recipe — either of
// which turns "this node involves no model and either performed the call or
// failed" into something weaker.
func checkActionInvariants(node *ir.ToolNode) error {
	if node.Postcondition != "" {
		return fmt.Errorf("model: tool node %q declares both `action:` and a postcondition; "+
			"a connector action is its own verification and the Verified Action ladder may report success without calling the vendor", node.ID)
	}
	if node.Recovery != nil {
		return fmt.Errorf("model: tool node %q declares both `action:` and `recovery:`; "+
			"the recovery ladder ends in an LLM agent, which an action node is certified not to reach", node.ID)
	}
	return nil
}

// recipeKind classifies how a tool node's recipe is executed, under
// dispatch's own names for ir's answer. The classification lives in ir,
// with the fields it reads: executeToolNodeRecipe (standard path),
// runVerifiedRecipe (Verified Action ladder) and the launch walks that
// price the self-repair rung (ir.ToolRecoverySelfRepairRung) all read the
// one heuristic, so none can drift from the others.
type recipeKind = ir.ToolRecipeKind

const (
	recipeScript   = ir.ToolRecipeScript   // `script:` body via an interpreter
	recipeShell    = ir.ToolRecipeShell    // `command:` with template refs or shell metacharacters
	recipeRegistry = ir.ToolRecipeRegistry // bare `command:` resolved as a registered tool name
	recipeAction   = ir.ToolRecipeAction   // `action:` — a connector operation (ADR-098)
)

// recipeKindOf reports how node's recipe should run.
func recipeKindOf(node *ir.ToolNode) recipeKind { return ir.ToolRecipeKindOf(node) }

// toolNodeSetupError marks the sandbox refusal of a registry recipe: the
// recipe can never run under this sandbox, so the Verified Action ladder
// fails the node with it instead of treating it as a recipe run it may
// repair, recover around, or waive under `policy: best_effort`. Every other
// recipe failure stays a run failure for the ladder to judge.
type toolNodeSetupError struct{ err error }

func (s *toolNodeSetupError) Error() string { return s.err.Error() }
func (s *toolNodeSetupError) Unwrap() error { return s.err }

// executeToolNodeRecipe runs a tool node's recipe with exit-code = success
// (the pre-ADR-044 behaviour). It is the rung-2 primitive of the Verified
// Action ladder and the whole of the non-verified path.
func (e *ClawExecutor) executeToolNodeRecipe(ctx context.Context, node *ir.ToolNode, input map[string]any) (map[string]any, error) {
	switch recipeKindOf(node) {
	case recipeAction:
		// Not reached from executeToolNode, which intercepts an action node
		// above so it can never enter the Verified Action ladder this
		// function also serves. Kept as the correct dispatch rather than
		// deleted: falling through to the command arms would read an action
		// node's empty Command as a tool name.
		return e.executeToolNodeAction(ctx, node, input)
	case recipeScript:
		return e.executeToolNodeScript(ctx, node, input)
	case recipeShell:
		return e.executeToolNodeShell(ctx, node, input)
	}

	toolName := node.Command

	// Policy check before resolution — fail fast on denied tools, and never
	// pay an MCP boot for one. The check matches on the tool's IDENTITY for
	// alias spellings: a policy allowlisting `read_file` covers
	// `command: Read`, and a policy naming `Read` covers the canonical
	// spelling it resolves to — the same canonical name guardTool checks on
	// the agent path. The walk is the ALIAS tier only: a bare MCP shorthand
	// or an mcp__ FQN keeps the spelling it wrote, so a policy verdict never
	// depends on whether an unrelated node happened to boot that server
	// earlier in the run. The denial event and message name the RAW
	// spelling, what the node actually asked for.
	policyName, policyQualified := e.toolNodePolicyIdentity(ctx, toolName)
	if err := e.checkToolNodePolicy(ctx, node, policyName, policyQualified, toolName); err != nil {
		return nil, err
	}

	resolved, ok, err := e.resolveSingleToolForNode(ctx, node, toolName)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: %w", node.ID, err)
	}
	if !ok {
		return nil, fmt.Errorf("model: tool node %q references unregistered tool %q", node.ID, toolName)
	}
	// A sandboxed run executes a tool node's shell and script recipes in the
	// container, but a registry tool is a closure in THIS process: only one
	// whose home is the launcher's own state may run here.
	if e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
		if placement, reason := tool.SandboxPlacementOf(resolved.QualifiedName); placement != tool.PlacementLauncher {
			return nil, &toolNodeSetupError{fmt.Errorf(
				"model: tool node %q: registry tool %q would execute on the host, outside the run's sandbox (placement %s: %s) — "+
					"write it as a shell `command:`, which runs in the container, or run the workflow unsandboxed (`sandbox: none` / `--sandbox none`)",
				node.ID, toolName, placement, reason)}
		}
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: marshal input: %w", node.ID, err)
	}

	// Persistence-aware redaction: privacy_filter carries raw PII as
	// input; privacy_unfilter produces raw PII as output. The hooks
	// below feed the persisted event log, so apply redaction up-front
	// (input) and after Execute (output). The in-memory values handed
	// to Execute and downstream nodes are untouched.
	inputForEvent := inputJSON
	if toolName == privacy.FilterToolName {
		inputForEvent = redactJSONTextField(inputJSON)
	}

	if e.hooks.OnToolStarted != nil {
		e.hooks.OnToolStarted(node.ID, LLMToolStartedInfo{
			ToolName:  toolName,
			InputSize: len(inputJSON),
			Input:     json.RawMessage(inputForEvent),
		})
	}

	start := time.Now()
	outputStr, err := resolved.Execute(ctx, inputJSON)
	duration := time.Since(start)
	// Redacted before either hook, not between them: both feed the
	// persisted event log, so the output travels in one shape only.
	outputForEvent := outputStr
	if toolName == privacy.UnfilterToolName {
		outputForEvent = string(redactJSONTextField([]byte(outputStr)))
	}
	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(node.ID, LLMToolCallInfo{
			ToolName:  toolName,
			InputSize: len(inputJSON),
			Duration:  duration,
			Output:    outputForEvent,
			Error:     err,
		})
	}
	// Emit detailed tool I/O via the prompt hook (reused for tool node logging).
	if e.hooks.OnToolNodeResult != nil {
		e.hooks.OnToolNodeResult(node.ID, toolName, inputForEvent, outputForEvent, duration, err)
	}
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: execute: %w", node.ID, err)
	}
	if lost := e.deliverToolNodeFiles(node.ID, outputForEvent); lost != nil {
		return nil, lost
	}

	// Try to parse tool output as JSON map, otherwise wrap as text.
	// Registry-tool path preserves untrimmed stdout in the fallback;
	// the shell/script paths trim (see parseToolNodeOutput).
	return parseToolNodeOutput(outputStr, outputStr), nil
}

// emitToolNodeStarted fires the OnToolStarted hook for shell/script tool
// nodes (the byte-identical pre-exec sequence shared by
// executeToolNodeShell and executeToolNodeScript). The registry-tool path
// in executeToolNode uses a different payload shape (with Input bytes and
// privacy redaction) and intentionally does not use this helper.
func (e *ClawExecutor) emitToolNodeStarted(nodeID, toolName string, inputSize int) {
	if e.hooks.OnToolStarted == nil {
		return
	}
	e.hooks.OnToolStarted(nodeID, LLMToolStartedInfo{
		ToolName:  toolName,
		InputSize: inputSize,
	})
}

// emitToolNodeFinish fires OnToolCall and OnToolNodeResult for shell /
// script tool nodes (the byte-identical post-exec sequence shared by
// executeToolNodeShell and executeToolNodeScript). The registry-tool
// path uses a different payload shape (single combined output stream,
// privacy-redacted variants) and intentionally does not use this helper.
//
// It also hands over the files the output declared and returns their
// verdict — nil unless a `required=true` directive lost its file — so
// the delivery reads the SAME combined stream the event and the log
// carry, built once here.
func (e *ClawExecutor) emitToolNodeFinish(nodeID, toolName, resolved, stdout, stderr string, dur time.Duration, runErr error) error {
	// Both streams concatenated, stdout first so the structured payload is
	// visible at the top of long stderr dumps from yarn/npm/git.
	logged := combineStreamsForLog(stdout, stderr)
	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(nodeID, LLMToolCallInfo{
			ToolName: toolName,
			Duration: dur,
			// The same output the run log gets, on the event the
			// failure is audited from. Tool nodes are where the
			// deterministic gates live, and a gate whose event says
			// only "exit status 1" is a verdict nobody can read —
			// measured on a gate killed at a 4-hour wall that then had
			// to be resized without ever knowing what had spent it.
			// Same reason the LLM tool path keeps its output. Bounded
			// by the hooks layer: inline when small, sidecar blob or
			// capped preview when not.
			Output: logged,
			Error:  runErr,
		})
	}
	if e.hooks.OnToolNodeResult != nil {
		e.hooks.OnToolNodeResult(nodeID, toolName, []byte(resolved), logged, dur, runErr)
	}
	// A recipe that failed has its own story: the files it did not finish
	// writing are that failure's consequence, not a second verdict, and
	// the loss is on the run either way (the event). Only a recipe that
	// SUCCEEDED can be held to what it declared.
	if runErr != nil {
		return nil
	}
	return e.deliverToolNodeFiles(nodeID, logged)
}

// deliverToolNodeFiles fires OnToolNodeDeliverables and returns the
// verdict that fails the node: the loss of a file the node's output
// declared `required=true`. Nil when no hook is registered, when every
// declared file reached the run, and for every loss the author did not
// mark required — the directive's default contract is unchanged.
//
// The verdict is wrapped with the node id the way every other tool-node
// failure is, so an operator reading the run cannot tell it apart from
// the rest by its shape — only by what it says.
func (e *ClawExecutor) deliverToolNodeFiles(nodeID, output string) error {
	if e.hooks.OnToolNodeDeliverables == nil {
		return nil
	}
	if err := e.hooks.OnToolNodeDeliverables(nodeID, output); err != nil {
		return fmt.Errorf("model: tool node %q: %w", nodeID, err)
	}
	return nil
}

// parseToolNodeOutput parses stdout as a JSON object; on failure wraps
// `fallback` under the conventional `result` key. The two arguments
// differ across call sites: the registry-tool path passes the raw
// stdout for both (preserving any trailing newline in the fallback),
// while shell/script paths pass `strings.TrimSpace(stdout)` as the
// fallback so shell newlines don't leak into the structured wrapper.
// The unmarshal target itself is whitespace-tolerant (Go's json package
// skips leading/trailing whitespace), so the raw stdout is always
// acceptable for the parse attempt.
func parseToolNodeOutput(stdout, fallback string) map[string]any {
	output, _ := parseToolNodeOutputWrapped(stdout, fallback)
	return output
}

// parseToolNodeOutputWrapped is parseToolNodeOutput plus WHICH of the two
// shapes came back: wrapped reports that stdout did not parse and the map is
// the `result` text wrapper. A caller that knows the drain of this output was
// cut short needs that answer, because the two shapes differ in what they can
// hide — bytes that parsed carry no EVIDENCE of a cut, since a prefix of a
// JSON object does not parse at all, while the wrapper may be a prefix of
// anything. No evidence is not proof of whole: bytes cut where they happen
// to parse are published unmarked, and [toolNodeDrainCutKey] names that
// hole.
func parseToolNodeOutputWrapped(stdout, fallback string) (output map[string]any, wrapped bool) {
	if json.Unmarshal([]byte(stdout), &output) != nil {
		return map[string]any{"result": fallback}, true
	}
	return output, false
}

// executeToolNodeCommon is the shared execution skeleton for the two
// direct virtual-tool node modes — shell (executeToolNodeShell) and script
// (executeToolNodeScript). They differ only in how the final command
// string is produced (resolve) and how the *exec.Cmd is built (buildCmd);
// everything else is identical and lives here: the policy check, the
// OnToolStarted hook, the separated-stream run, the OnToolCall /
// OnToolNodeResult finish hooks, the error wrap, and the JSON-or-text
// output parse.
//
// resolve returns the final command/script string (env-expand, run-ref +
// template substitution, and — shell only — the compression rewrite). buildCmd
// turns that string into an *exec.Cmd, returning an optional cleanup (the
// script path uses it to remove its temp file) and an error (e.g.
// unsupported language / temp-file failure). buildCmd runs UNTIMED — only
// the subprocess execution itself is measured into duration — and the
// OnToolStarted hook fires only after it succeeds, so a build failure
// emits neither a started nor a finished event (matching the original
// per-mode functions). failVerb shapes the run-failure error ("shell
// command failed" / "script failed").
func (e *ClawExecutor) executeToolNodeCommon(
	ctx context.Context,
	node *ir.ToolNode,
	toolName, failVerb string,
	resolve func() string,
	buildCmd func(resolved string) (cmd toolCommand, cleanup func(), err error),
) (map[string]any, error) {
	res, setupErr := e.runToolNodeCore(ctx, node, toolName, resolve, buildCmd)
	if setupErr != nil {
		return nil, setupErr
	}
	if res.runErr != nil {
		return nil, fmt.Errorf("model: tool node %q: %s: %w\nstdout: %s\nstderr: %s", node.ID, failVerb, res.runErr, res.stdout, res.stderr)
	}
	// The recipe succeeded and a file it declared `required=true` did not
	// reach the run: the node did not do what it says it does.
	if res.deliveryErr != nil {
		return nil, res.deliveryErr
	}
	return res.output, nil
}

// recipeResult is the full outcome of running a tool node's recipe (shell
// or script): the parsed output map, the raw stdout/stderr streams, the
// resolved (placeholder-form) command, and the subprocess run error (nil
// on exit 0). The Verified Action ladder (executor_verified_action.go)
// consumes the streams + resolved command to drive self-repair, where
// executeToolNodeCommon only needs output + runErr.
type recipeResult struct {
	output   map[string]any
	stdout   string
	stderr   string
	resolved string
	runErr   error
	// deliveryErr is the verdict of handing over the files the output
	// declared: non-nil only when a `required=true` directive lost its
	// file (#2668). It is NOT a recipe failure — the recipe exited 0 —
	// so it never feeds the self-repair rungs, which would re-run a
	// command that did nothing wrong.
	deliveryErr error
}

// toolCommand is a tool node's command as runToolNodeCore runs it: the
// process to start and, when the sandbox runs it, the completion its stdout
// must carry (sandbox.Completion) — nil on the host, where the exit status is
// the process's own.
type toolCommand struct {
	cmd        *exec.Cmd
	completion *sandbox.Completion
}

// runToolNodeCore is the shared recipe-execution skeleton: policy check,
// resolve, build the *exec.Cmd, emit the started/finish hooks, run with
// separated streams, and parse the JSON-or-text output. It returns the
// recipeResult (carrying any subprocess run error in res.runErr) and a
// SEPARATE setup error for unrecoverable build/policy failures (which the
// recovery ladder must NOT try to repair). Extracted from
// executeToolNodeCommon so the Verified Action ladder can reuse the exact
// same execution path and observe the streams.
//
// Every caller judges a run by its EXIT STATUS, which is why the drain
// bound's cut is read here, once, instead of four times (#2855):
//   - executeToolNodeCommon — the recipe: a non-zero status fails the node;
//   - runVerifiedRecipe — rung 2 of the ladder: the status is recorded, and
//     the postcondition is what judges it (ADR-044);
//   - runPostcondition — the oracle itself, "exit 0 = met";
//   - selfRepair — the corrected command's re-run, judged by that same
//     oracle.
//
// A caller whose answer is the BYTES could not take this reading, and must
// not be added here: it calls runWithSeparateStreams itself, where exec's
// sentinel comes back unwrapped. The set above is not prose a fifth reader
// can join unnoticed — `TestEveryReaderOfRunToolNodeCoreNamesItsFamily`
// (tool_node_core_callers_test.go) is its inventory.
func (e *ClawExecutor) runToolNodeCore(
	ctx context.Context,
	node *ir.ToolNode,
	toolName string,
	resolve func() string,
	buildCmd func(resolved string) (cmd toolCommand, cleanup func(), err error),
) (recipeResult, error) {
	if err := e.checkToolNodePolicy(ctx, node, toolName, toolName, toolName); err != nil {
		return recipeResult{}, err
	}

	resolved := resolve()

	tc, cleanup, err := buildCmd(resolved)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return recipeResult{resolved: resolved}, err
	}

	e.emitToolNodeStarted(node.ID, toolName, len(resolved))

	start := time.Now()
	// Separate stdout (for structured JSON parsing) from stderr (for
	// diagnostic logging). Tools that emit a JSON result on stdout MUST be
	// able to use stderr for prose (yarn's resolution output, git's
	// `[detached HEAD ...]` line, a `script: js` body's console.error)
	// without that prose breaking the JSON parse downstream.
	// CombinedOutput() conflated the two and poisoned the parse.
	stdoutBytes, stderrStr, drainBound, runErr := runWithSeparateStreams(tc.cmd)
	duration := time.Since(start)
	drainCut := proc.DrainCutShort(runErr)
	if drainCut {
		// exec reports this sentinel for no other status: the command
		// exited 0 and only the copy of its output's tail was abandoned.
		// Every reader above judges by that status, so the success and the
		// answer stand and the drain is REPORTED — on the node's event,
		// which carries this stream, and in the run log, the way the claw
		// crossing already reports the pipes an agent's `server &` keeps.
		// Read before the sandbox settles its run: there the completion
		// line is what judges the command's end, and a sentinel left in
		// runErr would travel joined to its verdict. The cut itself travels
		// to the parse below, which is where a consumer of the OUTPUT can
		// still be told the bytes may stop mid-answer.
		//
		// The cut is also the node's TEARDOWN of what held the drain
		// (#2883): exec's context watcher hands its verdict to Wait as soon
		// as the direct child is REAPED, so the group kill
		// [proc.TerminateGroupOnCancel] arms is never dispatched once the
		// shell has exited — a leftover would otherwise survive the node's
		// whole teardown window (measured: a node under a 3 s deadline whose
		// command left `sleep 30 &` came back at 30 s, the group's kill
		// never sent). The group id is the child's own pid, recorded at
		// spawn, and the kill is delivered HERE — inside the window the
		// bound just closed, never stored for later, so it cannot land on a
		// group the OS built over the recycled id. What the signal cannot
		// reach (a descendant that escaped the group, an in-container job
		// behind a driver's exec client) keeps its own bound: the pipes the
		// cut closed under it, the sandbox teardown, the completion sweep.
		groupKilled, killErr := proc.KillProcessGroup(tc.cmd)
		if killErr != nil && e.logger != nil {
			e.logger.Warn("tool node %q: the group the command led could not be killed: %v", node.ID, killErr)
		}
		stderrStr = noteAbandonedDrain(stderrStr, drainBound, groupKilled)
		if e.logger != nil {
			e.logger.Warn("tool node %q: %s", node.ID, abandonedDrainReport(drainBound, groupKilled))
		}
		runErr = nil
	}
	// A client that never started ran nothing in the container: its own
	// error is the whole story, and there is no session to sweep.
	if tc.completion != nil && tc.cmd.Process != nil {
		stdoutBytes, runErr = e.settleSandboxedRun(ctx, node.ID, tc.completion, stdoutBytes, runErr)
	}
	outputStr := string(stdoutBytes)

	deliveryErr := e.emitToolNodeFinish(node.ID, toolName, resolved, outputStr, stderrStr, duration, runErr)

	output, wrapped := parseToolNodeOutputWrapped(outputStr, strings.TrimSpace(outputStr))
	if drainCut && wrapped {
		// The success and the bytes stand — the verdict is the exit status
		// — but these bytes did not parse as the node's answer, and the
		// bound abandoned the copy that would have completed them: a
		// downstream {{outputs.<node>.result}} reads what MAY be a prefix.
		// The mark is the only thing here that can tell it so; the report
		// of the bound and the knob rides the node's event and the run log.
		output[toolNodeDrainCutKey] = true
	}

	return recipeResult{
		output:      output,
		stdout:      outputStr,
		stderr:      stderrStr,
		resolved:    resolved,
		runErr:      runErr,
		deliveryErr: deliveryErr,
	}, nil
}

// executeToolNodeShell handles tool nodes whose command contains {{...}}
// template references. Templates are resolved from the node's input map,
// and the resulting string is executed as a shell command via sh -c.
func (e *ClawExecutor) executeToolNodeShell(ctx context.Context, node *ir.ToolNode, input map[string]any) (map[string]any, error) {
	resolve, buildCmd := e.shellRecipe(ctx, node, input)
	return e.executeToolNodeCommon(ctx, node, shellToolNodeToolName(node), "shell command failed", resolve, buildCmd)
}

// shellRecipe returns the resolve + buildCmd closures for a shell tool
// node, factored out of executeToolNodeShell so both the standard path
// and the Verified Action ladder share one source of truth for how a
// shell recipe is resolved and built.
func (e *ClawExecutor) shellRecipe(ctx context.Context, node *ir.ToolNode, input map[string]any) (func() string, func(resolved string) (toolCommand, func(), error)) {
	return func() string {
			// Expand environment variables FIRST, on the author-controlled
			// command template only. Doing this AFTER resolveCommandTemplate
			// would re-introduce shell metacharacters into substituted values
			// that shellEscape thought were inert single-quoted strings — e.g.
			// an upstream-LLM-controlled input of `$INJECT` would survive
			// shellEscape as `'$INJECT'`, then become `''; rm -rf ~; ''` if the
			// env had INJECT=`'; rm -rf ~; '`. By expanding before substitution,
			// only the .bot author's own `$VAR` references in the static
			// command template are expanded; substituted values stay quoted.
			//
			// Only the BRACED form `${NAME}` is treated as an env var reference.
			// Bare `$NAME`, `$ec`, `$?`, `$1` etc. pass through verbatim so
			// shell-level variables (positional args, exit-status, captured
			// stdout) survive into the resolved command for sh -c to interpret.
			expandedCommand, _ := ToolBodyEnv(node, node.Command, lookupToolEnv)
			// One snapshot, one pass — the same snapshot the prompts render
			// from. {{run.*}} and {{outputs.*}} resolve beside {{input.*}},
			// under the same shell escaping and the same missing-value rule.
			td := TemplateDataFromContext(ctx)
			resolved := resolveCommandTemplate(expandedCommand, node.CommandRefs, input, e.vars, td, RunIDFromContext(ctx), e.nodeShapes(node), e.secretGuard)
			// Compression (tool nodes): node-level opt-in ONLY — compresses
			// command output only when the node's own `compress:` is on/ultra (a
			// run override can force-off as a kill switch, never force-on), so a
			// review loop's `git diff` stays full-fidelity unless the author opts
			// in. Done before secretGuard.Materialize so hooks/logs persist the
			// placeholder (rewriter-form) command, never the materialised secret.
			// Shell-only: a script body (executeToolNodeScript) is not a shell
			// command line and never rewrites.
			// A command naming a secret is not compressed: the compressor
			// runs it with the value (rtk records every command it runs).
			// The rewrite runs host-side, in this engine process, even when the
			// command itself runs in-container — sandboxSkipsRewrite is the one
			// predicate consulting that sandbox signal, and its default under a
			// sandbox is to stand the chain down (#2007).
			if m := rewrite.ResolveToolNode(e.compressOverride, node.Compress); m.Enabled() && !e.secretGuard.NamesMaterializableSecret(resolved) && !e.secretGuard.NamesEnvOnlySecret(resolved) {
				if e.sandboxSkipsRewrite() {
					if e.logger != nil {
						e.logger.Warn("tool node %q: rewriter chain stands down (%s, default skip; =host restores the host-side chain) — the sandboxed command runs in-container verbatim, without the chain's 60–90%% output-token saving",
							node.ID, rewrite.SandboxEnv)
					}
				} else if rewritten, changed := e.chain.Rewrite(ctx, m, resolved); changed {
					resolved = rewritten
				}
			}
			return resolved
		},
		func(resolved string) (toolCommand, func(), error) {
			// Materialise secret placeholders into the command actually
			// executed via env-var indirection, not by inlining the raw
			// value into the command string: `resolved` (placeholder
			// form) is what the hooks/logs persist, and the exec'd
			// command line itself must not carry the plaintext secret
			// either — a command line is visible to any co-resident
			// local process via `ps`/`/proc/<pid>/cmdline` for the
			// subprocess's whole lifetime, unlike envp.
			// Authored: this is the node's own `command:`, so the env-only
			// class (the run's minted grant) resolves here. A command a
			// MODEL proposed — the self-repair rung — goes through the plain
			// materialiser, where it resolves to nothing.
			materialized, env := e.secretGuard.MaterializeAuthoredShellEnv(resolved)
			return e.toolNodeCommand(ctx, materialized, env), nil, nil
		}
}

// shellToolNodeToolName returns the canonical virtual tool name used for
// policy checks and hooks for direct shell-command tool nodes. Operators can
// allow a specific shell node with "shell:<nodeID>" or all shell nodes with
// "shell:*".
func shellToolNodeToolName(node *ir.ToolNode) string {
	return "shell:" + node.ID
}

// scriptToolNodeToolName returns the canonical virtual tool name used for
// policy checks and hooks for direct script tool nodes. Empty language
// defaults to "sh", matching scriptInterpreter. Operators can allow a
// specific script node with "script:<language>:<nodeID>" or scripts in a
// language with "script:<language>:*".
func scriptToolNodeToolName(node *ir.ToolNode) string {
	language := node.Language
	if language == "" {
		language = "sh"
	}
	return "script:" + language + ":" + node.ID
}

// checkToolNodePolicy applies the executor tool policy to all tool-node
// execution modes: registry tools and direct virtual shell/script tools.
// toolName and qualifiedName are the spelling the check matches on and the
// identity it matches as — the same values on every recipe that does not
// resolve an alias. On denial it emits OnToolCall with the policy error,
// matching failed executed tool calls, and returns an error wrapped with
// node and tool context.
func (e *ClawExecutor) checkToolNodePolicy(ctx context.Context, node *ir.ToolNode, matchName, qualifiedName, rawName string) error {
	if e.toolPolicy == nil {
		return nil
	}
	pctx := tool.PolicyContext{
		Ctx:               ctx,
		NodeID:            node.ID,
		NodeKind:          ir.NodeTool.String(),
		ToolName:          matchName,
		QualifiedToolName: qualifiedName,
		Vars:              e.vars,
		// Derived from the NODE rather than passed by each call site: this
		// function is the single check every recipe goes through, so reading
		// the property here is what makes a future recipe inherit the rule
		// instead of having to remember it.
		Deterministic:  node.Action != "",
		ResolvePattern: e.policyPatternResolver(ctx, node),
	}
	if err := e.toolPolicy.CheckContext(pctx); err != nil {
		if e.hooks.OnToolCall != nil {
			e.hooks.OnToolCall(node.ID, LLMToolCallInfo{
				ToolName: rawName,
				Error:    err,
			})
		}
		return fmt.Errorf("model: tool node %q: tool %q denied: %w", node.ID, rawName, err)
	}
	return nil
}

// toolNodePolicyIdentity resolves the identity a tool node's policy check
// matches on for an ALIAS spelling: the canonical name the alias tier
// resolves it to. Every other spelling — a canonical name, a bare MCP
// shorthand, an mcp__ FQN — compares exactly as written, so a policy
// verdict never depends on unrelated registry state (which MCP servers
// happened to boot earlier in the run). Read-only: it never boots an MCP
// server; the policy gate runs before the real resolution so a denied tool
// never pays a server boot.
func (e *ClawExecutor) toolNodePolicyIdentity(ctx context.Context, name string) (string, string) {
	if e.toolRegistry == nil || !tool.BuiltinAliasesEnabled(ctx) || toolcatalog.BuiltinAlias(name) == "" {
		return name, name
	}
	td, err := e.toolRegistry.ResolveWithAliases(name)
	if err != nil || td == nil {
		return name, name
	}
	return td.QualifiedName, td.QualifiedName
}

// toolNodeDrainBound is this site's default bound on the wait for a tool
// node's output once the command itself is gone, overridable through
// [proc.WaitDelayEnv].
//
// A tool node's command is the BOT's text, and backgrounding a process in it
// (`server &`) is an authored recipe, not an accident: the shell exits, the
// job it forked inherits the output pipe, and [exec.Cmd.Wait] ends only when
// EVERY holder of that pipe has closed it. Nothing else bounds that wait:
// `timeout:` on a tool node is read by the `action:` recipe alone
// ([ir.ToolNode.CallTimeout]) and warned as inert on a command or a script
// (C266, `TestOrphanTimeoutWarns`), and the cancel that signals the whole
// process GROUP
// ([proc.TerminateGroupOnCancel], installed by toolNodeCommand) does not
// reach it either — exec hands the context watcher's verdict to Wait as soon
// as the direct child is REAPED, so once the shell has exited that kill is
// never dispatched, whatever the context does afterwards (measured: a node
// under a 3 s deadline whose command left `sleep 30 &` came back at 30 s
// with no error). That is why the cut is also where the group kill lands
// (#2883): [proc.KillProcessGroup] is dispatched where the sentinel is read,
// inside the window the bound just closed, so the leftover that held the
// drain does not survive the node's teardown.
//
// Two seconds, the family's default: the copier drains the output WHILE the
// command runs, so what is left to read at its exit is a pipe buffer, not a
// transcript.
//
// It is the HOST's bound, and the readings above are the host path's —
// which is every run the engine did not place in a sandbox, not `none`
// alone: a declared `sandbox: auto` the host degraded to unsandboxed for
// want of a container runtime (the run's sandbox_skipped event, #1425)
// leaves e.sandbox nil, so its nodes take the branch below and read exactly
// like none's — this bound, this report, this mark, this override. On a
// node whose driver runs the command elsewhere, this Cmd is the DRIVER's
// exec client — [ClawExecutor.sandboxToolCommand] hands a
// [sandbox.HostRun] driver's command straight through instead, so the noop
// one stays on the host path — and the bound in force is then that
// client's: the driver's own when it set one — the kubernetes driver puts
// kubectlDrainBound on every kubectl it builds (cmdContext, through
// [proc.WaitDelayEnv] like this site, which is the only reason the override
// reaches it), and [runWithSeparateStreams] keeps that choice — this site's
// default otherwise, which is what the docker driver takes, having set
// none. What a cut can say there follows the same
// rule: [proc.DrainCutShort] is asked about the CLIENT's pipes. A driver
// that bounds its client raises exec's sentinel, which this site reports,
// and marks when the bytes in hand do not parse — about that client's
// drain. One that does not leaves this site reading a nil error, with no
// sentinel to report or to mark: measured with the docker CLI (29.9.2,
// image ghcr.io/socialgouv/iterion-sandbox-slim:edge; a podman client's
// grace is its own), `docker exec` itself stops copying some 2 s after the
// exec'd process exits and exits 0 — holders writing at 4 s and at 10 s
// both leave the command's own bytes in hand after 2.05 s, and the same
// recipe run at bounds of 10 ms, 100 ms, 2 s and unbounded comes back at
// 2.04–2.07 s alike, so no override widens that grace.
//
// Either way the reading says nothing about the in-container job that held
// the command's output: [toolNodeDrainCutKey] and the report describe the
// drain of what THIS process waited for, and a sandboxed node's answer
// carries no promise from them.
//
// A sandboxed node has one other thing to read, and it does not stand in
// for a sentinel that did not come. What the completion line proves is the
// command's END — it arrives after every byte the command wrote to stdout
// before it, and it is what judges a sandboxed command
// ([ClawExecutor.settleSandboxedRun]) — never that what followed that line
// is whole: this process holds only what the client copied to it. The one
// thing left to ask the container is whether the command's
// session still holds a process ([sandbox.Completion.Sweep] scans it), and a
// process proves a process: a job that redirected its own streams reads
// there exactly like one that held the output, and neither says a byte was
// lost. So a recipe whose answer comes from a process it did not wait for
// has nothing to read under a sandbox.
const toolNodeDrainBound = 2 * time.Second

// runWithSeparateStreams runs cmd with stdout and stderr captured into
// distinct buffers. Returns (stdout bytes, stderr string, the drain bound in
// force, run error).
// Use this for tool nodes where downstream needs to parse stdout as a
// structured payload while leaving stderr free for diagnostic chatter.
//
// The run error is exec's OWN, unwrapped — [exec.ErrWaitDelay] included,
// which is what comes back when the drain bound cut the copy of a finished
// command's output short. Reading that sentinel belongs to the CALLER,
// because the reading depends on what the caller answers with: a verdict
// that is the exit status keeps the success and reports the abandoned drain,
// one that is the BYTES cannot ([proc.DrainCutShort] holds both readings).
// The bound travels back so whoever reports names the bound that RAN, never
// this site's default, which a tightened override would make a lie.
func runWithSeparateStreams(cmd *exec.Cmd) ([]byte, string, time.Duration, error) {
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	// Before Start, which Output performs — Start reads the field to decide
	// whether to arm the context watcher — and only when no layer below chose
	// a bound of its own: a sandbox driver that bounded the client it built
	// (the kubernetes one, for a credential plugin holding kubectl's pipes)
	// keeps its choice, which it took through this same override.
	drainBound := cmd.WaitDelay
	if drainBound == 0 {
		drainBound = proc.SetWaitDelay(cmd, toolNodeDrainBound)
	}
	stdoutBytes, runErr := cmd.Output()
	// exec.ExitError carries stderr it captured before we set ours; the
	// buffer we provided is still the source of truth in our path.
	return stdoutBytes, stderrBuf.String(), drainBound, runErr
}

// toolNodeDrainCutKey marks a tool node's output as possibly SHORT of the
// answer: the drain bound abandoned the copy of this run's output, and what
// arrived did not parse as a JSON object, so the `result` wrapper beside the
// mark may be a prefix rather than the whole. The report of the bound and the
// knob goes on the node's event and in the run log; this is the same fact in
// the one place a consumer of the OUTPUT reads.
//
// Private and `_`-prefixed, the shape `_verified_action` already uses
// (ADR-044): [ValidateOutput] reads the schema's declared fields only, so the
// mark never fails a node whose output it describes. Unlike
// `_verified_action`, which the engine strips once it has emitted its event,
// this one is NOT stripped: it travels with the output into the artifact a
// `publish:` writes and into the node_finished event's data, where the reader
// who has to tell a prefix from an answer is.
//
// Its reach, exactly: a `{{outputs.<node>._drain_cut}}` ref resolves on
// EVERY tool node, whether it declares an `output:` schema or not. The rule
// is `isRuntimeInjectedField` (pkg/dsl/ir, validate_edges.go): an
// underscore-prefixed field is exempt from the outputs-ref validator
// (C031/C032) and from the static type checker, the exemption session
// metadata already travels on. Measured on a node declaring a schema whose
// only field is `result`: `{{outputs.held.nope}}` is C031 and
// `{{outputs.held._drain_cut}}` validates and resolves to `true`. The price
// of that exemption is that a MISSPELLING compiles too — `_drain_cutt` earns
// no diagnostic, and its raw `{{…}}` reaches `bash -c` as text.
//
// An edge gate is the other surface, and the schema does decide there:
// `node -> warn when _drain_cut` is C014 on a node that declares an
// `output:` schema, because the condition validator takes no such exemption
// (pkg/dsl/ir, validateConditionFields), and compiles and fires on a node
// that declares none. The engine's own event SUMMARY line skips
// `_`-prefixed keys by convention (engine_events.go), which is why the drain
// is reported in prose beside the node's event as well as marked here.
//
// What it cannot mark is named by the PARSE, not by a shape: a cut whose
// bytes still parse is published unmarked, whatever they parse TO. A command
// emitting one JSON object per line, cut after a whole one, parses and is
// unmarked; so is a bare `null` — what `cmd | jq '.absent'` prints — which
// parses to no map at all, so the output a `publish:` writes is nil and
// carries neither the mark nor the engine's own `_duration_ms`. Measured,
// all three with the drain cut and reported: `null` leaves a nil output,
// `{}` leaves `{"_duration_ms": …}` unmarked, and a lone space is marked
// because a space does not parse. The mark rides the wrapper because that is
// the shape this site can prove nothing about, never a claim that every lost
// byte is named.
const toolNodeDrainCutKey = "_drain_cut"

// abandonedDrainReport is what a tool node's run says when the drain bound
// stopped the wait for an output a process the command left behind still
// holds: what the site KNOWS — the command exited 0, something still holds
// the pipe, the wait stopped at the bound that ran — and the knob that
// widens it. groupKilled says whether the teardown that rides the cut
// ([proc.KillProcessGroup], dispatched where the cut is read) signalled the
// process group the command led; it is false on a driver whose client was
// never made a group leader and when the group was already gone, and the
// report then says nothing about a kill rather than claim one.
//
// Never that the output was "read short": what the bound cuts is the TAIL of
// a copy, and the measured case is a complete answer whose fd a descendant
// merely kept — this site cannot tell the two apart, so it reports only the
// one it can prove. The one shape where the difference is invisible to a
// reader of the OUTPUT carries [toolNodeDrainCutKey] instead.
func abandonedDrainReport(bound time.Duration, groupKilled bool) string {
	report := fmt.Sprintf("the command exited 0 and a process it left still holds its output; stopped waiting %s",
		proc.WaitDelayHint(bound))
	if groupKilled {
		report += "; the process group the command led was killed"
	}
	return report
}

// noteAbandonedDrain adds that report to the stderr the node's event and the
// run log carry, so the drain is auditable from the node itself and not only
// from the engine's log. Engine prose, marked as such: the directives a tool
// node's output declares are `[iterion] <key>=` lines, which this is not.
func noteAbandonedDrain(stderr string, bound time.Duration, groupKilled bool) string {
	note := "iterion: " + abandonedDrainReport(bound, groupKilled)
	if trimmed := strings.TrimRight(stderr, "\n"); trimmed != "" {
		return trimmed + "\n" + note
	}
	return note
}

// combineStreamsForLog formats stdout + stderr into a single string for
// run.log display. Empty streams are elided so the common
// "JSON on stdout, nothing on stderr" case stays compact.
func combineStreamsForLog(stdout, stderr string) string {
	stdout = strings.TrimRight(stdout, "\n")
	stderr = strings.TrimRight(stderr, "\n")
	switch {
	case stdout == "" && stderr == "":
		return ""
	case stderr == "":
		return stdout
	case stdout == "":
		return stderr
	default:
		return stdout + "\n--- stderr ---\n" + stderr
	}
}

// executeToolNodeScript handles tool nodes declared with `script:` +
// optional `language:`. The script body is resolved (template refs
// substituted), written to a temp file inside the workspace, and
// executed via the interpreter named by Language (defaulting to sh
// when empty). The temp file lives in the workspace so it is visible
// from inside the sandbox bind-mount, and is removed on success or
// failure.
func (e *ClawExecutor) executeToolNodeScript(ctx context.Context, node *ir.ToolNode, input map[string]any) (map[string]any, error) {
	resolve, buildCmd := e.scriptRecipe(ctx, node, input)
	return e.executeToolNodeCommon(ctx, node, scriptToolNodeToolName(node), "script failed", resolve, buildCmd)
}

// scriptRecipe returns the resolve + buildCmd closures for a script tool
// node, factored out of executeToolNodeScript so the standard path and
// the Verified Action ladder share one source of truth.
func (e *ClawExecutor) scriptRecipe(ctx context.Context, node *ir.ToolNode, input map[string]any) (func() string, func(resolved string) (toolCommand, func(), error)) {
	return func() string {
			// Same env-then-substitution ordering as executeToolNodeShell:
			// only the author-controlled `${NAME}` braces are env-expanded so
			// injected values from inputs/vars stay inert. But where the shell
			// path uses shell-escape, script: tools use resolveScriptTemplate
			// (JSON literal rendering) so values land as valid JS/Python/Ruby
			// literals — shell-escape's single-quote wrapping breaks
			// script-language string parsers when the value contains embedded
			// apostrophes. No compression: a script body is not a shell command line.
			expanded, _ := ToolBodyEnv(node, node.Script, lookupToolEnv)
			// Same single pass over one snapshot as the shell recipe:
			// {{run.*}} and {{outputs.*}} beside {{input.*}}, here as JSON
			// literals.
			td := TemplateDataFromContext(ctx)
			return resolveScriptTemplate(expanded, node.ScriptRefs, input, e.vars, td, RunIDFromContext(ctx), e.secretGuard)
		},
		func(resolved string) (toolCommand, func(), error) {
			interp, ext := scriptInterpreter(node.Language)
			if len(interp) == 0 {
				return toolCommand{}, nil, fmt.Errorf("model: tool node %q: unsupported language %q", node.ID, node.Language)
			}
			// The script file must be reachable from where the interpreter
			// runs. Out of the judged tree whenever a place exists that both
			// sides see at the same path: the host's temp dir for an
			// unsandboxed run, the shared state dir for a bind-mount sandbox
			// that has one. Only a copy-based sandbox, or a bind-mount without
			// a shared dir, still gets the file in the workspace (basename,
			// same relative path on both sides, pushed through the write-
			// through seam below where the driver copies) — a dotfile a gate
			// that judges the tree's cleanliness would otherwise read as
			// uncommitted work of the run. Removed on success or failure via
			// the returned cleanup.
			wd := e.workDir
			if wd == "" {
				wd = "."
			}
			_, copyBased := e.sandbox.(sandbox.WorkspaceFileRefresher)
			scriptDir, inWorkspace := scriptScratchDir(wd, e.sharedStateDir, e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut), copyBased)
			if !inWorkspace {
				if merr := os.MkdirAll(scriptDir, 0o755); merr != nil {
					return toolCommand{}, nil, fmt.Errorf("model: tool node %q: create script scratch dir: %w", node.ID, merr)
				}
			}
			tmpFile, err := os.CreateTemp(scriptDir, ".iterion-script-*"+ext)
			if err != nil {
				return toolCommand{}, nil, fmt.Errorf("model: tool node %q: create temp script: %w", node.ID, err)
			}
			tmpPath := tmpFile.Name()
			cleanup := func() { _ = os.Remove(tmpPath) }
			// Materialise secret placeholders into the executed script body
			// only. `resolved` (placeholder form) stays the value passed to
			// hooks/logs, so the real secret never reaches the event stream.
			body := e.secretGuard.MaterializeShell(resolved)
			// A script body is the author's own program: iterion cannot
			// rewrite a reference into a variable read for it, so the author
			// names the environment variable and the engine hands the value
			// to the interpreter — instead of the body (and the temp file it
			// is written to, which a copy-based sandbox pushes into the
			// workspace) carrying the credential in clear.
			scriptEnv := e.secretGuard.AuthoredEnvForBody(body)
			if _, werr := tmpFile.WriteString(body); werr != nil {
				_ = tmpFile.Close()
				cleanup()
				return toolCommand{}, nil, fmt.Errorf("model: tool node %q: write temp script: %w", node.ID, werr)
			}
			if cerr := tmpFile.Close(); cerr != nil {
				cleanup()
				return toolCommand{}, nil, fmt.Errorf("model: tool node %q: close temp script: %w", node.ID, cerr)
			}
			base := filepath.Base(tmpPath)
			// Out of the workspace, the interpreter is handed the absolute
			// path — the same on both sides by construction of the scratch.
			scriptArg := base
			if !inWorkspace {
				scriptArg = tmpPath
			}
			// Copy-based sandboxes (kubernetes: workspace tar-copied at
			// Prepare time) never see a host-side file created mid-run, so
			// the script must ALSO be pushed through the write-through seam.
			// The in-pod copy is removed on cleanup — a stray workspace
			// dotfile would otherwise be swept up by a later `git add -A`.
			if inWorkspace && e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
				if refresher, ok := e.sandbox.(sandbox.WorkspaceFileRefresher); ok {
					if rerr := refresher.RefreshWorkspaceFile(ctx, base, []byte(body)); rerr != nil {
						cleanup()
						return toolCommand{}, nil, fmt.Errorf("model: tool node %q: write script into sandbox: %w", node.ID, rerr)
					}
					hostCleanup := cleanup
					sb := e.sandbox
					cleanup = func() {
						hostCleanup()
						rmCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cancel()
						_, _ = sb.Exec(rmCtx, []string{"rm", "-f", base}, sandbox.ExecOpts{})
					}
				}
			}
			// In the workspace, just the basename (the bind mount uses the same
			// relative path); out of it, the absolute scratch path.
			return e.toolNodeScriptCommand(ctx, interp, scriptArg, scriptEnv), cleanup, nil
		}
}

// scriptInterpreter maps a `language:` token to the interpreter argv (the
// executable on PATH, then its flags) and a file extension hint (extension
// is informational, not required by any interpreter). An empty language
// defaults to sh.
//
// Python runs isolated (-I). Without it, python puts the script's directory
// first on sys.path, and the script file can land in the workspace (a
// copy-based sandbox): a json.py the judged tree carries would replace the
// standard module inside the node. -I also ignores PYTHON* variables and
// the user site, so a script body reaches the standard library and the
// system site-packages only.
func scriptInterpreter(language string) (argv []string, ext string) {
	switch language {
	case "", "sh":
		return []string{"sh"}, ".sh"
	case "bash":
		return []string{"bash"}, ".sh"
	case "js", "node":
		return []string{"node"}, ".js"
	case "py", "python", "python3":
		return []string{"python3", "-I"}, ".py"
	default:
		return nil, ""
	}
}

// withRunEnv is env (a fresh map, the caller's left as is) plus the
// rewriter chain's run env: every command a tool node runs — its recipe, its
// script, its postcondition, a self-repair's command — may run a rewriter
// itself, compressed or not, and keeps what it ran and printed out of the
// rewriter's own stores.
func (e *ClawExecutor) withRunEnv(env map[string]string) map[string]string {
	runEnv := e.chain.RunEnv()
	if len(runEnv) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(runEnv))
	maps.Copy(out, env)
	for _, kv := range runEnv {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// engineBinaryEnvEntry returns the key/value pair a tool command's
// environment carries for sandbox.EngineBinaryEnvVar, and whether one
// applies. Inside a sandbox the value is the container path the bind mount
// (addClawBinaryMount) and the runner image both place the engine's binary
// at — the host resolution would name a file the container cannot see, and
// a shared pid namespace does not change which FILESYSTEM resolves the path
// (sandbox.HostRun marks the one driver that executes on the host).
// Everywhere else it is the same proc.LocateIterionBinary resolution the
// MCP helpers get. Neither present, no entry: a reader the variable matters
// to fails on its own terms.
func (e *ClawExecutor) engineBinaryEnvEntry() (string, bool) {
	if e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
		if hr, ok := e.sandbox.(sandbox.HostRun); ok && hr.HostExecutesCommands() {
			// fall through to the host resolution: the command runs here.
		} else {
			return sandbox.EngineBinaryEnvVar + "=" + sandbox.EngineBinaryContainerPath, true
		}
	}
	if p := proc.LocateIterionBinary(); p != "" {
		return sandbox.EngineBinaryEnvVar + "=" + p, true
	}
	return "", false
}

// engineChildEnvEntries returns the entries the engine owns on every command
// it spawns against the workspace: the path of its own binary (when one
// resolves) and the invocation-class marker, which is unconditional — the
// class is a fact about the spawn, not about what resolved.
//
// The marker is what tells a nested iterion — the one a bot script re-enters
// through sandbox.EngineBinaryEnvVar, and every iterion below it by
// inheritance — that it is the engine's child and not a human's CLI call, so
// it does not auto-load the `.env` of the workspace it runs in (#2844,
// internal/envtrust EnvEngineChild).
func (e *ClawExecutor) engineChildEnvEntries() []string {
	entries := []string{envtrust.EngineChildEnvEntry()}
	if entry, ok := e.engineBinaryEnvEntry(); ok {
		entries = append(entries, entry)
	}
	return entries
}

// withEngineChildEnv applies those entries to a per-command env map (the
// sandbox path's shape), creating the map when the command has no env of its
// own.
func (e *ClawExecutor) withEngineChildEnv(env map[string]string) map[string]string {
	for _, entry := range e.engineChildEnvEntries() {
		k, v, _ := strings.Cut(entry, "=")
		if env == nil {
			env = map[string]string{}
		}
		env[k] = v
	}
	return env
}

// toolNodeScriptCommand returns the command that invokes the interpreter
// argv (scriptInterpreter) on the script temp file. Mirrors toolNodeCommand
// for the script-mode path: sandbox-routed if a sandbox is active and the
// node has not opted out.
func (e *ClawExecutor) toolNodeScriptCommand(ctx context.Context, interpreter []string, script string, env map[string]string) toolCommand {
	argv := append(append([]string{}, interpreter...), script)
	env = e.withRunEnv(env)
	if e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
		return e.sandboxToolCommand(ctx, argv, sandbox.ExecOpts{Env: e.withEngineChildEnv(env)})
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// A script body backgrounds jobs as freely as a shell recipe does, so
	// its lifetime ends with the node's context the same way — see
	// toolNodeCommand.
	proc.TerminateGroupOnCancel(cmd)
	// Host path only: sandboxed commands already see the variable from the
	// container env (the same dir is bind-mounted there). The run-level
	// env (the launch surface's layer plus the engine's PATH composition)
	// is appended after the inherited env so on a duplicate key it wins.
	cmd.Env = os.Environ()
	if runLevelEnv := e.processExtraEnv(); e.artifactFilesDir != "" || len(runLevelEnv) > 0 {
		cmd.Env = append(cmd.Env, runLevelEnv...)
		if e.artifactFilesDir != "" {
			cmd.Env = append(cmd.Env, "ITERION_ARTIFACT_FILES_DIR="+e.artifactFilesDir)
		}
	}
	cmd.Env = append(cmd.Env, e.chain.RunEnv()...)
	// The values the body named, keyed by the variable it reads.
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// The canonical tree-noise pathspecs, engine-owned: a run without a
	// devbox.json carries them just the same (#1464) — but the operator,
	// the run or the node wins when they set the variable themselves, as
	// everywhere else (verdict 2 R05b122, verdict 3 R5478b3).
	cmd.Env = append(cmd.Env, e.treeNoiseEnvAppend(env)...)
	// The engine's own binary path and the invocation-class marker,
	// engine-owned: appended last so no inherited or launch-surface value
	// can forge the resolution a bot script trusts when it re-enters the
	// CLI (#1799), nor claim the re-entered iterion is a human's call
	// (#2844).
	cmd.Env = append(cmd.Env, e.engineChildEnvEntries()...)

	if e.workDir != "" {
		cmd.Dir = e.workDir
	}
	return toolCommand{cmd: cmd}
}

// streamedRecipeRunner runs a recipe streamed on its stdin once all of it has
// arrived ($1 is its size): the recipe lands in a private temp file, refused
// unless its size is $1, and runs as `bash -s` reading that file, already
// unlinked — the recipe means what `bash -s` makes of it, a `read` in it
// reading its next lines included, and exec keeps it in the process that
// leads its session and group. Without mktemp, the file is created
// exclusively under a name of its own.
const streamedRecipeRunner = `umask 077
t=$(mktemp 2>/dev/null) || { t="${TMPDIR:-/tmp}/iterion-recipe.$$"; (set -C; : > "$t") 2>/dev/null || exit 125; }
cat > "$t" || { rm -f "$t"; exit 125; }
n=$(( $(wc -c < "$t") ))
[ "$n" -eq "$1" ] || { rm -f "$t"; echo "iterion: the recipe arrived cut short: $n of $1 bytes" >&2; exit 125; }
exec 3< "$t"
rm -f "$t"
exec bash -s <&3 3<&-`

// sandboxToolCommand routes a tool node's argv through the sandbox under a
// completion (sandbox.Completion): the exec client's exit status is not proof
// that the command ended, the command's own completion line is. Every tool
// node command the sandbox runs is built here, so none runs unproven. A
// driver that runs commands on the engine's own host hands back the process
// itself: its exit status is the command's own, there is no stream to lose,
// and the command runs as it is.
func (e *ClawExecutor) sandboxToolCommand(ctx context.Context, argv []string, opts sandbox.ExecOpts) toolCommand {
	if hr, ok := e.sandbox.(sandbox.HostRun); ok && hr.HostExecutesCommands() {
		return toolCommand{cmd: e.sandbox.Command(ctx, argv, opts)}
	}
	completion := sandbox.NewCompletion("tool")
	env := make(map[string]string, len(opts.Env)+1)
	maps.Copy(env, opts.Env)
	key, value := completion.SessionEnv()
	env[key] = value
	opts.Env = env
	return toolCommand{cmd: e.sandbox.Command(ctx, completion.Wrap(argv), opts), completion: completion}
}

// settleSandboxedRun judges a command the sandbox ran by its completion line,
// not by the exec client's status alone, and returns stdout without the line:
//   - the line found: the command ended, and it succeeded only if the client
//     and the line both say so. A proven end sweeps nothing: a command may
//     leave a service running for the nodes after it;
//   - the line missing: the exec session ended before the command did, or its
//     end never arrived. The command's session is swept, so that what it left
//     running cannot outlive the node and keep writing to a workspace the next
//     node works in, and the run fails with a sandbox.ExecStatusUnknownError —
//     never as a success carrying a partial output. A sweep that could not
//     prove the command stopped — it never ran to its end within its budget,
//     or something it signalled survived — makes it an ambiguous effect: no
//     automatic retry starts a second copy beside a command that may still
//     be running.
//
// A cancelled node keeps its own error — the cancel explains a missing line —
// unless its sweep could not finish: the command may then outlive the cancel,
// and the error says so the same way. A cancel that lands after the client
// reported success leaves the line to decide.
func (e *ClawExecutor) settleSandboxedRun(ctx context.Context, nodeID string, completion *sandbox.Completion, stdout []byte, runErr error) ([]byte, error) {
	clean, status, ended := completion.Settle(stdout)
	if ended {
		if status != 0 && runErr == nil {
			// Not "the client reported success": runToolNodeCore clears the
			// drain bound's sentinel before this settle runs, so a nil
			// runErr here is a client that reported nothing this site can
			// charge the command with — success, or a drain it abandoned.
			runErr = fmt.Errorf("sandbox: the command exited %d but its exec client did not report that status", status)
		}
		return clean, runErr
	}
	budget := cmp.Or(e.sweepBudget, toolSweepBudget)
	if ctx.Err() != nil {
		budget = cmp.Or(e.sweepBudgetOnCancel, toolSweepBudgetOnCancel)
	}
	outcome := completion.Sweep(e.sandbox, budget)
	unknown := &sandbox.ExecStatusUnknownError{Swept: outcome.Stopped(), Detail: describeToolSweep(outcome)}
	if ctx.Err() == nil {
		return stdout, errors.Join(runErr, unknown)
	}
	if len(outcome.Report.Stopped) > 0 || outcome.Err != nil {
		e.logger.Warn("tool node %q: the node ended before its command — %s", nodeID, unknown.Detail)
	}
	if runErr == nil {
		runErr = fmt.Errorf("the node ended before its command's end was proven: %w", ctx.Err())
	}
	if unknown.AmbiguousEffect() {
		return stdout, errors.Join(runErr, unknown)
	}
	return stdout, runErr
}

// toolSweepBudget bounds the retries of a sweep after a live node's command
// went unproven — about what recovery spends retrying a network outage
// before it gives up: an exec path down that long leaves the command's state
// to a human. A cancelled node's run is ending: its sweep is tried briefly.
const (
	toolSweepBudget         = 3 * time.Minute
	toolSweepBudgetOnCancel = 10 * time.Second
)

// describeToolSweep says what a tool command's sweep stopped and what it
// could not do.
func describeToolSweep(o sandbox.SweepOutcome) string {
	var parts []string
	if labels := o.Report.Labels(); len(labels) > 0 {
		parts = append(parts, "the sweep stopped "+strings.Join(labels, "; "))
	} else if o.Ran {
		parts = append(parts, "the sweep found nothing left running")
	}
	switch {
	case !o.Stopped():
		parts = append(parts, "the sweep could not prove the command stopped, it may still be running: "+fmt.Sprint(o.Err))
	case o.Err != nil:
		parts = append(parts, "the sweep could not account for everything: "+o.Err.Error())
	}
	return strings.Join(parts, "; ")
}

// treeNoiseEnvAppend returns the ITERION_TREE_NOISE entry to append to a
// host tool command's environment: the canonical list — unless the
// variable is already set by the node's env map (MaterializeShellEnv's
// output; no DSL surface carries this name), the run's env, or the
// operator's own environment, in which case nothing is appended: an
// explicit choice is never silently replaced (verdicts 2-3, #1464).
func (e *ClawExecutor) treeNoiseEnvAppend(nodeEnv map[string]string) []string {
	if value, set := nodeEnv[treenoise.TreeNoiseEnvVar]; set && value != "" {
		return nil
	}
	if value, inherited := os.LookupEnv(treenoise.TreeNoiseEnvVar); inherited && value != "" {
		return nil
	}
	for _, entry := range e.runExtraEnv {
		// An explicitly EMPTY run-level value is not a claim (verdict 9
		// on its fourth surface): the node-env, operator and sandbox
		// branches all treat empty as no-claim, and a launch projecting
		// the variable empty must not silence the host gate while the
		// same launch sandboxed gets the canonical list.
		if entry == treenoise.TreeNoiseEnvVar+"=" {
			continue
		}
		if strings.HasPrefix(entry, treenoise.TreeNoiseEnvVar+"=") {
			return nil
		}
	}
	return []string{treenoise.TreeNoiseEnvVar + "=" + treenoise.EnvValue()}
}

// toolNodeCommand returns a configured *exec.Cmd for a tool node's
// shell snippet. When the run is sandboxed and the node has not opted
// out (`sandbox: none` at node scope), the command is routed through
// the sandbox via [sandbox.Run.Command]; otherwise it is the
// pre-sandbox host invocation.
//
// env carries any secret values materialised by MaterializeShellEnv,
// keyed by the env-var name `resolved` references as "$NAME" — it is
// applied to the CHILD PROCESS's environment only (sandbox.ExecOpts.Env
// on the sandboxed branch, cmd.Env on the host branch), never inlined
// into `resolved` itself. This is what keeps a secret out of the exec'd
// command's own argv, which — unlike envp — is visible to any
// co-resident local process via `ps`/`/proc/<pid>/cmdline` for the
// subprocess's whole lifetime.
//
// Per-node opt-out lets a workflow run mostly sandboxed but cherry-pick
// a tool node that needs host access (e.g. `gh` configured against
// the host's keychain).
//
// Shell selection: we invoke `bash -c` (not `sh -c`) so recipe authors
// can use modern bashisms (`set -o pipefail`, arrays `BLOCKERS=()`,
// `[[ ... ]]`, process substitution, etc.) without surprises. On
// Debian/Ubuntu-derived containers and hosts, `/bin/sh` is dash —
// strict POSIX, no arrays, no pipefail, errors with
// `Syntax error: "(" unexpected` on `X=()`. The iterion-sandbox-slim
// image ships bash 5+; modern Linux desktops always have bash. If
// bash is genuinely absent (extremely minimal containers), the recipe
// author should either install bash via post_create or rewrite the
// tool body in POSIX shell.
func (e *ClawExecutor) toolNodeCommand(ctx context.Context, resolved string, env map[string]string) toolCommand {
	// A command template may contain a host-attested editor document. Passing
	// that complete shell program as one argv element hits Linux's per-argument
	// limit before bash starts (the Studio editor allows 160 KiB). Mirror the
	// sandbox drivers' proven 100 KiB threshold and stream oversized programs
	// through stdin instead. The short command path remains byte-for-byte
	// unchanged.
	const maxInlineToolCommandBytes = 100_000
	args := []string{"bash", "-c", resolved}
	var stdin io.Reader
	if len(resolved) > maxInlineToolCommandBytes {
		args = []string{"bash", "-s"}
		stdin = strings.NewReader(resolved)
	}
	env = e.withRunEnv(env)
	if e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
		if stdin != nil {
			// Streamed to the container, the program is written to a file and
			// run only once the file's size is the size sent: a stream cut short
			// exits 125 naming what arrived, never a prefix that runs and exits
			// 0 — a completion line proves the program's end, not that all of
			// it arrived. Run as a file, it means what `bash -s` makes of it.
			args = []string{"bash", "-c", streamedRecipeRunner, "iterion-recipe", strconv.Itoa(len(resolved))}
		}
		return e.sandboxToolCommand(ctx, args, sandbox.ExecOpts{Env: e.withEngineChildEnv(env), Stdin: stdin})
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// A tool node's lifetime is the node's. `bash -c` routinely backgrounds
	// jobs (`&`, a daemon a build script starts), and those grandchildren
	// inherit our stdout/stderr pipes: killing only the shell leaves the
	// read blocked, so cancelling the run would stop the wait and not the
	// work. Signal the whole group instead.
	proc.TerminateGroupOnCancel(cmd)
	cmd.Stdin = stdin
	runLevelEnv := e.processExtraEnv()
	cmd.Env = os.Environ()
	// Run-level env (the launch surface's layer plus the engine's
	// PATH composition) — appended after the inherited env so on a
	// duplicate key it wins.
	cmd.Env = append(cmd.Env, runLevelEnv...)
	// Host path only: sandboxed commands already see the variable from
	// the container env (the same dir is bind-mounted there).
	if e.artifactFilesDir != "" {
		cmd.Env = append(cmd.Env, "ITERION_ARTIFACT_FILES_DIR="+e.artifactFilesDir)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// The canonical tree-noise pathspecs, engine-owned: a run without a
	// devbox.json carries them just the same (#1464) — but the operator,
	// the run or the node wins when they set the variable themselves, as
	// everywhere else (verdict 2 R05b122, verdict 3 R5478b3).
	cmd.Env = append(cmd.Env, e.treeNoiseEnvAppend(env)...)
	// The engine's own binary path and the invocation-class marker,
	// engine-owned: appended last so no inherited or launch-surface value
	// can forge the resolution a bot script trusts when it re-enters the
	// CLI (#1799), nor claim the re-entered iterion is a human's call
	// (#2844).
	cmd.Env = append(cmd.Env, e.engineChildEnvEntries()...)
	if e.workDir != "" {
		cmd.Dir = e.workDir
	}
	return toolCommand{cmd: cmd}
}

// nodeOptOut classifies the kind of node being inspected for sandbox
// opt-out purposes. The current callers all examine the tool-node
// path, but the broadcast-style API leaves room for agent/judge
// routing later.
type nodeOptOut int

const (
	toolNodeOptOut nodeOptOut = iota
)

// nodeOptsOutOfSandbox reports whether the node currently being
// executed declared `sandbox: none` and therefore wants to run on
// the host even though the workflow has an active sandbox.
//
// Phase 1 keeps this simple: there is no per-call node context, so
// the executor cannot consult per-node overrides here. The hook is
// in place for Phase 2 where engine + executor pass the in-flight
// node identifier through. Returning false today preserves the
// "sandbox active = everything sandboxed" guarantee.
func (e *ClawExecutor) nodeOptsOutOfSandbox(_ nodeOptOut) bool {
	return false
}

// sandboxSkipsRewrite is the tool-node half of the #2007 placement decision:
// the one predicate consulting the sandbox signal before a tool node's
// command goes through the rewriter chain host-side. Its default under a
// sandbox is to skip; ITERION_REWRITE_SANDBOX=host restores the previous
// placement. The knob's value is read once at construction
// (rewriteSandboxEnv); a node that opted out of the sandbox runs its command
// on the host, so the host-side rewrite costs the boundary nothing there.
func (e *ClawExecutor) sandboxSkipsRewrite() bool {
	sandboxed := e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut)
	return rewrite.SkipsSandbox(e.rewriteSandboxEnv, sandboxed)
}

// resolveCommandTemplate substitutes {{input.X}}, {{vars.X}}, {{secrets.X}},
// {{outputs.<node>.<field>}} and {{run.X}} references in a command string —
// the input map, the workflow variables, the secret guard's placeholders,
// and the template snapshot td (the same one the prompts render from;
// nil-tolerant). ctxRunID is the run identity for hosts that wire only
// WithRunID and no snapshot; it answers for `{{run.id}}` alone. Values are
// shell-escaped to prevent command injection when the resolved string is
// passed to sh -c.
//
// Refs flagged as `Raw` (authored as `{{!input.X}}` / `{{!vars.X}}`) bypass
// shellEscape and are inserted verbatim. Use only for trusted values that
// are intentionally shell snippets (e.g. an upstream node returns a
// command line that the wrapping tool needs to RE-INTERPRET as shell, not
// pass as a single quoted token). Untrusted external inputs MUST keep the
// default escaping.
func resolveCommandTemplate(command string, refs []*ir.Ref, input map[string]any, vars map[string]any, td *TemplateData, ctxRunID string, shapes *Shapes, guards ...*secretguard.Guard) string {
	var guard *secretguard.Guard
	if len(guards) > 0 {
		guard = guards[0]
	}
	// Shell commands preserve `{{input.X}}` literal text for missing
	// values — sh -c sees the placeholder and either fails informatively
	// or the operator notices. Substituting silently would lose that
	// signal and could mask wiring bugs. An `{{outputs.*}}` ref whose node
	// has not produced, and a `{{run.*}}` member the namespace does not
	// carry, take the same rule.
	// Pinned by TestToolCommandRefsAreShellEscaped: the shell itself is the
	// oracle there, because reading a bot's `VAR={{vars.x}}` as unquoted is a
	// mistake that has already been made confidently.
	return renderCommand(command, refs, input, vars, td, ctxRunID, guard, shapes, nil)
}

// resolveScriptTemplate substitutes refs in a tool node's `script:` body.
// SCRIPT contexts (JS / Python / Ruby / any JSON-superset language) want
// JSON-encoded values, not shell-escaped ones — shell-escape wraps
// strings in single quotes (and renders embedded apostrophes as the
// shell-only `'\”` escape sequence) which then breaks the script
// language's string-literal parser. JSON encoding produces valid
// literals in all major scripting languages: `"foo"` is a JS / Python /
// Ruby string, `{"k":"v"}` is an object/dict literal, `[1,2,3]` is an
// array/list literal.
//
// The bang form `{{!input.X}}` keeps the legacy raw-passthrough
// behaviour (strings inserted unquoted) for authors who need to drop
// a snippet of source directly into the script body.
func resolveScriptTemplate(script string, refs []*ir.Ref, input map[string]any, vars map[string]any, td *TemplateData, ctxRunID string, guards ...*secretguard.Guard) string {
	var guard *secretguard.Guard
	if len(guards) > 0 {
		guard = guards[0]
	}
	// Script bodies (JS/Python/Ruby/…) must always parse — a nil-valued
	// ref left as raw `{{input.X}}` text crashes the interpreter at
	// parse time before any user logic can react. Substitute with the
	// language's null literal (rendered as JSON null = "null") so the
	// script can still run and handle the missing input itself.
	return renderScript(script, refs, input, vars, td, ctxRunID, guard, nil)
}

// renderCommand and renderScript are the two readings of a tool body —
// shell-escaped values with a hole kept as written, JSON literals with a
// hole as null — named once each so the production path and a dry run
// cannot choose differently.
func renderCommand(command string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string, guard *secretguard.Guard, shapes *Shapes, unresolved func(ref string)) string {
	return resolveTemplateWith(command, refs, input, vars, td, ctxRunID, guard, shapes, true, shellEscapeValue, false, unresolved)
}

// A script body carries no declared shape: every namespace renders as a
// JSON literal there, which is already what both list types mean to a
// JS/Python/Ruby parser — `["a","b"]` for a `string[]`, the parsed document
// for a `json`. Passing the declarations would be wiring nothing reads.
func renderScript(script string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string, guard *secretguard.Guard, unresolved func(ref string)) string {
	return resolveTemplateWith(script, refs, input, vars, td, ctxRunID, guard, nil, false, jsonLiteralValue, true, unresolved)
}

// resolveTemplateWith is the shared core: walk refs, look up each value,
// dispatch to rawTemplateValue (bang form) or the renderer the caller
// provided (default form). Keeps shell- and script-mode template logic
// in one place.
//
// Substitution is single-pass over the original template: we build a
// map[ref.Raw]rendered and then scan the template once, replacing
// each {{...}} occurrence by its rendered value. A strings.ReplaceAll
// loop would feed each substitution's output back into subsequent
// passes, so a value that happened to contain a {{...}} literal
// matching a later ref would be silently rewritten (the "cascade"
// bug). The single-pass walk only touches positions that were in the
// source template — which is why EVERY namespace a tool body can
// reference resolves here, in this one walk, rather than in a pre-pass
// of its own.
//
// unresolved, when a caller listens, receives each reference that resolved
// to nothing — kept as written in a shell body, null in a script body — as
// `namespace.path`. It is the only reading of what was left: the rendered
// text is never re-read for braces, since a value may carry `{{…}}` of its
// own and that is the value, not a reference. The production callers
// listen to none; a dry run names them.
func resolveTemplateWith(template string, refs []*ir.Ref, input map[string]any, vars map[string]any, td *TemplateData, ctxRunID string, guard *secretguard.Guard, shapes *Shapes, shellBody bool, defaultRender func(any, ValueShape) string, substituteNil bool, unresolved func(ref string)) string {
	if len(refs) == 0 {
		return template
	}
	subs := make(map[string]string, len(refs))
	for _, ref := range refs {
		if ref.Kind == ir.RefLiteralOpen {
			// Delimiter escaping is source syntax, not a dynamic value. Do
			// not shell-escape/JSON-quote it or rescan the emitted braces.
			subs[ref.Raw] = "{{"
			continue
		}
		// A credential the server minted for this run
		// (store.ServerMintedSecretVars) renders as the guard's placeholder,
		// single-quoted: exactly the form MaterializeAuthoredShellEnv swaps
		// for a reference to the CHILD's environment, so the bearer never
		// enters a command line — which any co-resident process reads from
		// /proc/<pid>/cmdline for the whole life of the command.
		//
		// The placeholder is ASKED of the guard, never derived from the
		// name: only one this guard will materialise may take a value's
		// place, or the command would run with a reference resolving to
		// nothing. Emitted here rather than through a renderer below,
		// because the text and its quoting are one decision: no declared
		// shape can reshape it into a form the materialiser no longer
		// recognises.
		//
		// Shell bodies only: a script body's value is a language literal,
		// materialised into the script file itself. The raw {{!vars.x}} form
		// is the author's own text, quoting included, and keeps the value.
		if shellBody && !ref.Unquoted && ref.Kind == ir.RefVars && len(ref.Path) == 1 {
			if ph := guard.AuthoredEnvPlaceholder(ref.Path[0]); ph != "" {
				subs[ref.Raw] = "'" + ph + "'"
				continue
			}
		}
		// The same mask where NO materialiser stands behind the reference —
		// the preview renderings (RenderCommand/RenderScript) pass a nil
		// guard, so the placeholder the branch above would emit resolves
		// nowhere here. The value must still not render: freeze the
		// identity, as the prompt path does (TemplateResolver). A shell
		// body gets the placeholder in the one form the run renders it;
		// a script body gets it as the language literal its value would
		// have been. A guard — the run — keeps its own reading (it either
		// holds the name env-only and took the branch above, or it
		// deliberately did not register the value), and the raw
		// {{!vars.x}} form keeps the value, as it does on the run: it is
		// the author's own text.
		if guard == nil && !ref.Unquoted && ref.Kind == ir.RefVars && len(ref.Path) == 1 {
			if ph, masked := mintedVarMask(ref.Path[0], vars[ref.Path[0]]); masked {
				if shellBody {
					subs[ref.Raw] = "'" + ph + "'"
				} else {
					subs[ref.Raw] = defaultRender(ph, shapes.Of(ref))
				}
				continue
			}
		}
		var val any
		var handled, present bool
		switch {
		case ref.Kind == ir.RefInput && len(ref.Path) > 0:
			// Drilled to the leaf, as a prompt reads it: `{{input.a.b}}` is
			// the field b of a, not the whole of a — one reading of a
			// reference, whichever body holds it. A missing leaf is nil and
			// takes the missing-value rule below.
			val, present = drillTemplatePath(input, ref.Path)
			handled = true
		case ref.Kind == ir.RefVars && len(ref.Path) > 0:
			// Drilled to the leaf, like input and outputs: `{{vars.cfg.on}}`
			// is the member `on` of the `json` var cfg. Reading only
			// vars[path[0]] answered the whole document here while an
			// expression on the same text answered the member — one
			// reference, three surfaces, three values.
			val, present = drillTemplatePath(vars, ref.Path)
			handled = true
		case ref.Kind == ir.RefOutputs && len(ref.Path) > 0:
			// The snapshot the prompts render from. An output not yet
			// produced (or no snapshot at all) is nil here and takes the
			// same missing-value rule as an absent input below — the
			// placeholder in a shell body, `null` in a script body — so
			// the two namespaces cannot disagree about a hole.
			val, present = outputsTemplateValue(td, ref.Path)
			handled = true
		case ref.Kind == ir.RefRun && len(ref.Path) > 0:
			// The engine's `run.*` namespace — identity plus the run's
			// consumption and effective budget caps — read through the
			// same runNamespaceValue the prompt path uses, so a member
			// cannot resolve in a prompt and stay literal in a command.
			// The snapshot answers for every member, `id` included, which
			// is what makes a command inside a fan-out branch render like
			// the same command on the trunk; ctxRunID is the fallback for
			// hosts that wire only WithRunID. A member the namespace does
			// not carry is nil here and takes the shared missing-value
			// rule below — the placeholder in a shell body, `null` in a
			// script body — instead of vanishing from the command line.
			val, _ = runNamespaceValue(ctxRunID, td, ref.Path[0])
			handled = true
		case ref.Kind == ir.RefSecrets && len(ref.Path) > 0:
			// Render the opaque placeholder into the command; the real
			// value is swapped in by the secret guard immediately before
			// exec (executeToolNodeShell / Script), never landing in the
			// persisted command text or logs. File secrets render their
			// mounted path instead.
			if guard != nil {
				val = guard.ResolveSecretRef(ref.Path[0])
			}
			if val == nil || val == "" {
				val = secretguard.PlaceholderForName(ref.Path[0])
			}
			handled = true
		case ref.Kind == ir.RefArtifacts || ref.Kind == ir.RefAttachments || ref.Kind == ir.RefLoop:
			// The namespaces a prompt resolves — artifacts, attachments, loop
			// — resolve here through the shared resolver, so no reference
			// resolves in a prompt and stays literal in a command (a dry run
			// found `{{attachments.x}}` handed to the shell as written). The
			// expression is the parsed reference's, not the raw text's: the
			// parser has already read the bang and the spaces. The VALUE,
			// not the prompt's text of it: a script body gets a counter as
			// a number and an artifact as an object, through the renderer
			// below like every other namespace. A value the snapshot has
			// not is nil and takes the missing-value rule below.
			if v, ok := (&TemplateResolver{Vars: vars}).ResolveValue(refExpr(ref), input, td); ok {
				val = v
			}
			handled = true
		}
		if !handled {
			if unresolved != nil {
				unresolved(refExpr(ref))
			}
			continue
		}
		shape := shapes.Of(ref)
		// A `json` slot whose value is PRESENT and null holds a value —
		// JSON null — not a hole, and `null` is its text.
		//
		// A `string[]` one does NOT take that rule: an empty list renders
		// no token, which is right for a list the author wrote empty and
		// wrong for a producer that had nothing to say — the argument
		// would vanish and the next one shift into its place, in the one
		// configuration where the author DECLARED the slot. Null there is
		// the missing-value case and keeps its placeholder.
		//
		// substituteNil controls what the missing-value rule does: shell
		// contexts keep the raw `{{input.X}}` placeholder so a missing
		// wiring is visible; script contexts MUST render (the renderer
		// turns nil into "null") because a JS/Python/Ruby parser otherwise
		// crashes on the literal braces before any script logic runs.
		if val == nil && (!present || shape != ShapeJSON) {
			if unresolved != nil {
				unresolved(refExpr(ref))
			}
			if !substituteNil {
				continue
			}
		}
		if ref.Unquoted {
			subs[ref.Raw] = rawTemplateValue(val, ShapeUndeclared)
		} else {
			subs[ref.Raw] = defaultRender(val, shape)
		}
		// A placeholder that arrives through DATA — an input an edge mapped
		// from a model's output, an upstream output, a launch var — was
		// written by a model, not by the author: it is the agent sink's,
		// whatever body it lands in. A secret kept off the agent is defused
		// here, before the body's materialiser scans the rendered text and
		// would resolve it under the body's own grant.
		if ref.Kind != ir.RefSecrets {
			subs[ref.Raw] = guard.DefuseUngranted(secretsink.Agent, subs[ref.Raw])
		}
	}
	if len(subs) == 0 {
		return template
	}
	var b strings.Builder
	b.Grow(len(template))
	i := 0
	for i < len(template) {
		if i+1 < len(template) && template[i] == '{' && template[i+1] == '{' {
			end := strings.Index(template[i:], "}}")
			if end == -1 {
				b.WriteString(template[i:])
				return b.String()
			}
			raw := template[i : i+end+2]
			if rendered, ok := subs[raw]; ok {
				b.WriteString(rendered)
			} else {
				b.WriteString(raw)
			}
			i += end + 2
			continue
		}
		b.WriteByte(template[i])
		i++
	}
	return b.String()
}

// refExpr is the reference as the resolver reads it, `namespace.path`,
// from the parsed reference — never from its raw text, whose bang and
// spaces the parser has already consumed.
func refExpr(ref *ir.Ref) string {
	return ref.Kind.String() + "." + strings.Join(ref.Path, ".")
}

// jsonLiteralValue renders val as a valid JSON literal suitable for
// pasting into a script-language source file. Strings get JSON-quoted
// (`"foo"`, with embedded quotes escaped as `\"`); maps and slices get
// JSON-encoded as object/array literals. Numbers, bools, and nil are
// rendered as JSON's natural form. The result is a valid expression
// in JavaScript, Python, Ruby, and any modern language that accepts
// JSON-superset literal syntax — no further wrapping needed.
func jsonLiteralValue(val any, _ ValueShape) string {
	b, err := json.Marshal(val)
	if err != nil {
		// json.Marshal effectively never fails on values we accept
		// (interface{} of map/slice/string/number/bool/nil), but if
		// it ever did, emit a JSON null so the script parses rather
		// than aborting at a bare unquoted identifier.
		return "null"
	}
	return string(b)
}

// rawTemplateValue renders a value verbatim (without shell-escaping) for
// the {{!ref}} raw substitution mode. Strings pass through; complex types
// are JSON-encoded (matches formatValue's prompt-rendering convention so
// authors can reason about both contexts uniformly).
func rawTemplateValue(val any, _ ValueShape) string {
	if val == nil {
		return "null"
	}
	if s, ok := val.(string); ok {
		return s
	}
	return formatValue(val)
}

// ToolBodyEnv is the one reading of a tool body's environment forms. It
// returns the body as its reader hands it on, and every braced form the
// reading consulted — each with the name it looks up and whether the author
// wrote a `:-` arm: a plain ${NAME} is answered when lookup carries the name
// and kept as written when it does not, a ${NAME:-default} is answered from
// lookup first and resolved to its default otherwise. That list is the
// decision record a preview reports (ADR-130).
//
// The reads are the SOURCE body's, offsets included: ir.BracedEnvRead.At is
// the position of the `$` in the body passed IN, which the caller holds
// (node.Command, node.Script, node.Postcondition). The returned text is that
// body after the reading, where those offsets no longer line up — compute a
// position against the authored text, never against the answer.
//
// lookup is the environment the reader speaks for: lookupToolEnv for a run —
// the engine's own overlay-then-process chain — and a lookup that answers
// nothing for a preview, which must read the same on a laptop and in CI, the
// reading `iterion validate` already gives a launch value
// (ir.ResolveVarTextAsWritten). A nil lookup answers nothing and is guarded
// here because the helpers call the lookup DIRECTLY: a nil func value
// panics. noToolEnv is the environment that answers no name, which is what a
// preview wants said rather than crashed.
//
// The reading is env-scoped, and it comes FIRST: every caller reads the
// environment forms before resolving the body's {{…}} references, which is
// the injection defence — expanding afterwards would read an upstream value's
// own `$NAME` back out of the single quotes shellEscapeValue put it in.
//
// node is the body's own node — the subject the reading answers for. ADR-130
// puts the declaration of what a body's environment reading does on the NODE,
// which is why the chokepoint takes it rather than the text alone: a node that
// does not ir.ToolNode.ExpandsBracedEnv gets its body back unchanged and an
// empty record, whatever the lookup holds. This is the ONE place a run or a
// preview reads it, so a reading that honours the declaration anywhere honours
// it everywhere — a run, a dry run, the three bodies alike — and the predicate
// fails closed, so a call that arrives with no declaration carrier keeps the
// body rather than rewriting one an author may have declared verbatim.
func ToolBodyEnv(node *ir.ToolNode, body string, lookup func(string) (string, bool)) (string, []ir.BracedEnvRead) {
	if !node.ExpandsBracedEnv() {
		// The node declines the reading (ADR-130): the body travels with its
		// environment text as the author wrote it, whatever the lookup
		// carries, and there is no decision to report because none was
		// made. The predicate is the IR's and fails closed, so a reading
		// that arrives without a declaration carrier — a nil node, a word
		// the compiler refuses — keeps the body too, rather than rewriting
		// one an author may have declared verbatim.
		return body, nil
	}
	if lookup == nil {
		lookup = noToolEnv
	}
	expanded, _ := expandBracedEnvSteps(body, lookup)
	return expanded, ir.BracedEnvReads(body)
}

// noToolEnv is the environment of a reader that speaks for none: every name
// is unset, so a ${NAME:-default} resolves to its default and a plain
// ${NAME} is kept as written.
func noToolEnv(string) (string, bool) { return "", false }

// expandBracedEnv expands ${NAME} and ${NAME:-default} env references in
// the author-controlled command template under the engine's own environment
// chain, by the body alone. Every production reading of a tool body goes
// through ToolBodyEnv; this form is the door the scanner's own witnesses
// hold it through — the linearity oracle (#1393) and the C188 reader pin
// (ir.BracedEnvReads). Bare $NAME (and shell-special
// $1, $?, $@, $ec, $OUT, ...) are passed through verbatim so shell-level
// constructs survive into the resolved command for sh -c to interpret.
//
// The previous implementation called os.ExpandEnv which treats both forms
// the same — silently eating any bare $NAME and breaking tool authors who
// wanted to capture exit codes (`ec=$?`), compose intermediate shell
// values (`OUT=$(...) ; echo "$OUT"`), or use positional args from
// `bash -c '...' _ "$1" "$2"`. The braced form is also the only form
// SKILL.md documents, so tightening to it is BC for documented usage.
func expandBracedEnv(s string) string {
	out, _ := expandBracedEnvSteps(s, lookupToolEnv)
	return out
}

// expandBracedEnvSteps is expandBracedEnv with the number of source positions
// the scan read — the loop's own and the closing-brace searches' — which is
// what a test asserts linearity on: a duration ratio would say the same thing
// and flake on a shared runner (#1393). lookup is the environment the body's
// reader speaks for; ToolBodyEnv is where a caller's nil becomes the lookup
// that answers nothing.
func expandBracedEnvSteps(s string, lookup func(string) (string, bool)) (string, int) {
	var out strings.Builder
	out.Grow(len(s))
	steps := 0
	i := 0
	for i < len(s) {
		steps++
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			// Find matching '}'.
			end := strings.IndexByte(s[i+2:], '}')
			// The suffix scan is accounted before branching, so a revert of
			// the early-stop below cannot take the counter with it: the
			// linearity witness must still see a rescan-per-`${` as the
			// quadratic work it is.
			scanned := end + 1
			if end == -1 {
				scanned = len(s) - (i + 2)
			}
			steps += scanned
			if end == -1 {
				// Unterminated — and so is every later `${`, since no `}`
				// follows this one anywhere: the rest of s passes through
				// verbatim, in one write. Stepping one byte and searching
				// again from the next `${` re-reads the same suffix from
				// every unmatched open, which is quadratic in the template.
				out.WriteString(s[i:])
				break
			}
			body := s[i+2 : i+2+end]
			// Only treat the body as an env var reference when it
			// LOOKS like one (`NAME` or `NAME:-default` where NAME is
			// a valid C-style identifier). Without this guard, a
			// script: js body's `${batchPackages.length}` or
			// `${sha.slice(0, 7)}` would be eaten — the body matches
			// no env var, no default, and the function used to return
			// "" which silently erased the JS template literal. Same
			// hazard for `${p.name}`, `${process.env.X}`, etc. inside
			// any script-language source we substitute into.
			// Expand only when:
			//   - the body lexically looks like a shell env ref, AND
			//   - either the ref has an explicit `:-default` (which is
			//     unambiguously a shell-style env ref — JS template
			//     literals don't have that syntax), OR the named env
			//     var is actually set on the process.
			// Otherwise (e.g. `${fname}` in a JS template literal where
			// no FNAME env var is set, or `${sha.slice(0, 7)}` whose
			// body doesn't even look like an env name), we pass the
			// `${body}` through verbatim so the downstream language
			// parser sees what the author wrote. The old behaviour was
			// to erase unset-no-default refs which silently broke JS
			// template literal interpolations.
			if looksLikeEnvRef(body) && bracedEnvWouldExpand(body, lookup) {
				out.WriteString(resolveBracedEnvBody(body, lookup))
				i += 2 + end + 1
				continue
			}
			// Pass the literal `${body}` through unchanged.
			out.WriteString(s[i : i+2+end+1])
			i += 2 + end + 1
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String(), steps
}

// bracedEnvWouldExpand reports whether expandBracedEnv would produce a
// non-empty, non-passthrough result for this body. Used to decide
// whether `${body}` should be substituted at all: a body that
// lexically resembles an env ref (looksLikeEnvRef) is only treated as
// one when either the author wrote an explicit `:-default` (a syntax
// that script languages don't use, so this is unambiguously shell) OR
// the named env var is actually set on the process. Otherwise the
// `${body}` is preserved verbatim so a script-language interpolation
// like `${fname}` doesn't get silently erased. lookup is the environment
// the body's reader speaks for, which is what decides the second arm.
func bracedEnvWouldExpand(body string, lookup func(string) (string, bool)) bool {
	if strings.Contains(body, ":-") {
		return true
	}
	name := body
	if idx := strings.Index(body, ":-"); idx != -1 {
		name = body[:idx]
	}
	_, ok := lookup(name)
	return ok
}

// lookupToolEnv resolves a tool command's `${NAME}` through the same
// overlay-then-process chain as every other `${ITERION_*:-default}` of the
// DSL (ir.LookupEnv, ADR-093): a bot-var setting must reach the command that
// reads it, not only the node fields beside it — a review table reading
// ${ITERION_VIBE_EFFORT_CLAUDE:-high} published "high" while the reviewer ran
// at the stored "max". Presence keeps the process semantics (a set-but-empty
// variable is present), because it decides whether `${body}` is an env ref or
// a script template left verbatim; an empty overlay value counts as unset,
// as it does everywhere else.
func lookupToolEnv(name string) (string, bool) {
	if strings.HasPrefix(name, "ITERION_") {
		if v := ir.LookupEnv(name); v != "" {
			return v, true
		}
	}
	if !ir.ProcessEnvReadable(name) {
		return "", false
	}
	return os.LookupEnv(name)
}

// looksLikeEnvRef reports whether body matches the shell convention
// for a `${NAME}` or `${NAME:-default}` reference: NAME is a valid
// C-style identifier (`[A-Za-z_][A-Za-z0-9_]*`), nothing else allowed
// before the optional `:-default` suffix. This separates "real env
// var lookups" from script-language template literals (`.`, `(`,
// `[`, spaces, etc.) which we must NOT eat.
func looksLikeEnvRef(body string) bool {
	name := body
	if idx := strings.Index(body, ":-"); idx != -1 {
		name = body[:idx]
	}
	if name == "" {
		return false
	}
	for i, r := range name {
		isLetter := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 {
			if !isLetter {
				return false
			}
		} else {
			if !isLetter && !isDigit {
				return false
			}
		}
	}
	return true
}

// resolveBracedEnvBody handles the body of a ${...} reference. Supports
// the bash-style default form ${NAME:-fallback} so authors can write
// recipes that work both with and without the env var set. lookup is
// consulted FIRST on both forms: a name it carries answers the reference,
// and the author's default is the fallback.
func resolveBracedEnvBody(body string, lookup func(string) (string, bool)) string {
	name := body
	defaultVal := ""
	hasDefault := false
	if idx := strings.Index(body, ":-"); idx != -1 {
		name = body[:idx]
		defaultVal = body[idx+2:]
		hasDefault = true
	}
	if v, ok := lookup(name); ok {
		return v
	}
	if hasDefault {
		return defaultVal
	}
	return ""
}

// shellEscapeValue formats val for safe interpolation into a sh -c command.
//
// shape is what the WORKFLOW declares about the slot val came from, and it
// decides first: a `json` var or field is one token of compact JSON text, a
// `string[]` one shell word per element (see value_shape.go). Both arrive
// here as []any, so nothing about the value itself could separate them.
//
// Undeclared — an `outputs.*` field, a key an edge delivers to a node with
// no `input:` schema, a drilled path — falls to the value's own shape:
//
// Homogeneous scalar slices ([]string, or []interface{} of strings /
// numbers / bools) become a space-separated list of individually-shell-
// quoted tokens, so each element survives sh's re-tokenization as its
// own argument — letting workflow authors pass file lists or argument
// arrays via a single {{input.x}} reference. An empty slice substitutes
// as empty string (the surrounding command will fail naturally if it
// required at least one argument).
//
// Complex slices (containing maps / slices) and bare maps are JSON-
// encoded and the resulting string is shell-escaped as a single token.
// This matches what the {{!input.x}} raw-substitution form does via
// formatValue, so authors get the same JSON wire shape across both
// forms — the default form just adds shell-escaping so the JSON can be
// safely captured into a shell variable (e.g. `X={{input.x}}` then
// `printf '%s' "$X" | jq ...`). Without this, `fmt.Sprint(map)` would
// render Go's native `map[k:v ...]` representation into the command,
// breaking the shell parse with `command not found` (exit 127) on the
// `[`/`map[...]` fragments.
//
// Scalars fall back to fmt.Sprint + shellEscape, preserving the prior
// single-value behaviour for strings, numbers, and booleans.
func shellEscapeValue(val any, shape ValueShape) string {
	switch shape {
	case ShapeJSON:
		return shellJSONToken(val)
	case ShapeWords:
		return shellWords(val)
	}
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
		// An all-string []interface{} space-joins here. That is the
		// `string[]` reading — `git add -- {{input.files}}` needs one shell
		// word per element — and the wrong one for a `json` value, where the
		// author writes `KEY={{input.langs}} … python3 json.loads(KEY)` and a
		// space-join gives `KEY=Go TypeScript`, so sh runs `TypeScript`
		// (exit 127). Both arrive as []any, so this arm cannot separate them
		// and does not try: a DECLARED slot took the shape branch above, and
		// what reaches here carries no declaration. The space-join is the
		// reading that keeps an argv position working, which is what an
		// undeclared list at a shell site most often is.
		if sliceHasComplexElement(v) {
			// Mixed or complex slice → JSON-encode as a single shell token.
			b, err := json.Marshal(v)
			if err == nil {
				return shellEscape(string(b))
			}
			// json.Marshal essentially never fails on []interface{} built
			// from JSON input; fall through to the per-element path so the
			// caller at least sees something rather than empty string.
		}
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = shellEscape(fmt.Sprint(e))
		}
		return strings.Join(parts, " ")
	case map[string]any:
		// Maps don't have a sensible space-separated representation;
		// JSON is the only round-trippable shape.
		b, err := json.Marshal(v)
		if err == nil {
			return shellEscape(string(b))
		}
		return shellEscape(fmt.Sprint(v))
	default:
		return shellEscape(fmt.Sprint(v))
	}
}

// sliceHasComplexElement reports whether s contains at least one
// non-scalar element (map or nested slice). Used to decide between
// space-separated shell tokens (scalar-only slices) and a single
// JSON-encoded token (anything else).
func sliceHasComplexElement(s []any) bool {
	for _, e := range s {
		switch e.(type) {
		case map[string]any, []any, []string, []map[string]any:
			return true
		}
	}
	return false
}

// shellEscape wraps a value in single quotes, escaping any embedded single
// quotes. This produces a string safe for interpolation into sh -c commands.
//
// SECURITY: the returned string MUST NOT be passed through any further
// expansion that interprets shell metacharacters (notably os.ExpandEnv,
// which expands $VAR even inside single quotes from sh's perspective).
// Any post-escape expansion can re-introduce metacharacters that defeat
// the quoting and re-open command-injection paths. Apply such expansions
// to the raw command template BEFORE substitution, never after.
func shellEscape(s string) string {
	// Replace each ' with '\'': end current quote, insert escaped quote, reopen quote.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// scriptScratchDir decides where a tool node's script file is written, and
// whether that is inside the judged workspace. The host's temp dir serves an
// unsandboxed run; a bind-mount sandbox with a shared state dir gets a
// `scripts` scratch under it, reachable at the same absolute path on both
// sides; a copy-based sandbox, or a bind-mount without a shared dir, keeps
// the file in the workspace — the only place both sides then agree on.
func scriptScratchDir(workDir, sharedStateDir string, sandboxed, copyBased bool) (dir string, inWorkspace bool) {
	switch {
	case !sandboxed:
		return os.TempDir(), false
	case !copyBased && sharedStateDir != "":
		return filepath.Join(sharedStateDir, "scripts"), false
	}
	return workDir, true
}
