package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
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

// recipeKind classifies how a tool node's recipe is executed. The
// classification is shared by executeToolNodeRecipe (standard path) and
// runVerifiedRecipe (Verified Action ladder) so the script/shell/registry
// heuristic lives in exactly one place.
type recipeKind int

const (
	recipeScript   recipeKind = iota // `script:` body via an interpreter
	recipeShell                      // `command:` with template refs or shell metacharacters
	recipeRegistry                   // bare `command:` resolved as a registered tool name
)

// recipeKindOf reports how node's recipe should run. `script:` takes
// precedence over `command:` (IR validation makes them mutually exclusive);
// a command with template refs or shell metacharacters runs via sh, else it
// is a bare registered-tool name.
func recipeKindOf(node *ir.ToolNode) recipeKind {
	switch {
	case node.Script != "":
		return recipeScript
	case len(node.CommandRefs) > 0 || looksLikeShellCommand(node.Command):
		return recipeShell
	default:
		return recipeRegistry
	}
}

// executeToolNodeRecipe runs a tool node's recipe with exit-code = success
// (the pre-ADR-044 behaviour). It is the rung-2 primitive of the Verified
// Action ladder and the whole of the non-verified path.
func (e *ClawExecutor) executeToolNodeRecipe(ctx context.Context, node *ir.ToolNode, input map[string]any) (map[string]any, error) {
	switch recipeKindOf(node) {
	case recipeScript:
		return e.executeToolNodeScript(ctx, node, input)
	case recipeShell:
		return e.executeToolNodeShell(ctx, node, input)
	}

	toolName := node.Command

	// Policy check before resolution — fail fast on denied tools.
	if err := e.checkToolNodePolicy(ctx, node, toolName); err != nil {
		return nil, err
	}

	resolved, ok, err := e.resolveSingleToolForNode(ctx, node, toolName)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: %w", node.ID, err)
	}
	if !ok {
		return nil, fmt.Errorf("model: tool node %q references unregistered tool %q", node.ID, toolName)
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
	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(node.ID, LLMToolCallInfo{
			ToolName:  toolName,
			InputSize: len(inputJSON),
			Duration:  duration,
			Error:     err,
		})
	}
	outputForEvent := outputStr
	if toolName == privacy.UnfilterToolName {
		outputForEvent = string(redactJSONTextField([]byte(outputStr)))
	}
	// Emit detailed tool I/O via the prompt hook (reused for tool node logging).
	if e.hooks.OnToolNodeResult != nil {
		e.hooks.OnToolNodeResult(node.ID, toolName, inputForEvent, outputForEvent, duration, err)
	}
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: execute: %w", node.ID, err)
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
func (e *ClawExecutor) emitToolNodeFinish(nodeID, toolName, resolved, stdout, stderr string, dur time.Duration, runErr error) {
	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(nodeID, LLMToolCallInfo{
			ToolName: toolName,
			Duration: dur,
			Error:    runErr,
		})
	}
	if e.hooks.OnToolNodeResult != nil {
		// Log both streams concatenated so run.log still surfaces what
		// the operator would see in an interactive shell. Stdout first
		// so the structured payload is visible at the top of long
		// stderr dumps from yarn/npm/git.
		logged := combineStreamsForLog(stdout, stderr)
		e.hooks.OnToolNodeResult(nodeID, toolName, []byte(resolved), logged, dur, runErr)
	}
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
	var output map[string]any
	if json.Unmarshal([]byte(stdout), &output) != nil {
		return map[string]any{"result": fallback}
	}
	return output
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
	buildCmd func(resolved string) (cmd *exec.Cmd, cleanup func(), err error),
) (map[string]any, error) {
	res, setupErr := e.runToolNodeCore(ctx, node, toolName, resolve, buildCmd)
	if setupErr != nil {
		return nil, setupErr
	}
	if res.runErr != nil {
		return nil, fmt.Errorf("model: tool node %q: %s: %w\nstdout: %s\nstderr: %s", node.ID, failVerb, res.runErr, res.stdout, res.stderr)
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
}

// runToolNodeCore is the shared recipe-execution skeleton: policy check,
// resolve, build the *exec.Cmd, emit the started/finish hooks, run with
// separated streams, and parse the JSON-or-text output. It returns the
// recipeResult (carrying any subprocess run error in res.runErr) and a
// SEPARATE setup error for unrecoverable build/policy failures (which the
// recovery ladder must NOT try to repair). Extracted from
// executeToolNodeCommon so the Verified Action ladder can reuse the exact
// same execution path and observe the streams.
func (e *ClawExecutor) runToolNodeCore(
	ctx context.Context,
	node *ir.ToolNode,
	toolName string,
	resolve func() string,
	buildCmd func(resolved string) (cmd *exec.Cmd, cleanup func(), err error),
) (recipeResult, error) {
	if err := e.checkToolNodePolicy(ctx, node, toolName); err != nil {
		return recipeResult{}, err
	}

	resolved := resolve()

	cmd, cleanup, err := buildCmd(resolved)
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
	stdoutBytes, stderrStr, runErr := runWithSeparateStreams(cmd)
	outputStr := string(stdoutBytes)
	duration := time.Since(start)

	e.emitToolNodeFinish(node.ID, toolName, resolved, outputStr, stderrStr, duration, runErr)

	return recipeResult{
		output:   parseToolNodeOutput(outputStr, strings.TrimSpace(outputStr)),
		stdout:   outputStr,
		stderr:   stderrStr,
		resolved: resolved,
		runErr:   runErr,
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
func (e *ClawExecutor) shellRecipe(ctx context.Context, node *ir.ToolNode, input map[string]any) (func() string, func(resolved string) (*exec.Cmd, func(), error)) {
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
			expandedCommand := expandBracedEnv(node.Command)
			// Resolve {{run.id}} first — resolveCommandTemplate only knows the
			// input/vars/secrets namespaces, so a direct run ref would survive
			// into the command verbatim.
			expandedCommand = resolveRunRefs(expandedCommand, RunIDFromContext(ctx), TemplateDataFromContext(ctx), node.CommandRefs, shellEscapeValue)
			resolved := resolveCommandTemplate(expandedCommand, node.CommandRefs, e.jsonFieldsAsText(node, input), e.vars, e.secretGuard)
			// Compression (tool nodes): node-level opt-in ONLY — compresses
			// command output only when the node's own `compress:` is on/ultra (a
			// run override can force-off as a kill switch, never force-on), so a
			// review loop's `git diff` stays full-fidelity unless the author opts
			// in. Done before secretGuard.Materialize so hooks/logs persist the
			// placeholder (rewriter-form) command, never the materialised secret.
			// Shell-only: a script body (executeToolNodeScript) is not a shell
			// command line and never rewrites.
			if m := rewrite.ResolveToolNode(e.compressOverride, node.Compress); m.Enabled() {
				if rewritten, changed := e.chain.Rewrite(ctx, m, resolved); changed {
					resolved = rewritten
				}
			}
			return resolved
		},
		func(resolved string) (*exec.Cmd, func(), error) {
			// Materialise secret placeholders into the command actually
			// executed via env-var indirection, not by inlining the raw
			// value into the command string: `resolved` (placeholder
			// form) is what the hooks/logs persist, and the exec'd
			// command line itself must not carry the plaintext secret
			// either — a command line is visible to any co-resident
			// local process via `ps`/`/proc/<pid>/cmdline` for the
			// subprocess's whole lifetime, unlike envp.
			materialized, env := e.secretGuard.MaterializeShellEnv(resolved)
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
// execution modes: registry tools and direct virtual shell/script tools. On
// denial it emits OnToolCall with the policy error, matching failed executed
// tool calls, and returns an error wrapped with node and tool context.
func (e *ClawExecutor) checkToolNodePolicy(ctx context.Context, node *ir.ToolNode, toolName string) error {
	if e.toolPolicy == nil {
		return nil
	}
	pctx := tool.PolicyContext{
		Ctx:      ctx,
		NodeID:   node.ID,
		NodeKind: ir.NodeTool.String(),
		ToolName: toolName,
		Vars:     e.vars,
	}
	if err := e.toolPolicy.CheckContext(pctx); err != nil {
		if e.hooks.OnToolCall != nil {
			e.hooks.OnToolCall(node.ID, LLMToolCallInfo{
				ToolName: toolName,
				Error:    err,
			})
		}
		return fmt.Errorf("model: tool node %q: tool %q denied: %w", node.ID, toolName, err)
	}
	return nil
}

// runWithSeparateStreams runs cmd with stdout and stderr captured into
// distinct buffers. Returns (stdout bytes, stderr string, run error).
// Use this for tool nodes where downstream needs to parse stdout as a
// structured payload while leaving stderr free for diagnostic chatter.
func runWithSeparateStreams(cmd *exec.Cmd) ([]byte, string, error) {
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdoutBytes, runErr := cmd.Output()
	// exec.ExitError carries stderr it captured before we set ours; the
	// buffer we provided is still the source of truth in our path.
	return stdoutBytes, stderrBuf.String(), runErr
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
func (e *ClawExecutor) scriptRecipe(ctx context.Context, node *ir.ToolNode, input map[string]any) (func() string, func(resolved string) (*exec.Cmd, func(), error)) {
	return func() string {
			// Same env-then-substitution ordering as executeToolNodeShell:
			// only the author-controlled `${NAME}` braces are env-expanded so
			// injected values from inputs/vars stay inert. But where the shell
			// path uses shell-escape, script: tools use resolveScriptTemplate
			// (JSON literal rendering) so values land as valid JS/Python/Ruby
			// literals — shell-escape's single-quote wrapping breaks
			// script-language string parsers when the value contains embedded
			// apostrophes. No compression: a script body is not a shell command line.
			expanded := expandBracedEnv(node.Script)
			// {{run.id}} first — resolveScriptTemplate only knows input/vars/secrets.
			expanded = resolveRunRefs(expanded, RunIDFromContext(ctx), TemplateDataFromContext(ctx), node.ScriptRefs, jsonLiteralValue)
			return resolveScriptTemplate(expanded, node.ScriptRefs, input, e.vars, e.secretGuard)
		},
		func(resolved string) (*exec.Cmd, func(), error) {
			interp, ext := scriptInterpreter(node.Language)
			if interp == "" {
				return nil, nil, fmt.Errorf("model: tool node %q: unsupported language %q", node.ID, node.Language)
			}
			// Temp file lives in the workspace so it is visible from inside the
			// sandbox bind-mount (e.workDir is the host-side bind source the
			// container also sees); a basename written there is reachable from
			// both sides via the same relative path. Removed on success or
			// failure via the returned cleanup.
			wd := e.workDir
			if wd == "" {
				wd = "."
			}
			tmpFile, err := os.CreateTemp(wd, ".iterion-script-*"+ext)
			if err != nil {
				return nil, nil, fmt.Errorf("model: tool node %q: create temp script: %w", node.ID, err)
			}
			tmpPath := tmpFile.Name()
			cleanup := func() { _ = os.Remove(tmpPath) }
			// Materialise secret placeholders into the executed script body
			// only. `resolved` (placeholder form) stays the value passed to
			// hooks/logs, so the real secret never reaches the event stream.
			body := e.secretGuard.MaterializeShell(resolved)
			if _, werr := tmpFile.WriteString(body); werr != nil {
				_ = tmpFile.Close()
				cleanup()
				return nil, nil, fmt.Errorf("model: tool node %q: write temp script: %w", node.ID, werr)
			}
			if cerr := tmpFile.Close(); cerr != nil {
				cleanup()
				return nil, nil, fmt.Errorf("model: tool node %q: close temp script: %w", node.ID, cerr)
			}
			base := filepath.Base(tmpPath)
			// Copy-based sandboxes (kubernetes: workspace tar-copied at
			// Prepare time) never see a host-side file created mid-run, so
			// the script must ALSO be pushed through the write-through seam.
			// The in-pod copy is removed on cleanup — a stray workspace
			// dotfile would otherwise be swept up by a later `git add -A`.
			if e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
				if refresher, ok := e.sandbox.(sandbox.WorkspaceFileRefresher); ok {
					if rerr := refresher.RefreshWorkspaceFile(ctx, base, []byte(body)); rerr != nil {
						cleanup()
						return nil, nil, fmt.Errorf("model: tool node %q: write script into sandbox: %w", node.ID, rerr)
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
			// Pass just the basename for the in-sandbox view (the bind mount
			// uses the same path; portable whether we run via sandbox or host).
			return e.toolNodeScriptCommand(ctx, interp, base), cleanup, nil
		}
}

// scriptInterpreter maps a `language:` token to the executable name on
// PATH and a file extension hint (extension is informational, not
// required by any interpreter). An empty language defaults to sh.
func scriptInterpreter(language string) (cmd string, ext string) {
	switch language {
	case "", "sh":
		return "sh", ".sh"
	case "bash":
		return "bash", ".sh"
	case "js", "node":
		return "node", ".js"
	case "py", "python", "python3":
		return "python3", ".py"
	default:
		return "", ""
	}
}

// toolNodeScriptCommand returns a configured *exec.Cmd that invokes the
// interpreter on the basename of the script temp file. Mirrors
// toolNodeCommand for the script-mode path: sandbox-routed if a sandbox
// is active and the node has not opted out.
func (e *ClawExecutor) toolNodeScriptCommand(ctx context.Context, interpreter, scriptBasename string) *exec.Cmd {
	if e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
		return e.sandbox.Command(ctx, []string{interpreter, scriptBasename}, sandbox.ExecOpts{})
	}
	cmd := exec.CommandContext(ctx, interpreter, scriptBasename)
	// Host path only: sandboxed commands already see the variable from the
	// container env (the same dir is bind-mounted there). runExtraEnv
	// carries run-level provisioning (devbox profile PATH), appended
	// after the inherited env so on a duplicate key it wins.
	if e.artifactFilesDir != "" || len(e.runExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), e.runExtraEnv...)
		if e.artifactFilesDir != "" {
			cmd.Env = append(cmd.Env, "ITERION_ARTIFACT_FILES_DIR="+e.artifactFilesDir)
		}
	}
	if e.workDir != "" {
		cmd.Dir = e.workDir
	}
	return cmd
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
func (e *ClawExecutor) toolNodeCommand(ctx context.Context, resolved string, env map[string]string) *exec.Cmd {
	if e.sandbox != nil && !e.nodeOptsOutOfSandbox(toolNodeOptOut) {
		return e.sandbox.Command(ctx, []string{"bash", "-c", resolved}, sandbox.ExecOpts{Env: env})
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", resolved)
	if len(env) > 0 || e.artifactFilesDir != "" || len(e.runExtraEnv) > 0 {
		cmd.Env = os.Environ()
		// Run-level provisioning (devbox profile PATH) — appended after
		// the inherited env so on a duplicate key it wins.
		cmd.Env = append(cmd.Env, e.runExtraEnv...)
		// Host path only: sandboxed commands already see the variable from
		// the container env (the same dir is bind-mounted there).
		if e.artifactFilesDir != "" {
			cmd.Env = append(cmd.Env, "ITERION_ARTIFACT_FILES_DIR="+e.artifactFilesDir)
		}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if e.workDir != "" {
		cmd.Dir = e.workDir
	}
	return cmd
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

// looksLikeShellCommand returns true if the command string looks like a shell
// command rather than a bare tool name. Tool names are simple identifiers
// (e.g. "read_file", "bash"), while shell commands contain spaces, operators,
// or path separators.
func looksLikeShellCommand(cmd string) bool {
	return strings.ContainsAny(cmd, " \t|&;><$`(){}\"'/")
}

// resolveCommandTemplate substitutes {{input.X}} and {{vars.X}} references in
// a command string with values from the input map and workflow variables.
// Values are shell-escaped to prevent command injection when the resolved
// string is passed to sh -c.
//
// Refs flagged as `Raw` (authored as `{{!input.X}}` / `{{!vars.X}}`) bypass
// shellEscape and are inserted verbatim. Use only for trusted values that
// are intentionally shell snippets (e.g. an upstream node returns a
// command line that the wrapping tool needs to RE-INTERPRET as shell, not
// pass as a single quoted token). Untrusted external inputs MUST keep the
// default escaping.
func resolveCommandTemplate(command string, refs []*ir.Ref, input map[string]any, vars map[string]any, guards ...*secretguard.Guard) string {
	var guard *secretguard.Guard
	if len(guards) > 0 {
		guard = guards[0]
	}
	// Shell commands preserve `{{input.X}}` literal text for missing
	// values — sh -c sees the placeholder and either fails informatively
	// or the operator notices. Substituting silently would lose that
	// signal and could mask wiring bugs.
	// Pinned by TestToolCommandRefsAreShellEscaped: the shell itself is the
	// oracle there, because reading a bot's `VAR={{vars.x}}` as unquoted is a
	// mistake that has already been made confidently.
	return resolveTemplateWith(command, refs, input, vars, guard, shellEscapeValue, false)
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
func resolveScriptTemplate(script string, refs []*ir.Ref, input map[string]any, vars map[string]any, guards ...*secretguard.Guard) string {
	var guard *secretguard.Guard
	if len(guards) > 0 {
		guard = guards[0]
	}
	// Script bodies (JS/Python/Ruby/…) must always parse — a nil-valued
	// ref left as raw `{{input.X}}` text crashes the interpreter at
	// parse time before any user logic can react. Substitute with the
	// language's null literal (rendered as JSON null = "null") so the
	// script can still run and handle the missing input itself.
	return resolveTemplateWith(script, refs, input, vars, guard, jsonLiteralValue, true)
}

// resolveRunRefs substitutes run-namespace refs ({{run.id}}) into a tool
// command / script template. The shared resolveTemplateWith handles only
// the input / vars / secrets namespaces — the ones tool nodes normally
// reach via edge `with`-mappings — so a *direct* {{run.id}} (there is no
// node output to map it from) would otherwise survive verbatim and run as
// the literal text "{{run.id}}". That bit sec-audit-source's
// apply-mode prepare_branch, which named its temp branch
// `iterion/sec-fix/{{run.id}}` and ended up on `iterion/sec-fix/run.id`
// after its sanitiser stripped the braces. `render` formats the value for
// the target context (shellEscapeValue for command bodies, jsonLiteralValue
// for script bodies), matching the main resolver; the bang form keeps the
// raw passthrough.
//
// The members are the engine's `run.*` namespace — identity plus the run's
// consumption and effective budget caps — read through the same
// lookupRunTemplateRef the prompt path uses, so a shell guard on
// `{{run.elapsed_seconds}}` cannot resolve in a prompt and stay literal in
// a command. td may be nil (hosts that wire only WithRunID): `run.id` still
// resolves, every other member renders empty rather than as its placeholder.
// A run id and the budget figures are plain tokens with no `{{` of their
// own, so the literal ReplaceAll cannot re-trigger on a substituted value.
func resolveRunRefs(template, runID string, td *TemplateData, refs []*ir.Ref, render func(any) string) string {
	for _, r := range refs {
		if r == nil || r.Kind != ir.RefRun || len(r.Path) == 0 {
			continue
		}
		var val any
		if r.Path[0] == "id" {
			val = runID
		} else if td != nil {
			if v, ok := td.Run[r.Path[0]]; ok {
				val = v
			}
		}
		rendered := render(val)
		if r.Unquoted {
			rendered = rawTemplateValue(val)
		}
		template = strings.ReplaceAll(template, r.Raw, rendered)
	}
	return template
}

// resolveTemplateWith is the shared core: walk refs, look up each value,
// dispatch to rawTemplateValue (bang form) or the renderer the caller
// provided (default form). Keeps shell- and script-mode template logic
// in one place.
//
// Substitution is single-pass over the original template: we build a
// map[ref.Raw]rendered and then scan the template once, replacing
// each {{...}} occurrence by its rendered value. The previous
// strings.ReplaceAll loop fed each substitution's output back into
// subsequent passes, so an input value that happened to contain a
// {{...}} literal matching a later ref would be silently rewritten
// (the "cascade" bug). The single-pass walk only touches positions
// that were in the source template.
func resolveTemplateWith(template string, refs []*ir.Ref, input map[string]any, vars map[string]any, guard *secretguard.Guard, defaultRender func(any) string, substituteNil bool) string {
	if len(refs) == 0 {
		return template
	}
	subs := make(map[string]string, len(refs))
	for _, ref := range refs {
		var val any
		var handled bool
		switch {
		case ref.Kind == ir.RefInput && len(ref.Path) > 0:
			val = input[ref.Path[0]]
			handled = true
		case ref.Kind == ir.RefVars && len(ref.Path) > 0:
			val = vars[ref.Path[0]]
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
		}
		if !handled {
			continue
		}
		// substituteNil controls whether a recognised-but-nil ref gets
		// rendered or left as raw template text. Shell contexts keep
		// the raw `{{input.X}}` placeholder so a missing wiring is
		// visible; script contexts MUST render (renderer turns nil into
		// "null") because a JS/Python/Ruby parser otherwise crashes on
		// the literal braces before any script logic runs.
		if val == nil && !substituteNil {
			continue
		}
		if ref.Unquoted {
			subs[ref.Raw] = rawTemplateValue(val)
		} else {
			subs[ref.Raw] = defaultRender(val)
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

// jsonLiteralValue renders val as a valid JSON literal suitable for
// pasting into a script-language source file. Strings get JSON-quoted
// (`"foo"`, with embedded quotes escaped as `\"`); maps and slices get
// JSON-encoded as object/array literals. Numbers, bools, and nil are
// rendered as JSON's natural form. The result is a valid expression
// in JavaScript, Python, Ruby, and any modern language that accepts
// JSON-superset literal syntax — no further wrapping needed.
func jsonLiteralValue(val any) string {
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
func rawTemplateValue(val any) string {
	if val == nil {
		return "null"
	}
	if s, ok := val.(string); ok {
		return s
	}
	return formatValue(val)
}

// expandBracedEnv expands ${NAME} and ${NAME:-default} env references in
// the author-controlled command template. Bare $NAME (and shell-special
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
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			// Find matching '}'.
			end := strings.IndexByte(s[i+2:], '}')
			if end == -1 {
				// Unterminated — pass through verbatim.
				out.WriteByte(s[i])
				i++
				continue
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
			if looksLikeEnvRef(body) && bracedEnvWouldExpand(body) {
				out.WriteString(resolveBracedEnvBody(body))
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
	return out.String()
}

// bracedEnvWouldExpand reports whether expandBracedEnv would produce a
// non-empty, non-passthrough result for this body. Used to decide
// whether `${body}` should be substituted at all: a body that
// lexically resembles an env ref (looksLikeEnvRef) is only treated as
// one when either the author wrote an explicit `:-default` (a syntax
// that script languages don't use, so this is unambiguously shell) OR
// the named env var is actually set on the process. Otherwise the
// `${body}` is preserved verbatim so a script-language interpolation
// like `${fname}` doesn't get silently erased.
func bracedEnvWouldExpand(body string) bool {
	if strings.Contains(body, ":-") {
		return true
	}
	name := body
	if idx := strings.Index(body, ":-"); idx != -1 {
		name = body[:idx]
	}
	_, ok := os.LookupEnv(name)
	return ok
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
// recipes that work both with and without the env var set.
func resolveBracedEnvBody(body string) string {
	name := body
	defaultVal := ""
	hasDefault := false
	if idx := strings.Index(body, ":-"); idx != -1 {
		name = body[:idx]
		defaultVal = body[idx+2:]
		hasDefault = true
	}
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	if hasDefault {
		return defaultVal
	}
	return ""
}

// jsonFieldsAsText pre-encodes every input value whose schema field is
// declared `json` into its compact JSON text, so shell substitution
// renders it as ONE token.
//
// Without it, a `json` field holding an all-string list reaches
// shellEscapeValue's scalar-slice arm and space-joins:
//
//	QUICK={{input.quick_replies}} python3 -c "json.loads(os.environ['QUICK'])"
//
// resolves to `QUICK='Go' 'TypeScript' python3 …`, so sh reads only the
// first word as the assignment and runs `TypeScript` as the command —
// exit 127, with a fragment of model-authored prose as the error. The
// author declared `json` and parses JSON; the space-join silently
// contradicts both.
//
// Scoped to `json`-declared fields on purpose. The scalar-slice
// space-join is load-bearing for `string[]` fields — `git add --
// {{input.files}}` and friends need one shell word per element — and an
// agent's structured output arrives as []any either way, so the declared
// type is the only thing that distinguishes the two intents. Values
// already textual (string, nil) and fields the schema does not declare
// are returned untouched, and the input map is only copied when a field
// actually changes.
//
// Shell command bodies only: `script:` contexts JSON-encode values
// themselves, and pre-encoding here would double-encode them.
func (e *ClawExecutor) jsonFieldsAsText(node *ir.ToolNode, input map[string]any) map[string]any {
	if node == nil || node.InputSchema == "" || len(input) == 0 {
		return input
	}
	schema := e.schemas[node.InputSchema]
	if schema == nil {
		return input
	}
	out := input
	copied := false
	for _, f := range schema.Fields {
		if f == nil || f.Type != ir.FieldTypeJSON {
			continue
		}
		v, ok := out[f.Name]
		if !ok {
			continue
		}
		switch v.(type) {
		case nil, string:
			// Already a single token; re-encoding would add quotes the
			// author never wrote.
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			// Unmarshalable value — leave the existing behaviour rather
			// than dropping the field.
			continue
		}
		if !copied {
			cloned := make(map[string]any, len(input))
			for k, val := range input {
				cloned[k] = val
			}
			out, copied = cloned, true
		}
		out[f.Name] = string(b)
	}
	return out
}

// shellEscapeValue formats val for safe interpolation into a sh -c command.
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
func shellEscapeValue(val any) string {
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
		// An all-string []interface{} space-joins here. That is correct for a
		// `string[]` field — `git add -- {{input.files}}` needs one shell word
		// per element — and wrong for a `json` one, where the author writes
		// `KEY={{input.langs}} … python3 json.loads(KEY)` and gets
		// `KEY=Go TypeScript`, so sh runs `TypeScript` (exit 127). Both shapes
		// arrive as []any, so this function cannot tell them apart.
		// RESOLVED one frame up instead of here: jsonFieldsAsText pre-encodes
		// the values whose schema field is declared `json`, on both shell
		// command bodies (shellRecipe and runPostcondition), leaving this
		// arm's space-join intact for `string[]`. Object arrays keep
		// JSON-encoding below regardless of the declaration.
		// Still uncovered: `{{vars.x}}` on a `json`-declared var, and input
		// keys an edge delivers to a node that declares no `input:` schema.
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
