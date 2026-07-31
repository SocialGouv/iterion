package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// BackendPi is the registration name for the pi coding-agent backend
// (`backend: "pi"` in the DSL). pi (https://pi.dev,
// github.com/earendil-works/pi) is a multi-provider agent harness whose
// headless protocol is disjoint from claude-code's Session mode:
//
//	pi --mode json --provider <p> --model <id> [--thinking <level>] \
//	   --append-system-prompt <file> [--session-id <id>] < prompt
//
// What pi brings that no other wired backend does is provider breadth —
// ~36 first-class providers behind one agent loop, with its own credential
// store and OAuth flows — plus a per-message cost figure the provider
// computed, which makes budget enforcement quantitatively correct instead
// of estimated.
//
// What it does NOT bring, and what workflows must not assume: pi's built-in
// tool set (read, bash, edit, write, grep, find, ls) is a strict subset of
// claude_code's. There is no todo, no subagent/Task, no web fetch/search,
// no notebook, no background bash, and no MCP client at all — so board
// capabilities, ask_user and workflow-declared `mcp_server` blocks do not
// reach a pi node. Reach for pi to run a model claude_code cannot, not to
// replace claude_code on a workflow that already works.
//
// Credentials are resolved by pi itself from ~/.pi/agent/auth.json and the
// provider env vars; ResolveEnv only layers per-run BYOK keys on top and, by
// default, strips the Anthropic OAuth subscription (see piGuardForfait). See
// ADR-065 for the CLI-agent seam and ADR-085 for this backend.
const BackendPi = "pi"

// piProtocol describes pi's headless invocation.
var piProtocol = CLIAgentProtocol{
	Name:          BackendPi,
	DefaultBinary: "pi",

	// Prompt on stdin, with no flag. Three independent reasons, each
	// sufficient on its own:
	//
	//  1. `-p <prompt>` silently DROPS a prompt starting with "-" or "@":
	//     pi only consumes the next argv entry as the message when it
	//     doesn't look like a flag or a file attachment. An iterion prompt
	//     routinely starts with a markdown bullet.
	//  2. A composed prompt can exceed MAX_ARG_STRLEN (128 KiB).
	//  3. pi reads non-TTY stdin to EOF and merges it into the initial
	//     message. Handing it a reader that EOFs immediately is what makes
	//     that behaviour benign — a pipe left open would hang pi forever.
	PromptViaStdin: true,
	PromptFlag:     "",

	// pi ships a native agentic system prompt assembled from the ACTIVE
	// tool set, so `--system-prompt` (which replaces it) would strip the
	// per-tool guidelines the agent expects — the same trap as
	// claude_code's `--system-prompt` and grok's `--system-prompt-override`.
	// Pairs with SystemPromptAppendToNative.
	SystemPromptFlag:    "--append-system-prompt",
	SystemPromptViaFile: true,

	// Provider and model are emitted together by piExtraArgsFor: pi only
	// strips a `provider/` prefix from the model pattern when it matches an
	// explicit --provider, so emitting the halves independently can yield an
	// unresolvable pattern.
	ModelFlag: "",
	MapModel:  nil,

	// ITERION_PI_BIN names a binary on the HOST. Resolving it here, once, would
	// put that path in argv[0] for sandboxed runs too — and nothing bind-mounts
	// it into the container, while the published images already ship pi on PATH.
	// The documented use case ("a host with no Node") is a host concern, so the
	// lookup happens per task and only when there is no sandbox.
	HostBinaryEnv: "ITERION_PI_BIN",

	MapEffort: piMapEffort,
	// Rebound by NewPiBackend to carry that backend's logger. The default has
	// to stay non-nil: this value is copied by anything building a pi
	// CLIAgentBackend by hand, and a nil field there is argv silently lost.
	ExtraArgsFor:    func(task Task) []string { return piExtraArgsFor(task, nil) },
	ParseOutputRich: parsePiOutput,
	ResolveEnv:      piResolveEnv,
	SandboxEnv:      piSandboxEnv,

	ExtraArgs: []string{
		"--mode", "json",
		// Refuse the target repository's own .pi/ resources. pi executes
		// project-local extensions as TypeScript inside the agent process —
		// the process holding the run's credentials — so trusting a
		// checked-out repo turns prompt injection into code execution.
		// Opt back in per node with ITERION_PI_TRUST_PROJECT=1.
		"--no-approve",
		// iterion owns prompt composition; pi's own template/theme
		// discovery would make a run's prompt depend on operator state.
		"--no-prompt-templates", "--no-themes",
	},
}

// NewPiBackend constructs the pi backend. command overrides the default `pi`
// binary (a pinned build or wrapper path); empty falls back to ITERION_PI_BIN
// and then to the binary on PATH.
//
// ITERION_PI_BIN is the documented escape hatch for a host that cannot run the
// npm CLI — a `bun --compile` single-file build, or an air-gapped machine with
// no Node. It was documented and never implemented: the variable existed only
// in the reference, so an operator who set it got the PATH binary anyway, with
// nothing saying why.
func NewPiBackend(logger *iterlog.Logger, command string) *PiBackend {
	proto := piProtocol
	proto.ExtraArgsFor = func(task Task) []string { return piExtraArgsFor(task, logger) }
	return &PiBackend{
		print: &CLIAgentBackend{
			Protocol: proto,
			Command:  command,
			Logger:   logger,
		},
		rpc:    &PiRPCBackend{Command: command, Logger: logger},
		Logger: logger,
	}
}

