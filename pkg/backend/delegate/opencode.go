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

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// BackendOpenCode is the registration name for the opencode backend
// (`backend: "opencode"` in the DSL). It drives the `opencode` agent CLI,
// whose headless protocol is disjoint from claude-code's Session mode:
//
//	opencode run --format json [-m provider/model[#variant]] \
//	         [--session <id> [--fork=true]]
//
// with the prompt on stdin. See ADR-065 for the CLI-agent seam.
const BackendOpenCode = backendcaps.OpenCode

// openCodeTrustProjectEnv opts into the target repository's own `.opencode/`
// resources. Refused by default — see openCodeProjectTrusted.
const openCodeTrustProjectEnv = "ITERION_OPENCODE_TRUST_PROJECT"

// openCodeLegacyArgvEnv pins the argv dialect to opencode 1.x: every
// generated flag first, the `run` subcommand last, the effort dial on
// `--variant`. The value is the major version it selects, so `=1` is the 1.x
// argv; anything else leaves the 2.x default in place. It is the escape
// hatch for a host still on the 1.x line, whose parser took either order —
// see opencodeLegacyProtocol.
const openCodeLegacyArgvEnv = "ITERION_OPENCODE_ARGV"

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
// `run` LEADS argv: opencode 2.x resolves the subcommand before it knows any
// flag, so a flag that precedes it is unrecognized — measured on 2.2.5,
// `opencode --format json -m … run` exits 1 with "Unrecognized flag:
// --format in command opencode" and prints its usage text on stdout. The 1.x
// line parsed either order; a host still on it sets ITERION_OPENCODE_ARGV=1
// (opencodeLegacyProtocol).
//
// There is no system-prompt flag, so the node's `system:` is folded into the
// user prompt as a preamble by the shared CLIAgentBackend path.
var opencodeProtocol = CLIAgentProtocol{
	Name:          BackendOpenCode,
	DefaultBinary: "opencode",

	// The subcommand opens argv; `run`'s own flags follow it.
	Subcommand: "run",

	PromptViaStdin: true,
	PromptFlag:     "",

	OutputFormatFlag: "--format",
	OutputFormat:     "json",

	// Model and effort are emitted together by opencodeExtraArgsFor:
	// opencode 2.x carries the variant INSIDE the model reference
	// (`-m provider/model#variant`, its own `run --help` wording) and has no
	// `--variant` flag at all. Emitting the halves independently cannot
	// spell it, and a variant with no model is a hard refusal.
	ModelFlag: "",
	MapModel:  nil,

	MapEffort: nil,

	ParseOutputRich: parseOpenCodeOutput,

	ResolveEnv: opencodeResolveEnv,

	// ITERION_OPENCODE_BIN names the CLI on the HOST, for a host whose PATH
	// the server process does not share. Consulted only off the sandbox path,
	// where a host path is meaningless. detect probes the same variable so
	// the report and the spawn agree on one binary.
	HostBinaryEnv: "ITERION_OPENCODE_BIN",

	// The model reference and the session flags, which follow `run`.
	ExtraArgsFor: opencodeExtraArgsFor,

	// PermissionHook nil: opencode exposes no PreToolUse hook. Its own
	// policy is declarative and in-process — OPENCODE_PERMISSION on the 1.x
	// line, REMOVED on 2.x where OPENCODE_CONFIG_CONTENT is the env
	// delivery (re-measured on 2.2.5 by TestCLILiveOpenCodeConfigContentDeny:
	// holds alone, fails open beside an agent-level permission, a
	// default_agent, or an invalid string) — but membership in the
	// compiler's gate table is earned by a live denial, never declared, so
	// a gated node is refused rather than trusted: preparePermissionHook
	// produces that refusal, typed (ErrCapabilityUnsupported) so recovery
	// never retries it (#1640).
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
// delegates to the shared CLI-agent path in the argv dialect the host speaks.
func (b *OpenCodeBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if err := refuseUntrustedOpenCodeProject(task.WorkDir); err != nil {
		return Result{BackendName: BackendOpenCode, ExitCode: -1}, err
	}
	cli := b.cli
	if openCodeLegacyArgv() {
		legacy := *cli
		legacy.Protocol = opencodeLegacyProtocol()
		cli = &legacy
	} else {
		b.warnEffortDial(task)
	}
	return cli.Execute(ctx, task)
}

// warnEffortDial reports a reasoning_effort the 2.x argv does not carry, in
// the two shapes the fold cannot spell. The variant travels inside the model
// reference: `-m "#variant"` with no model is refused by the CLI ("Invalid
// model reference: #high") and a reference that already spells one refuses a
// second ("Invalid model reference: provider/model#high#low"), both measured
// on 2.2.5. The run the node asked for still happens — at the model's default
// effort, or at the variant the operator wrote, whose explicit spelling
// outranks the dial — and the dropped dial is named rather than rejected,
// never in silence.
func (b *OpenCodeBackend) warnEffortDial(task Task) {
	if b.Logger == nil || opencodeMapEffort(task.ReasoningEffort) == "" {
		return
	}
	model := opencodeMapModel(task.Model)
	switch {
	case model == "":
		b.Logger.Warn("[%s#%d/%s] reasoning_effort %q is not emitted: opencode carries the variant inside the model reference (-m provider/model#variant), so the dial needs a model on the node",
			task.NodeID, task.Iteration, BackendOpenCode, task.ReasoningEffort)
	case strings.Contains(model, "#"):
		b.Logger.Warn("[%s#%d/%s] reasoning_effort %q is not emitted: the model reference %q already spells its variant, which opencode takes over the dial (a second #variant is refused)",
			task.NodeID, task.Iteration, BackendOpenCode, task.ReasoningEffort, model)
	}
}

// openCodeLegacyArgv reports whether the operator pinned the 1.x argv.
func openCodeLegacyArgv() bool {
	return strings.TrimSpace(os.Getenv(openCodeLegacyArgvEnv)) == "1"
}

// opencodeLegacyProtocol is opencodeProtocol in the 1.x dialect: no leading
// subcommand, `run` appended last by opencodeLegacyExtraArgsFor, the model on
// its own `-m` and the effort on `--variant`. Derived from the 2.x literal so
// that everything the two dialects agree on — the prompt on stdin, the
// parser, the env, the screens — is stated once.
func opencodeLegacyProtocol() CLIAgentProtocol {
	proto := opencodeProtocol
	proto.Subcommand = ""
	proto.ModelFlag = "-m"
	proto.MapModel = opencodeMapModel
	proto.MapEffort = opencodeLegacyMapEffort
	proto.ExtraArgsFor = opencodeLegacyExtraArgsFor
	return proto
}

// openCodeProjectTrusted reports whether the operator opted into the target
// repository's own `.opencode/` resources.
func openCodeProjectTrusted() bool {
	return strings.TrimSpace(os.Getenv(openCodeTrustProjectEnv)) == "1"
}

// refuseUntrustedOpenCodeProject refuses a run whose workspace would hand
// opencode code to execute.
//
// opencode loads project resources from every `.opencode/` directory at or
// above the working directory, plus an `opencode.json` at any of those levels
// — and it EXECUTES what it finds, in the process holding
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
	if openCodeProjectTrusted() {
		return nil
	}
	if workDir == "" {
		// Unreachable from a real run: the engine defaults its work dir to
		// os.Getwd() before any Task is built. Refused rather than screened,
		// because neither answer would be right — on the host the CLI would
		// inherit the SERVER's cwd, and under a sandbox the driver defaults
		// its --workdir to the bind-mounted checkout, so "nothing to screen"
		// would be a fail-OPEN on the very directory at issue.
		return fmt.Errorf("delegate: %s: no workspace to screen for opencode resources", BackendOpenCode)
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
//   - It does NOT stop at a git worktree root. 1.1.19 stopped there, and
//     this walk mirrored it; 2.2.5 climbs STRAIGHT PAST a `.git` and loads
//     an ancestor's plugin (measured end to end by the live screen bench,
//     TestLive_Feat_OpenCodeProjectScreen). A host carries one line or the
//     other, so the walk takes the UNION of their two reaches: a stop
//     predicate stronger than the CLI's is a hole, and that one was
//     measured open. What a level ABOVE the checkout costs is a refusal the
//     operator lifts with the trust hatch, which is the cheaper side.
//   - The operator's HOME is never screened. `~/.opencode` exists on every
//     host where opencode is installed — it is the installer's own root — so
//     screening it would refuse every run, and it is the operator's
//     directory, not the checkout.
//
// Running out of levels is an error, not a shorter list: a cap that
// truncates a security walk fails open.
func openCodeProjectLevels(workDir string) ([]string, error) {
	// Abs first, then EvalSymlinks — the order validateWorkDir uses. A
	// relative WorkDir resolves against the server's cwd, which is where
	// runOnce's cmd.Dir would put the CLI; EvalSymlinks(".") succeeds and
	// returns "." unchanged, so the other order screens one level of nothing.
	work, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace %q: %w", workDir, err)
	}
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
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
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		return levels, nil // filesystem root
	}
	return nil, fmt.Errorf(
		"workspace %q has more than %d path components above it and reaches neither the operator's home nor the filesystem root: cannot screen it for opencode resources",
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

// opencodeExtraArgsFor emits the model reference, then the session flags —
// everything that follows `run` and is not a protocol-level flag.
//
// A fresh session names none: opencode mints its own id and reports it on
// every event of the JSON stream, which parseOpenCodeOutput reads. A resume
// passes that id back with --session; a fork adds --fork, which opencode
// rejects without a session to fork from, so it never travels alone.
//
// --fork carries its value (`--fork=true`). Both forms parse under `run` on
// 2.2.5 (measured), and the value-carrying one is what the 1.x hatch needs:
// there the parser finds the subcommand before it knows `run`'s own flags, so
// a bare boolean right before `run` takes `run` as its value and the CLI
// prints its help instead (measured on 1.19.0).
//
// An id opencode does not hold is version-dependent: 1.19 exits 1 with
// "Session not found", so a lost transcript fails the call; 2.2.5 CREATES a
// session under that id (both measured), so there a resume whose transcript
// is gone silently becomes an empty conversation.
func opencodeExtraArgsFor(task Task) []string {
	var args []string
	if model := opencodeModelRef(task); model != "" {
		args = append(args, "-m", model)
	}
	return append(args, opencodeSessionArgs(task)...)
}

// opencodeLegacyExtraArgsFor emits the 1.x tail: the session flags, then the
// `run` subcommand that CLOSES argv (ITERION_OPENCODE_ARGV=1). The model and
// the effort travel on their own flags there, so the protocol emits them.
func opencodeLegacyExtraArgsFor(task Task) []string {
	return append(opencodeSessionArgs(task), "run")
}

// opencodeSessionArgs emits the session flags, which both dialects spell the
// same way.
func opencodeSessionArgs(task Task) []string {
	if task.SessionID == "" {
		return nil
	}
	args := []string{"--session", task.SessionID}
	if task.ForkSession {
		args = append(args, "--fork=true")
	}
	return args
}

// opencodeModelRef renders opencode 2.x's model reference —
// `provider/model[#variant]`, the shape its own `run --help` documents — with
// the reasoning dial folded in as the variant.
//
// The fold is conditional on a model that does not already spell one:
// `-m "#high"` is refused outright ("Invalid model reference: #high") and so
// is a second `#` ("Invalid model reference: provider/model#high#low"), both
// measured on 2.2.5. An effort with no model emits no `-m` at all; a
// reference the operator wrote with its variant is emitted as written, the
// explicit spelling outranking the dial. OpenCodeBackend.Execute warns by
// name in both cases (warnEffortDial).
func opencodeModelRef(task Task) string {
	model := opencodeMapModel(task.Model)
	if model == "" || strings.Contains(model, "#") {
		return model
	}
	if variant := opencodeMapEffort(task.ReasoningEffort); variant != "" {
		return model + "#" + variant
	}
	return model
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
// variant name, the token opencodeModelRef folds into `-m`. The accepted
// names are per-MODEL (opencode derives them from the model's own reasoning
// options) and iterion cannot enumerate them, so the level is passed through
// verbatim and the CLI refuses an unknown one explicitly — measured on 2.2.5:
// `{"error":{"type":"provider.no-route","message":"Variant unavailable for
// scripted/m1: nonexistentvariant"}}`, exit 1.
//
// `ultracode` and `xhigh` are remapped to `high`: opencode has no ultracode
// mode, whose orchestration half is Anthropic-only (docs/ultracode.md) — the
// same remap grok does.
func opencodeMapEffort(effort string) string {
	effort = strings.TrimSpace(strings.ToLower(effort))
	switch effort {
	case "":
		return ""
	case "ultracode", "xhigh":
		return "high"
	}
	return effort
}

// opencodeLegacyMapEffort spells the same dial the 1.x way, on its own
// `--variant` flag (ITERION_OPENCODE_ARGV=1). The flag does not exist on the
// 2.x parser, at the root or under `run`.
//
// Measured on 1.1.19: a variant the model does not carry is DROPPED in
// silence there — `resolveVariant` returns the flag unchecked and the request
// then does a bare record lookup that yields nothing. Neither the CLI nor
// this parser can tell that from an honoured dial, so under the hatch a node
// may run at the model's default effort with no signal.
func opencodeLegacyMapEffort(effort string) []string {
	variant := opencodeMapEffort(effort)
	if variant == "" {
		return nil
	}
	return []string{"--variant", variant}
}

// parseOpenCodeOutput walks opencode's `--format json` stream: one JSON
// object per line, every one carrying "type" and "sessionID".
//
//	{"type":"text","sessionID":"…","part":{"text":"…"}}
//	{"type":"step_finish","sessionID":"…","part":{"tokens":{…},"cost":0.01}}
//	{"type":"error","sessionID":"…","error":{"name":"…","data":{"message":"…"}}}
//	{"type":"error","sessionID":"…","error":{"type":"…","message":"…"}}
//
// The last two are the same event in the 1.x and 2.x envelope shapes, both
// read by openCodeErrorMessage. A 2.x error travels on stdout even when the
// process exits non-zero, which is why the shared path parses the stream on
// a failing exit too (CLIAgentBackend.failureReason).
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

// openCodeErrorMessage renders an opencode error envelope, preferring a human
// message over the error's class, across both envelope shapes the CLI has
// shipped: `{"name":…,"data":{"message":…}}` on the 1.x line and
// `{"type":…,"message":…}` on 2.x (measured on 2.2.5: `provider.no-route` /
// "Model unavailable: …", `unknown` / "--fork requires --continue or
// --session"). Reading only the 1.x keys returned "" for every 2.x failure,
// and parseOpenCodeOutput then reported no error at all.
func openCodeErrorMessage(raw any) string {
	err, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	if data, ok := err["data"].(map[string]any); ok {
		if msg := openCodeErrorField(data, "message"); msg != "" {
			return msg
		}
	}
	for _, key := range []string{"message", "name", "type"} {
		if msg := openCodeErrorField(err, key); msg != "" {
			return msg
		}
	}
	return ""
}

// openCodeErrorField reads one trimmed string field of an error envelope.
func openCodeErrorField(envelope map[string]any, key string) string {
	s, _ := envelope[key].(string)
	return strings.TrimSpace(s)
}
