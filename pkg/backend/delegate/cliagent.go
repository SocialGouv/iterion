package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// CLIAgentProtocol declares how to drive a third-party agent CLI whose
// argument protocol is NOT claude-code-compatible (see ADR-065). Unlike the
// per-node `command:` override (#76) — which only swaps the binary while
// keeping claude-code's Session-mode argv (`--print`, `--input-format
// stream-json`, prompt on stdin, …) — a protocol describes the CLI's *own*
// invocation. It is the seam a real backend (e.g. Moonshot's `kimi-code`,
// `kimi -p <prompt> --output-format stream-json -m <alias>`) plugs into.
//
// The zero value is not usable; construct one per target CLI (see kimi.go)
// and register a CLIAgentBackend wrapping it.
type CLIAgentProtocol struct {
	// Name is the backend registration name and the label stamped on
	// Result.BackendName / log lines (e.g. "kimi").
	Name string

	// DefaultBinary is the CLI executable to invoke when the backend's
	// Command override is empty (e.g. "kimi").
	DefaultBinary string

	// PromptFlag delivers the user prompt on the command line. When
	// PromptViaStdin is false, the prompt is passed as the flag's value:
	// `<PromptFlag> <prompt>` (e.g. "-p <prompt>"). When PromptViaStdin is
	// true and PromptFlag is set, PromptFlag is emitted as a bare switch and
	// the prompt is written to the process stdin instead. Empty PromptFlag +
	// PromptViaStdin means "prompt straight to stdin, no flag".
	PromptFlag     string
	PromptViaStdin bool

	// SystemPromptFlag, when set, delivers the composed system prompt as
	// `<SystemPromptFlag> <system>`. When empty (the common case for CLIs
	// with no system-prompt flag, incl. kimi-code), the system prompt is
	// folded in as a preamble to the user prompt so the node's `system:`
	// task still reaches the agent.
	SystemPromptFlag string

	// SystemPromptViaFile passes SystemPromptFlag a *path* instead of the
	// prompt text: the composed prompt is written under
	// <WorkDir>/.iterion/<name>/ and deleted when the invocation returns.
	// Required for CLIs whose system-prompt flag accepts `<text|file>` —
	// a composed prompt (posture + cursors + skills + preset) can exceed
	// MAX_ARG_STRLEN (128 KiB), and passing a real path also removes the
	// text/path ambiguity such CLIs resolve with an existence check.
	//
	// The file is workspace-relative on purpose: a sandboxed run bind-mounts
	// WorkDir, so os.TempDir() would be invisible inside the container.
	// Ignored when SystemPromptFlag is empty.
	SystemPromptViaFile bool

	// ModelFlag maps Task.Model onto the CLI's own model selector, emitted as
	// `<ModelFlag> <MapModel(model)>` when both ModelFlag and the mapped model
	// are non-empty. MapModel translates iterion's `provider/model` spec into
	// the CLI's native alias; nil passes Model through verbatim.
	ModelFlag string
	MapModel  func(model string) string

	// OutputFormatFlag + OutputFormat request a machine-readable stream from
	// the CLI (e.g. "--output-format" "stream-json"). Empty OutputFormatFlag
	// leaves the CLI at its default (text) format.
	OutputFormatFlag string
	OutputFormat     string

	// MapEffort maps an iterion reasoning-effort level ("low".."max") onto
	// extra argv the CLI understands. Return an empty slice for an effort the
	// CLI can't express; a nil MapEffort drops effort entirely (the CLI has no
	// effort dial).
	MapEffort func(effort string) []string

	// ExtraArgs are appended verbatim after all generated flags (static CLI
	// switches the protocol always wants, e.g. a non-interactive flag).
	ExtraArgs []string

	// ExtraArgsFor returns per-task argv appended after ExtraArgs. Unlike the
	// static ExtraArgs it sees the Task, so a protocol can emit session ids,
	// skill paths, extension paths and sandbox-dependent switches without
	// needing its own Backend implementation. nil emits nothing.
	ExtraArgsFor func(task Task) []string

	// ParseOutput extracts the assistant's final text (plus optional session
	// id and token count) from the CLI's raw stdout. For a stream-json
	// protocol it walks the NDJSON events; for a text protocol it returns
	// stdout verbatim. The returned text is then fed through the shared
	// schema-aware fallback (parseSDKOutput) so structured `output:` schemas
	// work the same way they do for the other backends. nil defaults to
	// treating stdout as plain text.
	ParseOutput func(stdout string) (text, sessionID string, tokens int)

	// ParseOutputRich supersedes ParseOutput when non-nil. It returns the
	// full per-invocation accounting a modern agent CLI reports — a real
	// input/output token split, a provider-computed USD cost, the effective
	// model, the context window — plus a typed Err for CLIs that render a
	// FAILED run on a *zero* exit code (a machine-readable output mode that
	// only encodes failure in the stream). See CLIAgentParse.
	ParseOutputRich func(stdout string) CLIAgentParse

	// SandboxEnv, when set, replaces ResolveEnv on the SANDBOXED path. A
	// container inherits nothing from the host, so a CLI that resolves its own
	// credentials from the ambient environment needs them forwarded by name —
	// which ResolveEnv, built for per-run overrides, does not do. nil falls
	// back to ResolveEnv.
	SandboxEnv func(ctx context.Context, task Task) map[string]string

	// HostBinaryEnv names an environment variable holding an absolute path to
	// the CLI on the HOST. It is consulted only when the backend has no explicit
	// Command AND the task is not sandboxed: a host path is meaningless as
	// argv[0] inside a container, where the image supplies the CLI itself.
	HostBinaryEnv string

	// ResolveEnv returns credential/endpoint environment overrides sourced
	// from the target CLI's own config/env conventions (and any per-run
	// injected credentials on the context). The returned map is layered on
	// top of the inherited process environment. nil means "inherit the host
	// environment unchanged" — the CLI resolves its own credentials, which is
	// the correct default for a CLI that reads e.g. $MOONSHOT_API_KEY itself.
	ResolveEnv func(ctx context.Context) map[string]string

	// PermissionHook describes a native PreToolUse hook discovered from the
	// target CLI's home. When set, Execute creates a private shadow home for
	// this invocation, links the operator's existing CLI state into it, and
	// adds only iterion's permission hook registration. nil means this CLI
	// cannot enforce an enabled permission policy.
	PermissionHook *CLIAgentPermissionHook
}