// PiBackend drives pi. It wraps the print-mode CLIAgentBackend with the
// checks that cannot live in a pure-data CLIAgentProtocol, and is the seam
// where the richer `--mode rpc` transport is selected once it lands: print
// vs RPC is a transport, not a contract, so both share this backend name and
// every mapping below. ITERION_PI_MODE pins one for an operator who needs to
// roll back.
type PiBackend struct {
	print *CLIAgentBackend
	// rpc drives `pi --mode rpc`; nil until that transport ships.
	rpc    Backend
	Logger *iterlog.Logger
}

// Execute runs the task through the selected pi transport.
func (b *PiBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if err := b.noticeSubscriptionOAuth(ctx, task); err != nil {
		return Result{BackendName: BackendPi, ExitCode: -1}, err
	}
	// Everything pi writes for this node — the extension bundle, the composed
	// system prompt, the session transcripts, the seeded credential — lands in
	// one state root. When the run has somewhere better than the target
	// repository's checkout, that is where it goes and there is nothing here to
	// defend: the repo cannot pre-populate a directory it has no part in.
	//
	// The guards below exist for the fallback (host_state=none, the kubernetes
	// driver), where the workspace bind is the only thing the container can read
	// and the premise they were written for holds again. Gating them on
	// containment rather than running them unconditionally is what makes the
	// default path actually stop touching the target tree.
	// The whole pre-flight lives in one testable function. Three review rounds
	// running, the defect was in wiring that no test could reach because the
	// decision was inlined here and only its helpers had tests.
	if err := piPrepareStateRoot(task, b.Logger); err != nil {
		return Result{BackendName: BackendPi, ExitCode: -1}, err
	}
	// pi's openai-codex provider has no API-key path, so a ChatGPT plan only
	// reaches it through a seeded agent dir. Riding task.ExtraEnv puts it on
	// both transports at once, and on the sandboxed path with them.
	codexEnv, cleanupCodex, err := piCodexSeed(ctx, task, b.Logger)
	if err != nil {
		return Result{BackendName: BackendPi, ExitCode: -1}, err
	}
	defer cleanupCodex()
	for k, v := range codexEnv {
		task.ExtraEnv = append(task.ExtraEnv, k+"="+v)
	}
	// Transport selection. RPC is the default because it is strictly higher
	// fidelity — tool events reach the studio timeline, operator chat rides
	// pi's native steering, accounting comes from get_session_stats, and a
	// pre-flight handshake resolves the model before any token is spent —
	// while every mapping (model, effort, argv, credentials, system prompt) is
	// shared verbatim with print mode. The token count matches across both,
	// which is what makes the switch safe for a workflow's max_tokens budget
	// (asserted by TestPiRPCLiveEquivalence).
	//
	// ITERION_PI_MODE=print is the rollback for an operator who hits a
	// transport-specific problem in the field.
	switch strings.TrimSpace(strings.ToLower(os.Getenv("ITERION_PI_MODE"))) {
	case "print":
		return b.executePrint(ctx, task)
	default:
		if b.rpc == nil {
			return b.executePrint(ctx, task)
		}
		return b.rpc.Execute(ctx, task)
	}
}

// executePrint runs the print transport after checking what it cannot carry.
//
// Every iterion-specific capability on pi — the permission gate, ask_user, the
// board and workflow-declared MCP servers — is supplied by the embedded
// extension, which only the RPC path materialises and loads. Print mode is
// therefore not merely lower-fidelity: it silently drops those surfaces. The
// permission gate is the one that must FAIL rather than degrade, mirroring the
// RPC path's own rule — a gated node running ungated is a false sense of
// security, and it is exactly the anti-prompt-injection boundary the gate
// exists to hold. The rest degrade with a warning, because a node can still do
// useful work without them.
func (b *PiBackend) executePrint(ctx context.Context, task Task) (Result, error) {
	if task.Permission.Enabled() {
		return Result{BackendName: BackendPi, ExitCode: -1}, fmt.Errorf(
			"pi backend: this node declares a permission gate, which the print transport cannot " +
				"enforce — the iterion extension that IS the gate loads only on the rpc transport. " +
				"Unset ITERION_PI_MODE (or set it to rpc) to run this node")
	}
	if b.Logger != nil {
		var dropped []string
		if task.InteractionEnabled {
			dropped = append(dropped, "ask_user")
		}
		if len(task.Capabilities) > 0 {
			dropped = append(dropped, "board capabilities")
		}
		if len(task.MCPServers) > 0 {
			dropped = append(dropped, "workflow-declared MCP servers")
		}
		if len(dropped) > 0 {
			b.Logger.Warn("[%s#%d/%s] print transport: %s are INACTIVE for this node "+
				"(the iterion extension loads only on the rpc transport)",
				task.NodeID, task.Iteration, BackendPi, strings.Join(dropped, ", "))
		}
	}
	return b.print.Execute(ctx, task)
}

