package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// BackendOpenCode is the registration name for the opencode backend
// (`backend: "opencode"` in the DSL). It drives the `opencode` agent CLI,
// whose headless protocol is disjoint from claude-code's Session mode:
//
//	opencode --format json [-m provider/model] [--variant <effort>] run
//
// with the prompt on stdin. See ADR-065 for the CLI-agent seam.
const BackendOpenCode = "opencode"

// openCodeTrustProjectEnv opts into the target repository's own `.opencode/`
// resources. Refused by default — see openCodeProjectTrusted.
const openCodeTrustProjectEnv = "ITERION_OPENCODE_TRUST_PROJECT"

// openCodeProjectConfigFiles are the workspace config files opencode reads.
// Either can carry a `plugin:` list pointing at arbitrary TypeScript.
var openCodeProjectConfigFiles = []string{"opencode.json", "opencode.jsonc"}

// openCodeProjectDir is the per-directory resource root opencode discovers.
// Everything below it is load-bearing: `plugin/` and `tool/` are imported as
// modules, and a `package.json` there is installed with its lifecycle
// scripts. Enumerating the safe subset is the game that never converges, so
// the question asked is "is there one at all".
const openCodeProjectDir = ".opencode"

// openCodeMaxProjectLevels bounds the upward walk so a pathological path can
// never turn the screen into a filesystem crawl.
const openCodeMaxProjectLevels = 64

// opencodeProtocol describes the opencode CLI's headless invocation.
//
// The prompt travels on stdin with no flag, for the same three reasons pi
// does it (pi.go): opencode's `run` takes the message as a variadic
// POSITIONAL, so a composed prompt beginning with "-" — an iterion prompt
// routinely opens on a markdown bullet — is parsed as flags and the CLI
// prints its help text instead of running; a composed prompt can exceed
// MAX_ARG_STRLEN (128 KiB); and opencode reads non-TTY stdin to EOF and
// merges it into the message, so a reader that EOFs immediately is what
// makes that behaviour benign. (A stdin left OPEN hangs opencode forever;
// runOnce passes a nil Stdin here, which os/exec wires to the null device.)
//
// `run` is emitted through ExtraArgs rather than a dedicated protocol field:
// opencode's parser accepts the subcommand token after its options, so
// `opencode --format json -m … run` and `opencode run --format json -m …`
// parse identically.
//
// There is no system-prompt flag, so the node's `system:` is folded into the
// user prompt as a preamble by the shared CLIAgentBackend path.
var opencodeProtocol = CLIAgentProtocol{
	Name:          BackendOpenCode,
	DefaultBinary: "opencode",

	PromptViaStdin: true,
	PromptFlag:     "",

	OutputFormatFlag: "--format",
	OutputFormat:     "json",

	ModelFlag: "-m",
	MapModel:  opencodeMapModel,

	MapEffort: opencodeMapEffort,

	ParseOutputRich: parseOpenCodeOutput,

	ResolveEnv: opencodeResolveEnv,

	// ITERION_OPENCODE_BIN names the CLI on the HOST, for a host whose PATH
	// the server process does not share. Consulted only off the sandbox path,
	// where a host path is meaningless. detect probes the same variable so
	// the report and the spawn agree on one binary.
	HostBinaryEnv: "ITERION_OPENCODE_BIN",

	// The subcommand closes argv: every generated flag precedes it.
	ExtraArgs: []string{"run"},

	// PermissionHook nil: opencode exposes no PreToolUse hook. Its own
	// policy (OPENCODE_PERMISSION) is declarative and in-process, but
	// membership in the compiler's gate table is earned by a live denial,
	// never declared, so a gated node is refused rather than trusted —
	// preparePermissionHook produces that refusal.
}

// OpenCodeBackend drives opencode. It wraps the CLIAgentBackend with the
// checks that cannot live in a pure-data CLIAgentProtocol — the same seam
// PiBackend occupies for the same reason.
type OpenCodeBackend struct {
	cli    *CLIAgentBackend
	Logger *iterlog.Logger
}

// NewOpenCodeBackend constructs the opencode backend. command overrides the
// default `opencode` binary (a pinned build or wrapper path); empty uses the
// binary on PATH.
func NewOpenCodeBackend(logger *iterlog.Logger, command string) *OpenCodeBackend {
	return &OpenCodeBackend{
		cli: &CLIAgentBackend{
			Protocol: opencodeProtocol,
			Command:  command,
			Logger:   logger,
		},
		Logger: logger,
	}
}

// Execute refuses a workspace whose own `.opencode/` would run as code, then
// delegates to the shared CLI-agent path.
func (b *OpenCodeBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if err := refuseUntrustedOpenCodeProject(task.WorkDir); err != nil {
		return Result{BackendName: BackendOpenCode, ExitCode: -1}, err
	}
	return b.cli.Execute(ctx, task)
}