// CLIAgentPermissionHook describes how one CLI discovers a PreToolUse hook.
// WriteRegistration owns only the backend-specific registration syntax; the
// shadow-home lifecycle and policy serialisation remain shared.
type CLIAgentPermissionHook struct {
	HomeEnv           string
	DefaultHome       string
	ExcludedEntries   []string
	WriteRegistration func(realHome, shadowHome, command string) error
}

// resolveBinary picks argv[0] for this task: the explicit Command wins, then
// the protocol's host-only env override (never inside a sandbox — a host path
// is not a container path, and nothing mounts it there), then DefaultBinary,
// which is what the published images ship on PATH.
func (b *CLIAgentBackend) resolveBinary(task Task) string {
	if b.Command != "" {
		return b.Command
	}
	if b.Protocol.HostBinaryEnv != "" && task.Hostless() {
		return strings.TrimSpace(os.Getenv(b.Protocol.HostBinaryEnv))
	}
	return ""
}

// CLIAgentParse is the rich parse result of a CLI-agent invocation (see
// CLIAgentProtocol.ParseOutputRich). Every field is optional: a zero value
// degrades to exactly what the legacy ParseOutput contract produced.
type CLIAgentParse struct {
	// Text is the assistant's final message, fed to the shared schema-aware
	// parseSDKOutput fallback like ParseOutput's first return value.
	Text string
	// SessionID is the CLI's own session identifier, when it reports one.
	SessionID string

	// InputTokens / OutputTokens are the real split. The legacy ParseOutput
	// contract only reported a total, which cost.Annotate then booked
	// entirely at the (more expensive) output rate.
	InputTokens  int
	OutputTokens int
	// ThinkingTokens are reasoning tokens. They are a SUBSET of OutputTokens
	// for every provider we track — never add them to the total.
	ThinkingTokens int

	// CostUSD is the CLI's own cost figure, computed against its per-provider
	// pricing catalogue. When > 0 it wins over iterion's estimate table.
	CostUSD float64

	// EffectiveModel is the model the CLI actually resolved. CLIs that accept
	// a fuzzy model pattern can silently resolve to a different model than
	// requested; surfacing it lets the caller warn.
	EffectiveModel string
	// ContextWindow and PeakInputTokens feed the studio's context gauge.
	ContextWindow   int
	PeakInputTokens int

	// Notices are human-readable observations the backend logs at WARN.
	// They exist for facts a CLI reports about its own behaviour that the
	// Result cannot express — most importantly upstream retries the CLI
	// performed internally, which are invisible in its final transcript and
	// therefore silently absent from the reported token count and cost.
	Notices []string

	// Err is a failure the CLI reported *in its output stream* while exiting
	// zero. The typed value (*ErrTransient, *ErrRateLimited, or a plain
	// error) is returned verbatim from Execute so the executor's retry
	// classifier keys on it — which is also why the CLIAgentBackend's own
	// retry loop deliberately never re-runs on it: retry policy for a
	// well-classified upstream failure belongs to the executor, not here.
	Err error
}