// noticeSubscriptionOAuth warns when this node is about to spend an Anthropic
// subscription OAuth token, and refuses only if the operator opted out.
//
// pi speaks the Messages API directly rather than spawning Anthropic's CLI,
// which was once read as putting it out of policy. Anthropic's API settled it:
// the token is ACCEPTED from a third-party app and billed against a separate
// extra-usage balance instead of the plan's limits (see
// piIsExtraUsageExhausted for the response when that balance empties). The
// vendor's line is about billing, not about which client — so this is a
// notice, not a bar. The operator is nonetheless spending a different pot than
// they may expect, which is worth one warning line per node.
//
// A user's own `pi` login in ~/.pi/agent/auth.json is their relationship with
// the vendor: nothing is read or injected here.
func (b *PiBackend) noticeSubscriptionOAuth(ctx context.Context, task Task) error {
	if provider, _ := piResolveModel(task.Model, task.ProviderHint); provider != "anthropic" {
		return nil
	}
	if err := secrets.GuardSubscriptionOAuth(ctx, secrets.ProviderAnthropic, secrets.OAuthKindClaudeCode); err != nil {
		return fmt.Errorf("pi backend: %w", err)
	}
	if b.Logger != nil && secrets.SubscriptionOAuthOnly(ctx, secrets.ProviderAnthropic, secrets.OAuthKindClaudeCode) {
		b.Logger.Warn("[%s#%d/%s] %s", task.NodeID, task.Iteration, BackendPi,
			secrets.SubscriptionOAuthNotice(secrets.ProviderAnthropic))
	}
	return nil
}

// piProviderPrefixes maps an iterion routing prefix onto pi's provider id.
// Prefixes absent from the table pass through verbatim: pi registers ~36
// providers under their common names, so identity is the right default and
// an unknown one is pi's error to report, not ours to guess at.
var piProviderPrefixes = map[string]string{
	"google":   "google",
	"gemini":   "google",
	"vertex":   "google-vertex",
	"bedrock":  "amazon-bedrock",
	"foundry":  "azure-openai-responses",
	"azure":    "azure-openai-responses",
	"copilot":  "github-copilot",
	"moonshot": "moonshotai",
	"kimi":     "moonshotai",
}

// piResolveModel splits an iterion model spec into pi's --provider/--model
// pair. A bare id yields an empty provider, letting pi resolve it against
// its own catalogue.
//
// hint (the node's `provider:` chain entry) OVERRIDES the spec's own prefix.
// That is not a stylistic choice — it is what makes z.ai work. iterion
// expresses z.ai as `model: "anthropic/glm-5.2"` + `provider: "zai"`,
// because z.ai serves an Anthropic-compatible surface behind
// ANTHROPIC_BASE_URL. pi instead has a first-class `zai` provider, so the
// correct invocation is `--provider zai --model glm-5.2`; passing
// `anthropic/glm-5.2` through would fuzzy-match Anthropic's own catalogue
// and silently run a different model.
func piResolveModel(model, hint string) (provider, modelID string) {
	model = strings.TrimSpace(model)
	hint = strings.TrimSpace(strings.ToLower(hint))

	modelID = model
	if idx := strings.Index(model, "/"); idx > 0 {
		prefix := strings.ToLower(model[:idx])
		rest := strings.TrimSpace(model[idx+1:])
		if rest != "" {
			provider = piMapProvider(prefix)
			modelID = rest
		}
	}
	if hint != "" {
		provider = piMapProvider(hint)
	}
	return provider, modelID
}