// openCodeProjectTrusted reports whether the operator opted into the target
// repository's own `.opencode/` resources.
func openCodeProjectTrusted() bool {
	return strings.TrimSpace(os.Getenv(openCodeTrustProjectEnv)) == "1"
}

// refuseUntrustedOpenCodeProject refuses a run whose workspace would hand
// opencode code to execute.
//
// opencode loads project resources from every `.opencode/` directory between
// the working directory and the repository root, plus an `opencode.json` at
// any of those levels — and it EXECUTES what it finds, in the process holding
// the run's credentials: `plugin/*.ts` and `tool/*.ts` are imported as
// modules, and a `package.json` there is installed with its lifecycle scripts.
// Trusting a checked-out repository therefore turns prompt injection into code
// execution. This is pi's `--no-approve` posture (pi.go), expressed as a
// refusal because opencode (measured on 1.1.19) offers no flag and no
// environment variable that disables the discovery: OPENCODE_DISABLE_PROJECT_CONFIG,
// OPENCODE_PURE and OPENCODE_DISABLE_DEFAULT_PLUGINS each left it running.
//
// The check is one question per directory level rather than a list of known
// code paths: a guard that enumerates spellings gains one more every release,
// while "does this level carry opencode resources at all" cannot be widened.
//
// Opt back in with ITERION_OPENCODE_TRUST_PROJECT=1, for a repository you own.
func refuseUntrustedOpenCodeProject(workDir string) error {
	if workDir == "" || openCodeProjectTrusted() {
		return nil
	}
	levels, err := openCodeProjectLevels(workDir)
	if err != nil {
		return fmt.Errorf("delegate: %s: %w", BackendOpenCode, err)
	}
	for _, dir := range levels {
		found, err := openCodeResourceAt(dir)
		if err != nil {
			return fmt.Errorf("delegate: %s: cannot screen %q for opencode resources: %w",
				BackendOpenCode, dir, err)
		}
		if found == "" {
			continue
		}
		return fmt.Errorf(
			"delegate: %s: %s carries %s, which opencode loads and executes inside the agent process; "+
				"refusing to run against an untrusted checkout (set %s=1 for a repository you own)",
			BackendOpenCode, dir, found, openCodeTrustProjectEnv)
	}
	return nil
}

// openCodeProjectLevels lists the directories opencode would read resources
// from for this run, mirroring its own discovery walk.
//
// Three properties, each measured on 1.1.19 rather than assumed:
//
//   - The walk is PHYSICAL. opencode resolves resources from the spawned
//     process's working directory, and runOnce hands the CLI a cmd.Dir whose
//     cwd the kernel reports resolved. A lexical walk over a path that
//     crosses a symlink screens directories the CLI never reads, and misses
//     the ones it does.
//   - It stops at the GIT WORKTREE ROOT: a plugin one level above a git root
//     is not loaded. Below that root is the checkout, which is the untrusted
//     surface; a tree with no repository is climbed well past four levels.
//   - The operator's HOME is never screened. `~/.opencode` exists on every
//     host where opencode is installed — it is the installer's own root — so
//     screening it would refuse every run, and it is the operator's
//     directory, not the checkout.
//
// Running out of levels is an error, not a shorter list: a cap that
// truncates a security walk fails open.
func openCodeProjectLevels(workDir string) ([]string, error) {
	work, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		if work, err = filepath.Abs(workDir); err != nil {
			return nil, fmt.Errorf("resolve workspace %q: %w", workDir, err)
		}
	}
	home := openCodeOperatorHome()

	var levels []string
	dir := work
	for i := 0; i <= openCodeMaxProjectLevels; i++ {
		if home != "" && dir == home {
			// The operator's own tree: stop WITHOUT screening it.
			return levels, nil
		}
		levels = append(levels, dir)
		if isGitWorktreeRoot(dir) {
			return levels, nil
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		return levels, nil // filesystem root
	}
	return nil, fmt.Errorf(
		"workspace %q sits more than %d levels below its repository root: cannot screen it for opencode resources",
		work, openCodeMaxProjectLevels)
}

// openCodeOperatorHome is the resolved home directory, or "" when it cannot
// be determined — in which case the walk simply has one fewer stop.
func openCodeOperatorHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		return resolved
	}
	return home
}

