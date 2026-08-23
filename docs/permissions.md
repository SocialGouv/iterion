# Tool-permission gate (anti-prompt-injection)

iterion's permission gate restores Claude Code's default **"ask before
acting"** posture to iterion workflows. One deterministic policy now drives
`claude_code`, the in-process `claw` loop, and pi's embedded RPC extension.

## Why

By default `claude_code`, `claw`, and pi nodes run effectively ungated
(`bypassPermissions` on the CLIs): any tool the model decides to call executes
unconditionally. That is convenient but it is also the posture a
prompt-injection or "hypnosis" attack relies on — a poisoned web page,
a malicious file, or a confused chain of reasoning can get the agent to
exfiltrate a secret, `curl` an attacker, `rm -rf` a tree, or `git push`
to a rogue remote, and nothing stops it.

The gate makes the operator's **allow-list the frame of what's
authorized**, evaluated by deterministic code **outside the model's
controllable surface** (`pkg/backend/permission`). Anything off-frame is
denied, or surfaced to a human — exactly like Claude Code's `canUseTool`
default. The model cannot talk its way past a rule, because the rules
are not part of its context.

This mirrors the official Anthropic model (Agent SDK *Configure
permissions* + *Handle approvals and user input*): tool calls are
evaluated **deny rules → ask rules → allow rules → mode default**, and
unmatched calls fall through to human approval.

## Modes

Set on the `workflow` block, per node, the CLI, or the environment.
**Opt-in: the default is `off`** — existing bots are unchanged.