// piSkillArgs builds the `--skill` flags for this node.
//
// It offers pi only the skills ITERION wrote into this workspace, as reported
// by the engine on Task.MirroredSkills. `<workDir>/.claude/skills` is a
// checkout of the TARGET repository under `worktree: auto`, so a repo can ship
// its own skills there — and CLI `--skill` paths bypass the project-trust gate
// that `--no-approve` exists to close. For a webhook-launched review or triage
// bot against an untrusted repo, handing those over is attacker-authored prompt
// text loaded as trusted.
//
// The list has to come from the engine. An earlier attempt read provenance
// markers the mirror leaves in the workspace, which is not a trust boundary at
// all: the markers live inside the very checkout they were meant to vouch
// against, so a repo can forge them. Nothing recovered from the workspace can
// establish this; only the side that did the writing knows.
//
// `ITERION_PI_TRUST_PROJECT=1` is the documented opt-in that accepts the repo's
// own skills, and remains the only way to get them.
//
// Every path emitted here is one pi resolves: `--skill` is stat'd and dispatched
// to a directory scan or a single-file load (core/skills.ts), so both mirror
// shapes work — a `<name>/` directory holding SKILL.md (library skills and
// directory-form bundle skills) and a flat `<stem>.md` (plugin and flat bundle
// skills).
//
// Historical note on the other direction: the gate was once
// `len(task.SkillHints) > 0`, which carries only the DSL `skills:` field (the
// skill *library*) — so `--skill` was never emitted for a BUNDLE bot, pi had no
// skill awareness at all, and an agent whose own prompt ordered "LOAD YOUR
// SKILLS FIRST" was left hunting for files. claude_code discovers this
// directory natively and claw's `skill` tool reads it; this was a pi-only hole.
func piSkillArgs(task Task, logger *iterlog.Logger) []string {
	if task.WorkDir == "" {
		return nil
	}
	dir := filepath.Join(task.WorkDir, ".claude", "skills")

	// The documented opt-in: trust the repo's extensions, skills and settings.
	// The directory is created lazily — the mirrors only MkdirAll it when they
	// have something to write — so a bundle-less bot against a repo with no
	// .claude/skills/ would otherwise be handed a path that does not exist. The
	// branch below drops unresolvable paths for the same reason; these two must
	// not disagree.
	if strings.TrimSpace(os.Getenv("ITERION_PI_TRUST_PROJECT")) == "1" {
		if _, err := os.Stat(dir); err != nil && task.Hostless() {
			return nil
		}
		return []string{"--skill", dir}
	}

	// MirroredSkills is the ONLY source. Deriving a path from a SkillHint name
	// instead would route straight around this gate: a hint is recorded for
	// every skill the workflow references, INCLUDING one the target repo
	// shadowed — the hint describes what the agent will see, not who wrote it.
	// Synthesising `<dir>/<name>` from it therefore handed the repo's own file
	// to pi for any DSL `skills:` reference it chose to pre-empt. The engine
	// reports library skills on this same list, minus the shadowed ones.
	paths := task.MirroredSkills
	named := len(paths)
	seen := map[string]bool{}
	var args []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			// Under a sandbox WorkDir names an in-container path the host
			// cannot stat; offer it anyway — the engine says it wrote it, and
			// a path pi cannot resolve costs a skill, not the run. A noop
			// sandbox executes on the host, so it is not that case.
			if task.Hostless() {
				continue
			}
		}
		seen[p] = true
		args = append(args, "--skill", p)
	}
	// The silent-zero-skill failure this whole function exists to end: the
	// engine named skills, none of them resolved, and pi runs with none while
	// the bot's prompt tells the agent to load them. Nothing else reports it.
	if len(args) == 0 && named > 0 && logger != nil {
		logger.Warn("[%s#%d/%s] no skills offered to pi: the engine named %d for this run "+
			"but none resolved under %s (set ITERION_PI_TRUST_PROJECT=1 to load the workspace's own)",
			task.NodeID, task.Iteration, BackendPi, named, dir)
	}
	return args
}

func piMapProvider(name string) string {
	if mapped, ok := piProviderPrefixes[name]; ok {
		return mapped
	}
	return name
}

// piMapEffort maps iterion's reasoning_effort dial onto pi's --thinking
// flag. pi accepts off|minimal|low|medium|high|xhigh|max, a strict superset
// of iterion's levels, so every level passes through unchanged.
//
// `ultracode` is remapped to `xhigh` defensively: the runtime already does
// this on the wire, and pi has no subagent tool, so ultracode's
// orchestration half is inexpressible here regardless (see docs/ultracode.md).
func piMapEffort(effort string) []string {
	effort = strings.TrimSpace(strings.ToLower(effort))
	switch effort {
	case "":
		return nil
	case "ultracode":
		effort = "xhigh"
	}
	return []string{"--thinking", effort}
}

// piExtraArgsFor emits the per-task half of pi's argv.
//
// The logger is the reason this is not a plain `func(Task) []string`: the argv
// builder is the only place that knows a node ended up with zero skills, and a
// nil logger there turns that into exactly the silent degradation this backend
// keeps re-learning. Both transports bind it in their constructor.
func piExtraArgsFor(task Task, logger *iterlog.Logger) []string {
	var args []string

	if provider, modelID := piResolveModel(task.Model, task.ProviderHint); modelID != "" {
		if provider != "" {
			args = append(args, "--provider", provider)
		}
		args = append(args, "--model", modelID)
	}

	// Sessions. Only pinned when resuming or forking an existing one: pi
	// mints its own id for a fresh session and reports it in the stream
	// header, which parsePiOutput reads. Passing --session-id for a session
	// that does not exist yet works, but makes pi warn ("No project session
	// found with id …; creating a new session with that id") on *every*
	// first run — noise on the operator's console for no gain.
	switch {
	case task.SessionID != "" && task.ForkSession:
		args = append(args, "--fork", task.SessionID)
	case task.SessionID != "":
		args = append(args, "--session-id", task.SessionID)
	}
	args = append(args, "--session-dir", piSessionDir(task))

	// Skills. iterion mirrors bundle/plugin/library skills into the
	// workspace's .claude/skills/, which is not one of pi's own lookup
	// roots — but --skill takes an explicit path, and CLI-supplied skill
	// paths bypass the project-trust gate that --no-approve closes.
	args = append(args, piSkillArgs(task, logger)...)

	// The one tool gate pi expresses cleanly. iterion's `tools:` names do
	// not map onto pi's built-ins, and a partial mapping would silently
	// disable `bash` for any node listing iterion names — so the general
	// case stays advisory (ADR-065), and only readonly is enforced.
	if task.Readonly {
		args = append(args, "--tools", "read,grep,find,ls")
	}

	// Inside the sandbox the bundled model catalogue is authoritative:
	// a network egress policy would otherwise stall startup on pi's
	// catalogue refresh.
	if task.Sandbox != nil && strings.TrimSpace(os.Getenv("ITERION_PI_OFFLINE")) != "0" {
		args = append(args, "--offline")
	}

	if strings.TrimSpace(os.Getenv("ITERION_PI_TRUST_PROJECT")) == "1" {
		args = append(args, "--approve")
	}

	// pi walks up from the working directory and injects every AGENTS.md and
	// CLAUDE.md it finds into the system prompt. That is parity with
	// claude_code and on by default for the same reason — but it is not free,
	// and the bill is invisible until measured: on iterion's own tree (a
	// 103 KB CLAUDE.md) a trivial one-word prompt costs 26,933 input tokens
	// with context files against 448 without. Sixty times the input, before
	// the node does any work, on every call.
	//
	// So it stays on, and it gets an off switch.
	if strings.TrimSpace(os.Getenv("ITERION_PI_NO_CONTEXT_FILES")) == "1" {
		args = append(args, "--no-context-files")
	}

	return args
}