// CLIAgentBackend is a delegate.Backend that drives a third-party agent CLI
// described by a CLIAgentProtocol. It mirrors the codex backend's shape —
// build native argv, run with a wall-clock timeout, parse stdout, retry on a
// no-output transient — but the invocation is the target CLI's own, not
// claude-code's. See ADR-065.
type CLIAgentBackend struct {
	// Protocol describes the target CLI's argument protocol.
	Protocol CLIAgentProtocol
	// Command overrides Protocol.DefaultBinary (the per-node `command:`
	// override / an operator-pinned build path).
	Command string
	// Timeout caps a single CLI invocation's wall-clock time. Zero uses
	// defaultCLIAgentTimeout. The context deadline (run budget) still applies
	// and wins when tighter.
	Timeout time.Duration
	// Logger is the leveled logger for diagnostic output.
	Logger *iterlog.Logger
}

const (
	maxCLIAgentRetries     = 3
	defaultCLIAgentTimeout = 20 * time.Minute
)

// Execute runs the target CLI for the given task and parses its output into a
// delegate.Result.
func (b *CLIAgentBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}
	proto := b.Protocol
	backendName := proto.Name
	if backendName == "" {
		backendName = "cli_agent"
	}

	permissionEnv, permissionCleanup, err := b.preparePermissionHook(ctx, task, proto, backendName)
	if err != nil {
		return Result{BackendName: backendName, ExitCode: -1}, err
	}
	defer permissionCleanup()

	binary := b.resolveBinary(task)
	if binary == "" {
		binary = proto.DefaultBinary
	}
	if binary == "" {
		return Result{BackendName: backendName, ExitCode: -1},
			fmt.Errorf("delegate: %s: no CLI binary configured (set command: or protocol DefaultBinary)", backendName)
	}

	systemPrompt := task.BuildSystemPrompt()
	userPrompt := task.UserPrompt

	// When the CLI exposes no system-prompt flag, fold the composed system
	// prompt in as a preamble so the node's task still reaches the agent.
	promptArg := userPrompt
	if proto.SystemPromptFlag == "" && systemPrompt != "" {
		if promptArg != "" {
			promptArg = systemPrompt + "\n\n" + promptArg
		} else {
			promptArg = systemPrompt
		}
	}

	// SystemPromptViaFile swaps the prompt text for a path under the
	// workspace (visible inside the sandbox, unlike os.TempDir()).
	systemArg := systemPrompt
	if proto.SystemPromptFlag != "" && proto.SystemPromptViaFile && systemPrompt != "" {
		path, cleanup, err := writeSystemPromptFile(ctx, task, backendName, systemPrompt)
		if err != nil {
			return Result{BackendName: backendName, ExitCode: -1}, err
		}
		defer cleanup()
		systemArg = path
	}

	args, stdinPrompt := b.buildArgs(proto, task, promptArg, systemArg)

	env := os.Environ()
	if proto.ResolveEnv != nil {
		for k, v := range proto.ResolveEnv(ctx) {
			env = append(env, k+"="+v)
		}
	}
	// Run-level provisioning (devbox profile PATH) — appended last so on
	// a duplicate key the run-level value wins.
	env = append(env, task.ExtraEnv...)
	// The per-invocation shadow home must win over both ambient and run-level
	// values; otherwise a duplicate GROK_HOME/KIMI_CODE_HOME silently bypasses
	// the hook registration.
	env = append(env, permissionEnv...)

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = defaultCLIAgentTimeout
	}

	start := time.Now()
	stdout, stderr, exitCode, err := b.runWithRetry(ctx, task, binary, args, stdinPrompt, env, timeout)
	duration := time.Since(start)

	result := Result{
		Duration:    duration,
		ExitCode:    exitCode,
		Stderr:      truncate(stderr, 8192),
		BackendName: backendName,
	}
	if err != nil {
		return result, err
	}

	// Parse the CLI's stdout into the assistant's final text, then apply the
	// shared schema-aware fallback so `output:` schemas behave uniformly.
	parsed := b.parseStdout(proto, stdout)

	if b.Logger != nil {
		for _, notice := range parsed.Notices {
			b.Logger.Warn("[%s#%d/%s] %s", task.NodeID, task.Iteration, backendName, notice)
		}
	}

	result.SessionID = parsed.SessionID
	result.Tokens = parsed.InputTokens + parsed.OutputTokens
	result.ThinkingTokens = parsed.ThinkingTokens
	result.EffectiveModel = parsed.EffectiveModel
	result.ContextWindow = parsed.ContextWindow
	result.PeakInputTokens = parsed.PeakInputTokens

	text := parsed.Text
	output, rawLen, fallback := parseSDKOutput(&text, nil, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	cost.AnnotateWithUSD(result.Output, task.Model, parsed.InputTokens, parsed.OutputTokens, parsed.CostUSD)

	// A failure the CLI encoded in its stream while exiting zero. Reported
	// after the Result is filled in so the partial transcript, tokens and
	// cost are still observable on the failing node.
	if parsed.Err != nil {
		return result, parsed.Err
	}

	if parsed.EffectiveModel != "" && task.Model != "" && !sameModelID(parsed.EffectiveModel, task.Model) && b.Logger != nil {
		b.Logger.Warn("[%s#%d/%s] requested model %q resolved to %q",
			task.NodeID, task.Iteration, backendName, task.Model, parsed.EffectiveModel)
	}
	return result, nil
}