// isGitWorktreeRoot reports whether dir holds a `.git` entry — a directory
// for a primary checkout, a file for a linked worktree or a submodule.
// Either truncates opencode's own walk, so either truncates this one.
func isGitWorktreeRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// openCodeResourceAt names the opencode resource found at one directory
// level, or "" when there is none. An error means the level could not be
// screened, which is never read as "clean".
func openCodeResourceAt(dir string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, openCodeProjectDir))
	switch {
	case err == nil && len(entries) > 0:
		return openCodeProjectDir + "/", nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	for _, name := range openCodeProjectConfigFiles {
		switch _, err := os.Stat(filepath.Join(dir, name)); {
		case err == nil:
			return name, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
	}
	return "", nil
}

// opencodeResolveEnv pins the host state opencode would otherwise fold into a
// run: its Claude-Code compatibility layer reads the OPERATOR's
// ~/.claude/CLAUDE.md into the system prompt and loads `.claude` skills, so a
// node's effective prompt would depend on a directory iterion does not
// compose — and a single malformed SKILL.md there aborts the whole CLI.
// iterion owns prompt composition; this is pi's --no-prompt-templates /
// --no-themes posture.
func opencodeResolveEnv(context.Context) map[string]string {
	return map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE": "1"}
}

// opencodeMapModel passes an iterion model spec through unchanged: opencode's
// own `-m` format is `provider/model`, which is iterion's spec shape already.
// A bare id is left alone — opencode resolves it against its configured
// providers and reports an explicit ProviderModelNotFoundError when it
// cannot, which is a better diagnosis than a prefix iterion guessed.
func opencodeMapModel(model string) string {
	return strings.TrimSpace(model)
}

// opencodeMapEffort maps iterion's reasoning_effort dial onto opencode's
// `--variant`. The accepted variant names are per-MODEL (opencode derives
// them from the model's own reasoning options) and iterion cannot enumerate
// them, so the level is passed through verbatim.
//
// Measured on 1.1.19: a variant the model does not carry is DROPPED in
// silence — `resolveVariant` returns the flag unchecked and the request then
// does a bare record lookup that yields nothing. Neither the CLI nor this
// parser can tell that from an honoured dial, so a node may run at the
// model's default effort with no signal. `ultracode` is remapped to `high`:
// opencode has no ultracode mode, whose orchestration half is Anthropic-only
// (docs/ultracode.md) — the same remap grok does.
func opencodeMapEffort(effort string) []string {
	effort = strings.TrimSpace(strings.ToLower(effort))
	switch effort {
	case "":
		return nil
	case "ultracode", "xhigh":
		effort = "high"
	}
	return []string{"--variant", effort}
}

// parseOpenCodeOutput walks opencode's `--format json` stream: one JSON
// object per line, every one carrying "type" and "sessionID".
//
//	{"type":"text","sessionID":"…","part":{"text":"…"}}
//	{"type":"step_finish","sessionID":"…","part":{"tokens":{…},"cost":0.01}}
//	{"type":"error","sessionID":"…","error":{"name":"…","data":{"message":"…"}}}
//
// Token semantics follow opencode's own accounting (session.ts getUsage):
// `output` EXCLUDES reasoning and `input` EXCLUDES the cache halves, so both
// are added back here to report the real split CLIAgentParse documents —
// thinking stays a subset of output, never added to the total.
func parseOpenCodeOutput(stdout string) CLIAgentParse {
	var out CLIAgentParse
	var text strings.Builder
	var errMsgs []string
	sawEvent := false

	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil || ev == nil {
			continue
		}
		typ, _ := ev["type"].(string)
		if typ == "" {
			continue
		}
		sawEvent = true
		if sid, ok := ev["sessionID"].(string); ok && sid != "" {
			out.SessionID = sid
		}
		part, _ := ev["part"].(map[string]any)

		switch typ {
		case "text":
			if part != nil {
				if s, ok := part["text"].(string); ok {
					text.WriteString(s)
				}
			}
		case "step_finish", "step-finish":
			if part == nil {
				continue
			}
			if tokens, ok := part["tokens"].(map[string]any); ok {
				reasoning := asInt(tokens["reasoning"])
				out.InputTokens += asInt(tokens["input"]) + openCodeCacheTokens(tokens)
				out.OutputTokens += asInt(tokens["output"]) + reasoning
				out.ThinkingTokens += reasoning
			}
			if cost, ok := part["cost"].(float64); ok && cost > 0 {
				out.CostUSD += cost
			}
		case "error":
			if msg := openCodeErrorMessage(ev["error"]); msg != "" {
				errMsgs = append(errMsgs, msg)
			}
		}
	}

	if len(errMsgs) > 0 {
		out.Err = fmt.Errorf("delegate: %s: %s", BackendOpenCode, strings.Join(errMsgs, "; "))
	}
	if sawEvent {
		out.Text = text.String()
		return out
	}
	// Nothing parsed: hand the raw stream to the shared schema-aware
	// fallback rather than reporting an empty answer.
	out.Text = stdout
	return out
}

// openCodeCacheTokens sums the cache halves opencode subtracts from `input`.
func openCodeCacheTokens(tokens map[string]any) int {
	cache, ok := tokens["cache"].(map[string]any)
	if !ok {
		return 0
	}
	return asInt(cache["read"]) + asInt(cache["write"])
}

// openCodeErrorMessage renders an opencode error envelope, preferring the
// nested human message over the error's class name.
func openCodeErrorMessage(raw any) string {
	err, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	if data, ok := err["data"].(map[string]any); ok {
		if msg, ok := data["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
	}
	if name, ok := err["name"].(string); ok {
		return strings.TrimSpace(name)
	}
	return ""
}