// piSessionDir picks where pi keeps this run's session files. Never the
// operator's ~/.pi/agent/sessions: run sessions must be per-run and
// GC-able, and concurrent nodes must not collide. Sandboxed runs need a
// path visible inside the container, which means workspace-relative.
func piSessionDir(task Task) string {
	root, _ := task.StateDir(BackendPi)
	return filepath.Join(root, "sessions")
}

// piPrepareStateRoot runs everything that must happen before pi writes anything
// for this node: refuse a leaf someone else could have planted, reap what an
// interrupted node stranded, and make an in-checkout root unstageable.
//
// It exists as a function rather than inline in Execute so the WIRING is
// testable — which root is guarded, and under which condition — not merely the
// helpers it calls.
func piPrepareStateRoot(task Task, logger *iterlog.Logger) error {
	root, inCheckout := task.StateDir(BackendPi)

	// Guard the leaf where someone OTHER than the operator could have planted
	// it: inside the target repository's checkout, and on the shared mount,
	// which is bind-mounted read-write at a predictable path and shared by every
	// run on the host. NOT on `<StoreDir>/pi` or `~/.iterion/pi`: those are the
	// operator's, and pointing them at another volume is a sensible answer to
	// transcript growth — refusing there aborted every pi node with a message
	// asserting a checkout that does not exist.
	//
	// Tested against the CHOSEN root, not against SharedStateDir as a proxy:
	// StateDir only takes the shared branch when the task is sandboxed, so a
	// hostless task carrying a stale SharedStateDir would otherwise guard the
	// operator's own store root.
	if shared := strings.TrimSpace(task.SharedStateDir); inCheckout || (shared != "" && lexicallyWithin(shared, root)) {
		if err := piGuardWriteRoot(root); err != nil {
			return err
		}
	}
	// The leaf Lstat above is not enough inside a checkout: a symlink at ANY
	// component redirects the whole root, and everything pi writes goes through
	// it — the extension bundle (which is the permission gate), the composed
	// system prompt, the transcripts, the credential. Only the credential path
	// walked the components until now; the other three rode the leaf check
	// alone, which follows a symlinked ancestor and finds a plain leaf inside
	// the attacker's directory.
	if inCheckout {
		if err := refuseSymlinkedPath(task.WorkDir, root); err != nil {
			return err
		}
	}
	// Reap what an interrupted node stranded, for EVERY pi node — not just the
	// codex ones that seed a credential. Session transcripts are written by all
	// of them, and on a root shared across runs nothing else reaps them.
	piSweepStaleSeeds(root)
	if inCheckout {
		// A v2 campaign agent runs `git add -A` before each in-stride commit and
		// finalizeWorktree fast-forwards the result onto the operator's branch,
		// so anything here could be staged into the target repo.
		piHideWorkspaceSessionDir(task, logger)
	}
	return nil
}

// piGuardWriteRoot refuses to write pi's workspace state through a symlink the
// TARGET repository supplied at `<WorkDir>/.iterion/pi`.
//
// .gitignore does not stop a TRACKED symlink from being checked out, and both
// os.MkdirAll and pi itself follow one — so a repo could redirect the extension
// bundle, the composed system prompt and its own session transcripts to a host
// path of its choosing, creating directories along the way. Off the sandbox
// that path is outside the workspace entirely.
//
// Only the LEAF is refused, not a symlinked `.iterion` above it. That asymmetry
// is deliberate: `pi/` is a directory iterion creates and names, so a symlink
// there is never the operator's doing — whereas `.iterion` IS theirs. Without
// `worktree: auto`, WorkDir is the operator's own repo root and `.iterion` is
// the conventional store dir, which they may legitimately have pointed at
// another volume; refusing that would fail every pi node on a working setup to
// close a narrower hole.
//
// The residue is stated rather than hidden: a repo that commits `.iterion`
// ITSELF as a symlink is not caught here, because at that point it is
// impersonating the operator's own store convention and the two are
// indistinguishable from this side.
func piGuardWriteRoot(root string) error {
	if root == "" {
		return nil
	}
	leaf := root
	info, err := os.Lstat(leaf)
	if err != nil {
		return nil // absent, or unreadable for a reason MkdirAll will report
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("pi backend: refusing to run: %s is a symlink, and the workspace is a "+
			"checkout of the target repository — pi's extension, system prompt and session "+
			"transcripts would be written wherever it points", leaf)
	}
	return nil
}