// preparePermissionHook materialises the common half of the grok/kimi hook
// seam and returns the environment override plus its cleanup closure.
func (b *CLIAgentBackend) preparePermissionHook(ctx context.Context, task Task, proto CLIAgentProtocol, backendName string) ([]string, func(), error) {
	policy := task.Permission
	if !policy.Enabled() {
		return nil, func() {}, nil
	}
	if proto.PermissionHook == nil {
		return nil, nil, fmt.Errorf("delegate: %s: permission: %s is enabled but this CLI cannot enforce it", backendName, policy.Mode)
	}
	if policy.CanAsk() {
		return nil, nil, fmt.Errorf("delegate: %s: permission policy can ask for operator approval, but an external CLI hook cannot pause the iterion run; use permission: deny without ask rules", backendName)
	}
	if !task.Hostless() {
		return nil, nil, fmt.Errorf("delegate: %s: permission-gated sandboxed runs are unsupported because the CLI home and hook binary are host-side; refusing to run ungated", backendName)
	}

	hook := proto.PermissionHook
	if hook.HomeEnv == "" || hook.DefaultHome == "" || hook.WriteRegistration == nil {
		return nil, nil, fmt.Errorf("delegate: %s: incomplete permission-hook protocol", backendName)
	}
	iterionBin := proc.LocateIterionBinary()
	if iterionBin == "" {
		return nil, nil, fmt.Errorf("delegate: %s: cannot locate a stable iterion binary for the permission hook; set ITERION_BIN", backendName)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(probeCtx, iterionBin, "__permission-hook", "--probe").CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("delegate: %s: iterion hook binary %q is stale or unusable: %w (%s)", backendName, iterionBin, err, strings.TrimSpace(string(output)))
	}

	root, loc := task.StateDir(backendName)
	if err := PrepareStateRoot(task, root, loc, backendName+" backend", "the permission shadow home and linked CLI credentials", b.Logger); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, nil, fmt.Errorf("delegate: %s: create permission state dir: %w", backendName, err)
	}
	shadowHome, err := os.MkdirTemp(root, "permission-home-*")
	if err != nil {
		return nil, nil, fmt.Errorf("delegate: %s: create permission shadow home: %w", backendName, err)
	}
	cleanup := func() { _ = os.RemoveAll(shadowHome) }

	// A run-level environment override is part of the CLI invocation and must
	// therefore also be the source we shadow. Reading only os.Getenv would
	// replace an explicitly selected credential home with the ambient default.
	realHome := ""
	for _, kv := range task.ExtraEnv {
		if key, value, ok := strings.Cut(kv, "="); ok && key == hook.HomeEnv {
			realHome = strings.TrimSpace(value)
		}
	}
	if realHome == "" {
		realHome = strings.TrimSpace(os.Getenv(hook.HomeEnv))
	}
	if realHome == "" {
		operatorHome, homeErr := os.UserHomeDir()
		if homeErr != nil {
			cleanup()
			return nil, nil, fmt.Errorf("delegate: %s: resolve operator home: %w", backendName, homeErr)
		}
		realHome = filepath.Join(operatorHome, hook.DefaultHome)
	}
	if abs, absErr := filepath.Abs(realHome); absErr == nil {
		realHome = abs
	}
	if err := linkShadowHome(realHome, shadowHome, hook.ExcludedEntries); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("delegate: %s: build permission shadow home: %w", backendName, err)
	}

	policyPath := filepath.Join(shadowHome, ".iterion-permission-policy.json")
	rawPolicy, err := json.Marshal(policy.Config())
	if err == nil {
		err = os.WriteFile(policyPath, rawPolicy, 0o600)
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("delegate: %s: write permission policy: %w", backendName, err)
	}
	command := shellQuote(iterionBin) + " __permission-hook --backend " + shellQuote(backendName) + " --policy " + shellQuote(policyPath)
	if err := hook.WriteRegistration(realHome, shadowHome, command); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("delegate: %s: register permission hook: %w", backendName, err)
	}
	return []string{hook.HomeEnv + "=" + shadowHome}, cleanup, nil
}