| iterion `permission:` | Claude Code analog | Behavior |
| --- | --- | --- |
| `off` (default) | `bypassPermissions` | No gate (today's behavior). |
| `ask` | `default` | allow-rules auto-approve; **deny**-rules hard-block; everything else **pauses the run and surfaces the call to the human** (resumable). |
| `deny` | `dontAsk` | allow-rules approve; everything else is **hard-denied with no pause** — the policy boundary for headless / cloud / cron runs with no human attached. |

## Rule syntax

Rules use Claude Code's syntax — a bare tool name matches any use, or a
scoped `Tool(pattern)` matches an argument:

Rule lists use iterion's inline-array syntax (like `capabilities:`):

```
workflow main:
  permission: ask
  allow: ["Read(**)", "Edit(pkg/**)", "Bash(go test:*)", "Grep", "mcp__github__get_*"]
  ask:   ["Bash(git push:*)"]
  deny:  ["Bash(rm -rf:*)", "Read(.env*)", "WebFetch(domain:evil.example)"]
```

Where:

- `Read(**)` — read anything; `Edit(pkg/**)` — edit only under `pkg/`.
- `Bash(go test:*)` — any `go test …` command (`:*` = prefix match).
- `Grep` (bare) — any grep; `mcp__github__get_*` — any github MCP `get_` tool.
- `Bash(rm -rf:*)` in `deny:` — never `rm -rf`, even in `ask` mode.

A per-node override is the scalar mode only — `permission: deny` on an
`agent` or `judge` node (the gate evaluates *LLM-issued* tool calls; a
`tool` node's `permission:` is parsed but currently inert — see Status).

Matching semantics (`pkg/backend/permission`):

- **Bash** patterns match the `command`; `prefix:*` is a prefix match,
  a bare wildcard `*`/`**` is a greedy match, no wildcard is exact.
- **Read / Edit / Write / NotebookEdit** patterns match the file path;
  `pkg/**`, `*.go` etc. work as gitignore-style globs.
- **WebFetch** patterns match `domain:<host>`, `<host>`, or the full URL.
- **Tool-name globs**: `*` (any tool) and `mcp__<server>__*`.

**Cross-backend parity.** The same rule gates the matching tool on every
supported route: a single `Bash(...)` rule covers claude_code's `Bash`,
claw's `bash`/`shell`, pi's `bash`, Grok's `run_terminal_command`, and Kimi's
`Bash`; `Edit(...)` covers `Edit`/`edit_file`/`file_edit`/Grok's
`search_replace`; `Read(...)` covers `Read`/`read_file`; etc.
(see `canonicalToolName`).

**Infrastructure exemption.** iterion's own interaction/capability
plumbing — `ask_user`, the board / control / watch MCP families — is
never gated (or `ask` mode would pause on the very tool used to ask the
human).

## Precedence

Mode resolves with the same precedence as `compress:`:

```
CLI --permission  >  node permission:  >  workflow permission:  >  ITERION_PERMISSION  >  off
```

Rule lists are **additive**: the workflow `allow:`/`ask:`/`deny:` lists
plus any `--permission-allow`/`--permission-ask`/`--permission-deny`
run-level rules.

The studio Launch dialog captions the permission select with the
resolved mode and the level it came from ("effective: ask · from
workflow") — see [settings-precedence.md](settings-precedence.md).

## CLI

```bash
iterion run bot.bot --permission ask \
  --permission-allow 'Read(**)' --permission-allow 'Bash(go test:*)' \
  --permission-deny  'Bash(rm -rf:*)'

# Headless hard boundary (no human to pause for):
iterion run bot.bot --permission deny --permission-allow 'Read(**)'
```

Environment: `ITERION_PERMISSION=ask|deny|off`.

## How it works

The resolved `permission.Policy` is carried on `delegate.Task.Permission`
and evaluated by each gated backend before every tool runs:

- **claw** — `executeToolsDirect` (pkg/backend/model/generation.go)
  evaluates the policy before `gt.Execute`. Allow → execute; Deny → a
  synthetic `isError` tool_result the model adapts to; Ask → the loop
  aborts with `delegate.ErrAskUser` so the run pauses.
- **claude_code** — a broad PreToolUse hook (`wirePermissionHook` in
  claude_code.go) evaluates the policy. Under the always-on
  `bypassPermissions`, PreToolUse hooks still run and a `deny` decision
  still blocks the tool (Agent SDK order: hooks run first), so no
  `--permission-mode` change is needed. Ask reuses the `ask_user`
  capture-and-pause path.
- **pi (RPC mode)** — the embedded iterion extension intercepts tool calls and
  asks Go to evaluate `permission.evaluate` over the control channel. Ask
  unwinds the turn as the same `delegate.ErrAskUser` pause. Pi print mode has
  no control channel and refuses a permission-gated node rather than running
  it unguarded.
- **kimi (`deny` only)** — iterion creates a private shadow
  `KIMI_CODE_HOME` for each invocation, links the operator's credentials and
  config into it, and appends a `PreToolUse` hook. The hook subprocess rebuilds
  the policy and evaluates it with the same Go implementation. A
  deny is returned in kimi's native `hookSpecificOutput` shape. The real
  `~/.kimi-code` is never modified.
- **grok (`deny` only)** — the same shadow-home design uses `GROK_HOME` plus a
  global `hooks/iterion-permission.json`, and the deny is spelled in grok's
  native `{"decision":"deny","reason":…}` shape. It holds under the
  `--permission-mode bypassPermissions --always-approve` flags iterion always
  passes, because grok's authorization pipeline runs `PreToolUse` hooks *first*
  and always-approve only short-circuits the checks *after* them.

**The policy travels by value, and the shadow home lives outside the
workspace.** Both matter for the same reason: the hook subprocess is the gate's
entire authority on these backends, and the agent it gates runs as the same OS
user. So the serialised `PolicyConfig` is passed base64-encoded in the hook's
own argv — which both CLIs freeze when the session starts — instead of as a
file the hook would re-read on every tool call; and the shadow home is created
under the OS temp dir rather than `<workspace>/.iterion/<backend>`, which is
where a repo-scoped `Edit(**)` / `Write(**)` allow rule would reach. Without
those two properties, one allowed write of `{"mode":"off"}` would disarm the
gate for the rest of the node — an escalation the in-process claude_code and
claw gates cannot have, since their policy never leaves iterion's memory. A
policy the hook cannot decode fails **closed**.

**`sandbox: none` is required today, and C136 says so at compile time.** The
hook binary and the CLI home are host-side, so a sandboxed run cannot reach
them and the node is refused before the CLI starts. Since the shipped default
is `sandbox: auto`, a gated grok/kimi node with no `sandbox:` block would
otherwise compile clean and die mid-run — so the compiler warns (C136) rather
than letting the operator discover the coupling after launch. `--sandbox none`
and `ITERION_SANDBOX_DEFAULT=none` satisfy it too, which is why C136 warns
instead of rejecting. Lifting the restriction means carrying the shadow home
and the hook binary into the container; until then the refusal is the honest
answer.

**The hook binary must live outside the workspace too.** It is the third thing
the agent must not reach, and the weakest of the three: unlike the frozen argv,
it is re-executed on every tool call, and both CLIs fail **open** when a hook
fails to spawn — so corrupting the file is enough, no working replacement
needed. `proc.LocateIterionBinary` resolves `os.Executable()`'s directory
first, which in the repo-root shape (`./iterion run …` after `task build`) is
inside the gated workspace, so iterion refuses that configuration and points at
`ITERION_BIN` on a stable install path.

**The hook binary must live outside the workspace.** It is the third thing the
gated agent must not be able to reach, and the sharpest: unlike the frozen argv
it is re-executed on *every* tool call, and both CLIs fail open on a spawn
failure — so corrupting the file, not replacing it with a working one, is
enough. `proc.LocateIterionBinary` resolves next to `os.Executable()` first,
which in the repo-root shape (`./iterion run …`, or `task studio:dev` pinning
`ITERION_BIN` to a freshly built `./iterion`) is inside the very workspace
being gated. iterion refuses that configuration and points at `ITERION_BIN` on
a stable install path.

**Windows** needs Developer Mode (or an elevated process): the shadow home
links the operator's CLI home with symlinks, which stock Windows denies to
unprivileged processes. The failure names the limitation. Copying the entries
is deliberately not the fallback — that tree is a credential store.

Neither external hook is admitted on a declaration: each earned its entry in
`gateEnforcingModes` with a live denial where a filesystem sentinel — not model
prose — is the oracle (`e2e/live_feat_permission_{kimi,grok}_test.go`). Delete
those tests and the entry becomes the lie C176 exists to prevent.

Every hook honours the **same** `permission.Policy`; protocol adapters only
decode the native event and spell the native verdict.

## Status / limitations

- **`off` and `deny` modes, and explicit `allow:`/`deny:` rules in any
  mode, are fully deterministic** and need no human — the complete
  anti-injection boundary for headless and cloud runs.
- **`ask` mode** pauses the run (`paused_waiting_human`) and surfaces the
  off-policy call to the operator, so nothing off-policy ever executes
  silently. To resolve the pause, the operator just **answers the
  approval question** with `allow`, `allow always`, or `deny` — on any of the
  three gated backends. The pause carries a structured marker (tool + input
  + rule); the runtime maps the answer to a grant rule
  (`allow` = argument-scoped, `allow always` = whole-tool) and feeds it
  back into the resolved policy, so the agent's re-issued call passes the
  gate and executes after the generic `[PERMISSION GRANTED]` resume reminder.
  `deny` refuses the call and the agent adapts. The `--permission-allow`
  flags on `resume` remain available for scripted/headless approval.
- The marker also lets a `permission: ask` node pause **without** needing
  `interaction:` set — the gate is its own reason to pause.
- **Backend scope:** `claw`, `claude_code`, and pi RPC support `ask` and
  `deny`. Kimi and Grok support `deny` only; `ask`, a `deny` policy containing
  explicit `ask:` rules, and sandboxed guarded runs on either are refused before
  the CLI is launched. Codex has no permission seam and refuses any enabled
  gate.
- **Primary routes are screened too.** C176 applies to the node's effective
  primary backend as well as authored and run-level fallbacks, so an unsupported
  backend can no longer run a declared gate silently.
- **Node scope:** the gate evaluates the **tool calls an agent/judge LLM
  makes**. A `tool` node (a direct, deterministic shell command, no LLM)
  is the action itself and is governed by the **Verified Action** quad
  (`goal`/`postcondition`/`policy`/`recovery`), not this gate — so a
  `permission:` mode on a `tool` node is currently reserved (parsed,
  not yet enforced).

## See also

- `pkg/backend/permission/` — the matcher + Policy (single source of truth)
- `docs/plugins.md` — the sibling opt-in `compress:` field this mirrors
- Diagnostics: **C110** (invalid permission mode), **C111** (rules
  declared but gate off), **C112** (tool-node `permission:` — parsed but
  not enforced).