// piHideWorkspaceSessionDir makes a workspace-relative session dir invisible
// to the target repo's git, by dropping a self-ignoring .gitignore the way
// devbox does for its generated profile.
//
// Without it, pi's transcripts are untracked files inside the worktree, so
// they make workdirIsClean false and ride finalizeWorktree's `git add -A` into
// a wip-bank commit — meaning a sandboxed pi run lands a commit full of
// session transcripts in a repo that changed no code. It also writes iterion's
// own `.iterion/` into someone else's tree, which the repo-agnostic rule
// forbids. A no-op when the path is outside the workspace (the StoreDir case).
func piHideWorkspaceSessionDir(task Task, logger *iterlog.Logger) {
	// Guard whenever there is a workspace at all, not only when the SESSION
	// dir happens to land under it. piext.Materialise and writeSystemPromptFile
	// both write into <WorkDir>/.iterion/pi/ unconditionally, and on the
	// default non-sandboxed path piSessionDir resolves to StoreDir — so keying
	// on the session dir left the embedded extension bundle and the full
	// composed system prompt unguarded in the target repo, which is exactly
	// what this function exists to prevent.
	if task.WorkDir == "" {
		return
	}
	root := filepath.Join(task.WorkDir, ".iterion")
	if err := os.MkdirAll(root, 0o755); err != nil {
		if logger != nil {
			logger.Warn("pi: cannot create %s: %v — session files may ride a `git add -A`", root, err)
		}
		return
	}
	// `*` also ignores this file, so nothing under .iterion/ is ever staged.
	guard := filepath.Join(root, ".gitignore")
	// Lstat, never a FOLLOWING Stat. A repo can ship `.iterion/.gitignore` as a
	// tracked symlink, and following it fails both ways: a DANGLING link makes
	// os.WriteFile create an attacker-chosen host file, and a link to any
	// existing path makes the Stat below succeed so this returns as if the
	// workspace were guarded — silently leaving pi's transcripts and composed
	// system prompt stageable by a campaign agent's `git add -A`.
	//
	// The write policy deliberately differs from piWriteIgnoreGuard's: this path
	// can be the OPERATOR's own store dir, where appending `*` would re-ignore
	// everything their rules had negated (last match wins). Their file is left
	// exactly as it is.
	if err := refuseNonRegular(guard); err != nil {
		if logger != nil {
			logger.Warn("pi: %v — session files may ride a `git add -A`", err)
		}
		return
	}
	if _, err := os.Lstat(guard); err == nil {
		return // an operator's own guard: never overwrite
	}
	if err := os.WriteFile(guard, []byte("*\n"), 0o644); err != nil && logger != nil {
		logger.Warn("pi: cannot write %s: %v — session files may ride a `git add -A`", guard, err)
	}
}

// piCredentialEnvNames is every environment variable pi resolves a provider
// credential or endpoint from (packages/ai/src/env-api-keys.ts, plus the
// Anthropic bearer/base-URL pair that routes the z.ai facade).
//
// It exists for the sandbox: inside a container the ONLY environment is the
// one iterion passes through ExecOpts, so a host credential is invisible
// unless it is forwarded by name. A blanket `os.Environ()` forward would push
// every unrelated host secret into the container, so this is an explicit,
// auditable allowlist instead.
var piCredentialEnvNames = []string{
	"AI_GATEWAY_API_KEY",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"ANT_LING_API_KEY",
	"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
	"CEREBRAS_API_KEY", "CLOUDFLARE_API_KEY", "COPILOT_GITHUB_TOKEN",
	"DEEPSEEK_API_KEY", "FIREWORKS_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_CLOUD_API_KEY", "GROQ_API_KEY",
	"HF_TOKEN", "KIMI_API_KEY",
	"MINIMAX_API_KEY", "MINIMAX_CN_API_KEY", "MISTRAL_API_KEY", "MOONSHOT_API_KEY",
	"NVIDIA_API_KEY",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENCODE_API_KEY", "OPENROUTER_API_KEY",
	"QWEN_TOKEN_PLAN_API_KEY", "QWEN_TOKEN_PLAN_CN_API_KEY",
	"RADIUS_API_KEY", "TOGETHER_API_KEY",
	"XAI_API_KEY", "XIAOMI_API_KEY",
	"XIAOMI_TOKEN_PLAN_AMS_API_KEY", "XIAOMI_TOKEN_PLAN_CN_API_KEY", "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
	"ZAI_API_KEY", "ZAI_CODING_CN_API_KEY",
}