func linkShadowHome(realHome, shadowHome string, excluded []string) error {
	entries, err := os.ReadDir(realHome)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	skip := make(map[string]bool, len(excluded))
	skip[".iterion-permission-policy.json"] = true
	for _, name := range excluded {
		skip[name] = true
	}
	// ReadDir is sorted by filename, but keep this explicit: deterministic
	// shadow homes make failures and tests reproducible on every filesystem.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if skip[entry.Name()] {
			continue
		}
		if err := os.Symlink(filepath.Join(realHome, entry.Name()), filepath.Join(shadowHome, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// shellQuote quotes one fixed argv fragment for the shell command string both
// hook implementations accept. Single quotes are the only portable quoting
// form needed here; embedded quotes use the standard close/escape/reopen form.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// parseStdout applies the protocol's parser, preferring the rich contract.
// A protocol with neither parser treats stdout as the assistant's text.
func (b *CLIAgentBackend) parseStdout(proto CLIAgentProtocol, stdout string) CLIAgentParse {
	if proto.ParseOutputRich != nil {
		return proto.ParseOutputRich(stdout)
	}
	if proto.ParseOutput != nil {
		// The legacy contract reports a token total with no split. Book it
		// as output, preserving the historical cost.Annotate(…, 0, tokens).
		text, sessionID, tokens := proto.ParseOutput(stdout)
		return CLIAgentParse{Text: text, SessionID: sessionID, OutputTokens: tokens}
	}
	return CLIAgentParse{Text: stdout}
}

// sameModelID reports whether two model specs name the same model, ignoring
// any `provider/` routing prefix on either side.
func sameModelID(a, b string) bool {
	return bareModelID(a) == bareModelID(b)
}

func bareModelID(spec string) string {
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		return spec[i+1:]
	}
	return spec
}

// writeSystemPromptFile materialises the composed system prompt under
// <WorkDir>/.iterion/<backend>/ and returns its path plus a cleanup closure.
// See CLIAgentProtocol.SystemPromptViaFile for why it is workspace-relative.
//
// Each call owns a UNIQUE file. (NodeID, Iteration) is not unique: Iteration
// is a LOOP counter, and under a fan_out_each router the same node id runs
// concurrently across branches at the same iteration, so every branch would
// compute the same path and one branch's deferred cleanup would delete the
// file another branch's CLI is still starting on. The loss is silent rather
// than loud, which is what makes it dangerous: the flag accepts <text|file>
// and disambiguates by existence, so the losing branch runs with the literal
// path string as its system prompt — discarding the composed posture, cursors
// and skills without an error. piext.Materialise avoids the same hazard the
// same way.
func writeSystemPromptFile(ctx context.Context, task Task, backendName, systemPrompt string) (path string, cleanup func(), err error) {
	// A task with neither a workspace nor a store has nowhere of its own to put
	// this, and StateDir's last resort is the OPERATOR's iterion home — which is
	// not somewhere a per-invocation scratch file belongs. The old precondition
	// named WorkDir only, which was stale after StateDir landed, but deleting it
	// outright made a degenerate task create `~/.iterion/<backend>` on the
	// operator's machine (caught by a unit test doing exactly that).
	if task.WorkDir == "" && task.StoreDir == "" {
		return "", nil, fmt.Errorf("delegate: %s: SystemPromptViaFile requires a WorkDir or StoreDir", backendName)
	}
	// Task.StateDir keeps this out of the target repository's checkout whenever
	// the run has somewhere better, so the composed prompt — which carries the
	// node's whole operating posture — is not written into a tree the repo
	// controls. The symlink refusal is NOT here: this function is skipped for a
	// node with an empty system prompt, so it could never be the boundary.
	dir, _ := task.StateDir(backendName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", nil, fmt.Errorf("delegate: %s: create system-prompt dir: %w", backendName, err)
	}
	node := task.NodeID
	if node == "" {
		node = "node"
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf("%s-%d-*.sysprompt.md", sanitizeFileSegment(node), task.Iteration))
	if err != nil {
		return "", nil, fmt.Errorf("delegate: %s: create system prompt: %w", backendName, err)
	}
	path = f.Name()
	_, werr := f.WriteString(systemPrompt)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(path, 0o600)
	}
	if werr != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("delegate: %s: write system prompt: %w", backendName, werr)
	}
	// A copy-based sandbox never sees the host write, and the CLI is about to be
	// handed this PATH. Failing here beats spawning an agent whose whole
	// operating posture silently went missing.
	if merr := mirrorStateFileIntoSandbox(ctx, task, path, []byte(systemPrompt)); merr != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("delegate: %s: %w", backendName, merr)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// sanitizeFileSegment reduces a node id to a safe single path segment.
func sanitizeFileSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// buildArgs assembles the target CLI's argv from the protocol + task. It
// returns the argument slice (excluding the binary itself) and the prompt to
// pipe on stdin (empty unless the protocol uses stdin delivery).
func (b *CLIAgentBackend) buildArgs(proto CLIAgentProtocol, task Task, promptArg, systemPrompt string) (args []string, stdinPrompt string) {
	if proto.PromptViaStdin {
		if proto.PromptFlag != "" {
			args = append(args, proto.PromptFlag)
		}
		stdinPrompt = promptArg
	} else if proto.PromptFlag != "" {
		args = append(args, proto.PromptFlag, promptArg)
	}

	if proto.OutputFormatFlag != "" && proto.OutputFormat != "" {
		args = append(args, proto.OutputFormatFlag, proto.OutputFormat)
	}

	if proto.ModelFlag != "" && task.Model != "" {
		model := task.Model
		if proto.MapModel != nil {
			model = proto.MapModel(model)
		}
		if model != "" {
			args = append(args, proto.ModelFlag, model)
		}
	}

	if proto.SystemPromptFlag != "" && systemPrompt != "" {
		args = append(args, proto.SystemPromptFlag, systemPrompt)
	}

	if proto.MapEffort != nil && task.ReasoningEffort != "" {
		args = append(args, proto.MapEffort(task.ReasoningEffort)...)
	}

	args = append(args, proto.ExtraArgs...)
	if proto.ExtraArgsFor != nil {
		args = append(args, proto.ExtraArgsFor(task)...)
	}
	return args, stdinPrompt
}