// piSandboxEnv is the environment a sandboxed pi receives: the credential
// allowlist above as inherited from the host, then the per-run overrides, then
// the run's own provisioning (devbox PATH) last so it wins on a clash.
//
// Without this a sandboxed pi node fails with "No API key found for <provider>"
// even though the host has the key — the failure this function exists for.
func piSandboxEnv(ctx context.Context, task Task) map[string]string {
	env := map[string]string{}
	for _, name := range piCredentialEnvNames {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env[name] = v
		}
	}
	for k, v := range piResolveEnv(ctx) {
		env[k] = v
	}
	for _, kv := range task.ExtraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// piEnvKeys maps a pi provider id onto the API-key environment variable pi
// reads for it. Only providers iterion can supply a BYOK credential for
// appear; everything else pi resolves from its own credential store.
var piEnvKeys = map[secrets.Provider]string{
	secrets.ProviderAnthropic:  "ANTHROPIC_API_KEY",
	secrets.ProviderOpenAI:     "OPENAI_API_KEY",
	secrets.ProviderXAI:        "XAI_API_KEY",
	secrets.ProviderZAI:        "ZAI_API_KEY",
	secrets.ProviderOpenRouter: "OPENROUTER_API_KEY",
}

// piResolveEnv layers per-run credential overrides onto the inherited
// environment.
//
// A subscription OAuth token inherited from the environment is deliberately
// left in place: Anthropic accepts it from a third-party app and bills it
// against extra usage (see noticeSubscriptionOAuth). Stripping it used to be
// how the refusal was enforced process-wide; with the refusal gone the strip
// would only break a working credential path.
func piResolveEnv(ctx context.Context) map[string]string {
	env := map[string]string{}

	// Under the opt-out the strip comes back, and it has to: locally the
	// token reaches pi through the inherited environment, so the ctx-based
	// check in noticeSubscriptionOAuth has no per-run credentials to refuse.
	// An empty value is an unset on both the host and the sandbox path.
	if secrets.ForbidSubscriptionOAuth() {
		env["ANTHROPIC_OAUTH_TOKEN"] = ""
		env["CLAUDE_CODE_OAUTH_TOKEN"] = ""
		env["CLAUDE_CONFIG_DIR"] = ""
		// ANTHROPIC_AUTH_TOKEN is the variable this repo documents as the
		// Anthropic subscription bearer, and piCredentialEnvNames forwards it
		// into the sandbox — so omitting it left the opt-out a no-op on both
		// the host and the sandboxed path, which is worse than no opt-out at
		// all because the operator believes it holds. It is cleared only when
		// it actually carries a subscription token: the same variable also
		// carries the z.ai facade key and gateway bearers, which this switch
		// has no business revoking.
		if secrets.IsAnthropicSubscriptionToken(os.Getenv("ANTHROPIC_AUTH_TOKEN")) {
			env["ANTHROPIC_AUTH_TOKEN"] = ""
		}
	}

	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		for provider, envKey := range piEnvKeys {
			if key := creds.APIKey(provider); key != "" {
				env[envKey] = key
			}
		}
	}

	// Pinning pi's agent dir hides the operator's own auth.json — the OAuth
	// credential breadth that motivates this backend — so it is opt-in, for
	// operators who want a fully reproducible pi configuration (which is
	// also the only print-mode lever to disable pi's nested auto-retry).
	if dir := strings.TrimSpace(os.Getenv("ITERION_PI_AGENT_DIR")); dir != "" {
		env["PI_CODING_AGENT_DIR"] = dir
	}

	return env
}

// parsePiOutput extracts the assistant's answer and accounting from a
// `--mode json` stream.
//
// The failure path is the subtle half: pi's json mode exits 0 even when the
// turn failed (only its text mode maps an error to exit 1), so the exit code
// carries no signal and the stop reason is the only truth. Errors are
// re-typed with the shared, backend-agnostic classifiers so the executor's
// retry policy treats a pi rate limit exactly like a claude_code one.
func parsePiOutput(stdout string) CLIAgentParse {
	stream := pisdk.DecodeStreamString(stdout)

	var parse CLIAgentParse
	if stream.Header != nil {
		parse.SessionID = stream.Header.ID
	}

	// pi's own retries are real billed calls whose transcripts are discarded,
	// so they are absent from the accounting below. Surface them rather than
	// let a node be slow and under-costed with no explanation.
	if retries := stream.AutoRetries(); len(retries) > 0 {
		last := retries[len(retries)-1]
		parse.Notices = append(parse.Notices, fmt.Sprintf(
			"pi retried upstream %d time(s) internally (last: attempt %d/%d after %dms — %q). "+
				"Those attempts were billed but are absent from this node's token count and cost, "+
				"and iterion's retry classifier never saw them. Pin ITERION_PI_AGENT_DIR to an "+
				"agent dir with retry.enabled=false to let iterion own retry policy.",
			len(retries), last.Attempt, last.MaxAttempts, last.DelayMs, last.ErrorMessage))
	}

	messages := stream.AssistantMessages()
	if len(messages) == 0 {
		// Nothing recognisable — hand back the raw stream so the shared
		// schema-aware fallback can still try to recover a JSON object.
		parse.Text = stdout
		return parse
	}

	for _, m := range messages {
		if m.Usage == nil {
			continue
		}
		parse.InputTokens += m.Usage.Input
		parse.OutputTokens += m.Usage.Output
		parse.ThinkingTokens += m.Usage.ReasoningTokens()
		parse.CostUSD += m.Usage.Cost.Total
		if ctxTokens := m.Usage.ContextTokens(); ctxTokens > parse.PeakInputTokens {
			parse.PeakInputTokens = ctxTokens
		}
	}

	last := messages[len(messages)-1]
	parse.Text = last.Text()
	parse.EffectiveModel = last.EffectiveModel()
	parse.Err = piClassifyFailure(last)

	return parse
}

// piClassifyFailure re-types a failed turn onto iterion's error taxonomy, so
// the executor retries, falls back, or fails fast the same way it would for
// any other backend. Returns nil for a turn that ended normally.
func piClassifyFailure(m pisdk.Message) error {
	if !m.StopReason.Failed() {
		return nil
	}
	if m.StopReason == pisdk.StopAborted {
		return &ErrTransient{
			Provider: BackendPi,
			Reason:   "aborted",
			Detail:   m.ErrorMessage,
		}
	}

	msg := m.ErrorMessage
	if msg == "" {
		msg = "pi reported a failed turn with no error message"
	}

	// The upstream HTTP status is the precise signal, and pi reports it in
	// the message's diagnostics. Prefer it over any text matching.
	switch status := m.HTTPStatus(); {
	case status == 429:
		return piRateLimited(msg)
	case status == 408, status == 409, status >= 500:
		return &ErrTransient{Provider: BackendPi, Reason: fmt.Sprintf("upstream %d", status), Detail: msg}
	case status == 401, status == 403, status == 402:
		// Deterministic: a credential or billing problem. Retrying burns
		// attempts against a failure that cannot resolve itself.
		return fmt.Errorf("pi: auth/billing rejected (%d): %s", status, msg)
	case status >= 400:
		return fmt.Errorf("pi: upstream %d: %s", status, msg)
	}

	// Anthropic's "extra usage" condition, seen live: a subscription OAuth
	// token driving a third-party app is ACCEPTED, but billed against a
	// separate extra-usage balance rather than the plan's own limits. When
	// that balance is empty the API answers 400 invalid_request_error with a
	// message that says nothing about credentials — so without this the
	// operator gets an opaque 400 and reasonably concludes the token is bad.
	// Deterministic by nature: it needs a human to top the balance up.
	if piIsExtraUsageExhausted(msg) {
		return fmt.Errorf("pi: Anthropic subscription extra-usage balance is empty — "+
			"third-party apps bill against extra usage, not your plan limits. "+
			"Top it up at claude.ai/settings/usage, use a metered ANTHROPIC_API_KEY, "+
			"or run this node on backend: \"claude_code\". Upstream: %s", msg)
	}

	// No status reported — fall back to the message text.
	//
	// Deliberately NOT isRateLimitMessage(): that detector is tuned for
	// claude_code, where the candidate is untrusted assistant PROSE, so it
	// is kept narrow (its own comment notes that "rate_limit_error" was
	// dropped because security-audit agents write about rate limits). Here
	// the candidate is `errorMessage`, a structured field only pi's runtime
	// writes, so the broader provider-shaped forms are safe — and needed:
	// a plain `rate_limit_error: 429 too many requests` is invisible to the
	// narrow list, which would misclassify a throttle as a permanent
	// failure and skip both retry and provider fallback.
	if piMatchesRateLimitText(msg) {
		return piRateLimited(msg)
	}
	if MatchesNetworkSignature(msg) || isTransientAPIErrorResult(msg) {
		return &ErrTransient{Provider: BackendPi, Reason: "upstream", Detail: msg}
	}
	// Everything else is deterministic.
	return fmt.Errorf("pi: %s", msg)
}

// piIsExtraUsageExhausted matches Anthropic's response when a subscription
// OAuth token is used by a third-party app and the extra-usage balance is
// empty. Matched on the distinctive phrasing rather than the status, because
// the status is a generic 400.
func piIsExtraUsageExhausted(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "extra usage") &&
		(strings.Contains(lower, "third-party") || strings.Contains(lower, "plan limits"))
}

// piRateLimitSignals are provider-shaped rate-limit forms. Safe to keep
// broad because they are matched against structured error metadata, never
// against model-authored text (see piClassifyFailure).
var piRateLimitSignals = []string{
	"rate_limit", "rate limit", "too many requests", "429",
	"quota", "usage limit", "overloaded", "capacity",
}

func piMatchesRateLimitText(msg string) bool {
	if isRateLimitMessage(msg) {
		return true
	}
	lower := strings.ToLower(msg)
	for _, sig := range piRateLimitSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// piRateLimited builds the rate-limit error, reusing the shared classifier to
// split a subscription-window exhaustion (waiting is the only cure) from a
// plain throttle worth retrying soon, and to recover any reset instant.
func piRateLimited(msg string) error {
	kind, resetAt := classifyRateLimit(msg, time.Now())
	return &ErrRateLimited{
		Provider: BackendPi,
		Kind:     kind,
		ResetAt:  resetAt,
		Detail:   msg,
	}
}