// runWithRetry runs the CLI to completion, retrying up to maxCLIAgentRetries
// when the process exits successfully but produces no stdout (a known
// transient failure mode for streaming agent CLIs) or fails with a
// network-signature error. Overflow/quota/fatal exits are surfaced fast.
func (b *CLIAgentBackend) runWithRetry(ctx context.Context, task Task, binary string, args []string, stdinPrompt string, env []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error) {
	for attempt := 1; attempt <= maxCLIAgentRetries; attempt++ {
		stdout, stderr, exitCode, err = b.runOnce(ctx, task, binary, args, stdinPrompt, env, timeout)

		if err == nil && strings.TrimSpace(stdout) != "" {
			return stdout, stderr, exitCode, nil
		}

		// Context cancelled/deadline: not retryable, surface immediately.
		if ctx.Err() != nil {
			return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: context ended: %w", b.Protocol.Name, ctx.Err())
		}

		transient := err != nil && (MatchesNetworkSignature(err.Error()) || MatchesNetworkSignature(stderr))
		empty := err == nil && strings.TrimSpace(stdout) == ""
		if !transient && !empty {
			// A genuine non-transient failure (bad flag, auth error, …):
			// don't burn retries on a deterministic failure.
			return stdout, stderr, exitCode, err
		}

		if attempt < maxCLIAgentRetries {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			reason := "no output"
			if transient {
				reason = "transient error"
			}
			if b.Logger != nil {
				b.Logger.Warn("[%s#%d/%s] %s (attempt %d/%d), retrying after %s",
					task.NodeID, task.Iteration, b.Protocol.Name, reason, attempt, maxCLIAgentRetries, backoff)
			}
			select {
			case <-ctx.Done():
				return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: context ended during retry: %w", b.Protocol.Name, ctx.Err())
			case <-time.After(backoff):
			}
		}
	}

	if err != nil {
		return stdout, stderr, exitCode, &ErrTransient{
			Provider: b.Protocol.Name,
			Reason:   "cli agent failed",
			Detail:   fmt.Sprintf("%v after %d attempts", err, maxCLIAgentRetries),
		}
	}
	return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: no output after %d attempts", b.Protocol.Name, maxCLIAgentRetries)
}

// runOnce executes a single CLI invocation, on the host or (when the task is
// sandboxed) inside the run's container, honouring the wall-clock timeout.
func (b *CLIAgentBackend) runOnce(ctx context.Context, task Task, binary string, args []string, stdinPrompt string, env []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var outBuf, errBuf bytes.Buffer
	argv := append([]string{binary}, args...)

	// A stdin-delivery protocol (PromptViaStdin) must route its prompt
	// through the sandbox ExecOpts, NOT by setting cmd.Stdin after the
	// driver builds the command: the docker/k8s drivers only allocate a
	// forwarded stdin (`docker exec --interactive`) when opts.Stdin (or
	// KeepStdinOpen) is set, so a post-hoc cmd.Stdin would be silently
	// dropped inside the container.
	var stdin io.Reader
	if stdinPrompt != "" {
		stdin = strings.NewReader(stdinPrompt)
	}

	var cmd *exec.Cmd
	if task.Sandbox != nil {
		// Record the in-container PID so cancellation/timeout can actually
		// terminate the agent: killing the host-side `docker exec` client
		// has no signal path to the exec'd process (same leak class as
		// native:221edac8 on the claude_code path — the fix there,
		// pidfile wrapper + explicit in-container kill, applies verbatim).
		// The wrapper also self-cleans on retry: attempt N kills whatever
		// attempt N-1 the pidfile still points to.
		mark := sandboxDelegateMark(task)
		cleanup := killSandboxDelegate(task.Sandbox, mark, b.Logger)
		defer cleanup()
		cmd = task.Sandbox.Command(runCtx, wrapSandboxDelegateArgv(mark, argv), sandbox.ExecOpts{
			Env:     b.sandboxEnv(ctx, task),
			WorkDir: task.WorkDir,
			Stdin:   stdin,
		})
	} else {
		cmd = exec.CommandContext(runCtx, binary, args...) // #nosec G204 — binary/args are backend-configured, not attacker-controlled.
		cmd.Env = env
		if task.WorkDir != "" {
			cmd.Dir = task.WorkDir
		}
		cmd.Stdin = stdin
	}
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	if stderr != "" && b.Logger != nil {
		for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
			if line != "" {
				b.Logger.Info("[%s#%d/%s:err] %s", task.NodeID, task.Iteration, b.Protocol.Name, line)
			}
		}
	}
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: %s failed: %w", b.Protocol.Name, binary, runErr)
	}
	return stdout, stderr, exitCode, nil
}

// sandboxEnv builds the environment a sandboxed CLI receives.
//
// A container inherits NOTHING from the host, so this map is the whole
// environment the agent gets. Besides the protocol's credential overrides it
// must therefore carry the run's own provisioning (task.ExtraEnv — the devbox
// profile PATH), which the earlier version dropped: a sandboxed agent could
// not see tools the run had just installed for it.
//
// SandboxEnv lets a protocol forward host credentials by name, which
// ResolveEnv (built for per-run overrides) does not do — the failure being a
// sandboxed pi node reporting "No API key found for <provider>" while the host
// had the key all along.
func (b *CLIAgentBackend) sandboxEnv(ctx context.Context, task Task) map[string]string {
	env := map[string]string{}
	switch {
	case b.Protocol.SandboxEnv != nil:
		for k, v := range b.Protocol.SandboxEnv(ctx, task) {
			env[k] = v
		}
	case b.Protocol.ResolveEnv != nil:
		for k, v := range b.Protocol.ResolveEnv(ctx) {
			env[k] = v
		}
	}
	// Run-level provisioning last so it wins on a duplicate key, matching the
	// host path's ordering.
	for _, kv := range task.ExtraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// parseStreamJSONText walks both the legacy claude-code-style NDJSON stream
// (`type: assistant` + terminal `type: result`) and kimi-code 0.23+'s native
// role stream (`role: assistant`, followed by a `role: meta` resume hint).
// It extracts the assistant's final text, session id, and token usage. Unknown
// lines are skipped; when nothing parses it falls back to the raw stream.
func parseStreamJSONText(stdout string) (text, sessionID string, tokens int) {
	var legacyAssistantText strings.Builder
	var nativeAssistantText string
	var resultText string
	haveResult := false

	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev == nil {
			continue
		}
		if sid, ok := ev["session_id"].(string); ok && sid != "" {
			sessionID = sid
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			tokens += asInt(u["input_tokens"]) + asInt(u["output_tokens"])
		}
		if role, _ := ev["role"].(string); role == "assistant" {
			var message strings.Builder
			switch content := ev["content"].(type) {
			case string:
				message.WriteString(content)
			case []any:
				for _, item := range content {
					if block, ok := item.(map[string]any); ok {
						if value, ok := block["text"].(string); ok {
							message.WriteString(value)
						}
					}
				}
			}
			// Native kimi role events are complete assistant messages, not
			// token deltas. Tool-using sessions can contain an early status
			// message followed by the actual final answer; keep the latest
			// non-empty message instead of concatenating both into invalid JSON.
			if message.Len() > 0 {
				nativeAssistantText = message.String()
			}
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "result":
			if r, ok := ev["result"].(string); ok {
				resultText = r
				haveResult = true
			}
		case "assistant":
			if msg, ok := ev["message"].(map[string]any); ok {
				if content, ok := msg["content"].([]any); ok {
					for _, c := range content {
						if blk, ok := c.(map[string]any); ok {
							if t, _ := blk["type"].(string); t == "text" {
								if txt, ok := blk["text"].(string); ok {
									legacyAssistantText.WriteString(txt)
								}
							}
						}
					}
				}
			}
		}
	}

	switch {
	case haveResult && resultText != "":
		return resultText, sessionID, tokens
	case nativeAssistantText != "":
		return nativeAssistantText, sessionID, tokens
	case legacyAssistantText.Len() > 0:
		return legacyAssistantText.String(), sessionID, tokens
	default:
		// Nothing recognisable — hand back the raw stream so the schema-aware
		// fallback can still try to extract a JSON object from it.
		return stdout, sessionID, tokens
	}
}
