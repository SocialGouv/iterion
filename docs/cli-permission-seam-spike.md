# Spike — the permission seam on the `grok` and `kimi` CLI backends

**Issue:** [#476](https://github.com/SocialGouv/iterion/issues/476) — *backends: give
grok and kimi the permission seam pi already has (C176 locks CLI forfaits out of
every gated bot)*.

**Status:** spike complete; the resulting engine integration is documented in
[`permissions.md`](permissions.md). This note preserves the evidence for step 1
of the issue ("does either CLI expose an interception surface?") so the wire
contracts and registration constraints do not have to be re-derived.

**Measured on** 2026-08-22, host Linux, `grok 1.0.5 (5115b46bc9) [stable]`,
`kimi-code 0.37.2`.

---

## TL;DR

**Both CLIs expose a real `PreToolUse` hook that can deny a tool call**, in the
claude_code shape (event JSON on stdin, a decision on stdout, fail-open on
error). Neither needs anything invented, so the `pi` route — *build the missing
seam* — is not required; the `claude_code` route — *a hook evaluating the
resolved `permission.Policy`* — applies to both.

The real problem is **not** interception, it is **registration**: neither CLI
lets a caller attach a hook to a single headless invocation. Both discover hooks
from their own **home directory**. That is solvable without touching the
operator's home, and the solution was verified end-to-end on **both**:

> Point the CLI at a **per-invocation shadow home** — a scratch directory whose
> entries are symlinks to the real home, plus the injected hook registration —
> via the CLI's own home environment variable (`GROK_HOME` / `KIMI_CODE_HOME`).

| | interception surface | can deny? | registration for a one-shot run | verified |
|---|---|---|---|---|
| **grok** | `PreToolUse` hook (drop-in `hooks/*.json`) | yes | shadow `GROK_HOME` (hook discovered, scope `user` = always trusted) | **end-to-end ✅** — a real model's `run_terminal_command` was blocked under `--always-approve` |
| **kimi** | `PreToolUse` hook (`[[hooks]]` in `config.toml`) | yes | shadow `KIMI_CODE_HOME` | **end-to-end ✅** — a real model's `Bash` call was blocked |

---

## 1. grok

### 1.1 The surface

Grok ships a claude_code-compatible hook system, documented on the host at
`~/.grok/docs/user-guide/10-hooks.md` and `22-permissions-and-safety.md`.

- Events include `PreToolUse`, and the docs table marks it **"Blocking? Yes: can
  deny"**.
- Authorization order is explicit: *"1. **`PreToolUse` hooks**. A hook can deny a
  tool call before any other check."*
- The always-approve caveat is answered in the docs, exactly like claude_code's:
  *"Always-approve short-circuits this pipeline **after step 2**: `deny` rules,
  **hooks**, and `ask` rules that match a shell command's segments still apply"*.
  So iterion's own `--permission-mode bypassPermissions --always-approve`
  ([pkg/backend/delegate/grok.go](../pkg/backend/delegate/grok.go)) does **not**
  have to be removed.

Wire contract (`PreToolUse`), camelCase on stdin:

```json
{
  "hookEventName": "pre_tool_use",
  "sessionId": "abc-123",
  "cwd": "/path/to/project",
  "workspaceRoot": "/path/to/project",
  "permissionMode": "bypassPermissions",
  "toolName": "run_terminal_command",
  "toolInput": { "command": "npm test" },
  "toolUseId": "…",
  "timestamp": "…"
}
```

Decision on stdout: `{"decision":"deny","reason":"…"}` (or exit code `2` with the
reason on stderr). `{"decision":"allow"}` / empty output lets the call proceed.
Everything else **fails open** — only an explicit `deny` blocks.

Native tool names seen in the docs (what `toolName` actually carries):
`run_terminal_command`, `read_file`, `write_file`, `search_replace`, `list_dir`,
`grep`, `web_search`, `web_fetch`, `todo_write`, `spawn_subagent`, `use_tool`
(the MCP dispatcher — a matcher sees the qualified `server__tool`).

### 1.2 Registration — what does NOT work

- **`--plugin-dir`** ("process only, always trusted — hooks and MCP servers
  activate without a prompt") exists **only on `grok agent … stdio`**, not on the
  headless `grok -p` iterion drives:
  ```
  $ grok -p "hi" --plugin-dir /tmp/x
  error: unexpected argument '--plugin-dir' found
  ```
- **`GROK_CONFIG` / `GROK_CONFIG_PATH`** (config overlays) are deliberately
  fenced: *"confined, fail-closed, to an allowlist of soft settings … the overlay
  **cannot spawn commands**, set auth policy, … or **add a discovery source**"*
  and *"it is **not** a permission-escalation path"*. Hooks cannot be injected
  this way, by design.
- **Project scope** `<project>/.grok/hooks/*.json` works but **requires folder
  trust**, granted with `--trust` (accepted by `grok -p`) or `/hooks-trust`, and
  *"the decision is recorded in the unified folder-trust store
  (`~/.grok/trusted_folders.toml`)"*. That grant is **persistent and
  folder-wide**: it would also activate the *target repo's own* `.grok` hooks,
  MCP and LSP servers for every later session — the same hazard iterion already
  refuses for pi's `.pi/` extensions. Rejected.

### 1.3 Registration — what does work

`~/.grok/hooks/*.json` is a **drop-in directory**, scope "Global", trust
"Always". And `GROK_HOME` relocates the whole grok home. Combining the two gives
a per-invocation registration that never writes into the operator's home:

```sh
# shadow home: symlink every entry of the real ~/.grok, then add our own hooks/
mkdir -p "$SHADOW/hooks"
for e in ~/.grok/*; do ln -s "$e" "$SHADOW/$(basename "$e")"; done   # minus hooks/
cat > "$SHADOW/hooks/iterion-permission.json" <<'JSON'
{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"…","timeout":30}]}]}}
JSON
GROK_HOME="$SHADOW" grok inspect
```

Verified output:

```
  Hooks (1)
  └ command  user
```

Scope `user` = always trusted, no folder-trust prompt. Credentials also resolve
through the symlinked home.

### 1.4 End-to-end proof

Same shadow home, but the hook command is the real shipping one
(`iterion __permission-hook --backend grok --policy <path>`), the policy is
`{"mode":"deny","deny":["Bash"]}`, and the invocation carries the exact flags
grok.go always passes:

```sh
GROK_HOME=/tmp/grok-proof/home grok \
  -p "Use your run_terminal_command tool to run exactly: touch permission-hook-must-not-create . Then report whether it actually executed." \
  --output-format json --permission-mode bypassPermissions --always-approve
```

The model's own answer:

> It did **not** execute. `run_terminal_command` was blocked by the permission
> policy before the shell ran. The denial was: *Permission denied: the
> `run_terminal_command` tool is not authorized by this run's permission policy
> for `"touch permission-hook-must-not-create"` (deny rule: Bash).*

The sentinel file was **not** created — the filesystem, not the model's prose,
is the oracle. Note what this pins: the deny held **under
`--always-approve`**, confirming grok's documented ordering (hooks run at step 1,
always-approve short-circuits only what follows). The message is
`permission.DenyMessage`'s, so a single `Bash` rule reached grok's
`run_terminal_command` through `canonicalToolName` — cross-backend parity, not a
second rule dialect.

---

## 2. kimi

### 2.1 The surface

kimi-code has the same event vocabulary (`packages/agent-core-v2/src/agent/
externalHooks/types.ts` in the bundled `dist/main.mjs`): `PreToolUse`,
`PostToolUse`, `PermissionRequest`, `Stop`, … The tool executor wires it as a
veto:

```js
this._register(toolExecutor.onBeforeExecuteTool(async (event) => {
  const reason = await this.runPreToolUse(event);
  if (reason !== void 0) event.veto(denyToolExecution(reason));
}));
```

Wire contract, **snake_case** on stdin (measured, not inferred):

```json
{
  "hook_event_name": "PreToolUse",
  "session_id": "session_2ac7bae5-…",
  "cwd": "/tmp/kimi-spike",
  "client_type": "kimi_code_cli",
  "tool_name": "Bash",
  "tool_input": { "command": "echo hello-from-kimi" },
  "tool_call_id": "tool_243WqXGPdq0PHLVzxVbZMPme"
}
```

Two ways to block (`resultFromExitCode` / `structuredOutput` in `dist/main.mjs`):

- exit code `2`, reason on **stderr**;
- exit `0` with
  `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"…"}}`.

Everything else — non-zero exits other than 2, timeouts, malformed output,
spawn failures — **fails open**, same posture as grok and claude_code.

kimi's native tool names are already TitleCase and claude-shaped: `Bash`,
`Read`, `Write`, `Edit`, `Glob`, `Grep`, `WebSearch`, `FetchURL`, `Skill`,
`AskUserQuestion`, `TaskList`/`TaskOutput`/`TaskStop`, `EnterPlanMode`/
`ExitPlanMode`, `ReadMediaFile`. Only `FetchURL` is missing from iterion's
`toolAliases` (→ `webfetch`).

### 2.2 Registration

Hooks come from **one** place: the `[[hooks]]` array inside
`$KIMI_CODE_HOME/config.toml` (default `~/.kimi-code/config.toml`), plus the
hooks of installed plugins under `$KIMI_CODE_HOME/plugins/`. There is no
drop-in directory and no per-invocation flag, so the shadow home is not an
optimisation here — it is the only way that does not mutate the operator's own
`config.toml`.

### 2.3 End-to-end proof

Shadow home = symlinks to every entry of `~/.kimi-code` **except** `config.toml`,
which is copied and appended with:

```toml
[[hooks]]
event = "PreToolUse"
command = "/tmp/kimi-shadow/deny.sh"
```

`deny.sh` echoes the structured deny. Then:

```sh
KIMI_CODE_HOME=/tmp/kimi-shadow/home kimi \
  -p "Use your Bash tool to run exactly: echo hello-from-kimi . Then tell me whether it succeeded." \
  --output-format stream-json
```

Real transcript:

```json
{"role":"assistant","content":"Running the command now.","tool_calls":[{"type":"function","id":"tool_243…","function":{"name":"Bash","arguments":"{\"command\":\"echo hello-from-kimi\"}"}}]}
{"role":"tool","tool_call_id":"tool_243…","content":"iterion spike: blocked by the permission gate"}
{"role":"assistant","content":"It did not succeed. The command was blocked by the permission gate before it could run, so no output was produced."}
```

The tool never ran, the deny reason reached the model verbatim, and the model
adapted — exactly the behaviour the gate sells on claude_code and claw.
Credentials resolved through the symlinked `credentials/` + `oauth/`
directories, unchanged.

---

## 3. A hole this spike surfaced, wider than the issue

C176 screens **fallback routes** only. A **primary** backend that cannot enforce
the gate is not screened at all — this compiles clean today:

```
agent runner:
  backend: "grok"
  model: "grok-4.5"
  …
workflow main:
  permission: deny
  deny: ["Bash"]
```

```
$ iterion validate /tmp/gate-primary.bot
  result: OK
```

So a bot could already declare `permission: deny` on grok/kimi/codex as its
primary backend and run **completely ungated**, with no diagnostic. That is
strictly worse than the C176 refusal the issue complains about: the refusal is
loud, this is silent.

**Closed.** `validateFallbacks` now screens the node's effective primary
backend with the same `ungatedCrossingReason` predicate it applies to fallback
routes, so grok/kimi enforce the gate and codex is refused rather than silently
ungated.

---

## 4. What is still unproven

- **`ask` mode.** Both CLIs' hooks can deny; neither offers claude_code's
  "cancel the stream and pause the run" affordance from a *separate hook
  process*. Shipped decision: both backends are admitted for `deny` only, and a
  policy that could ask — `permission: ask`, or `deny` carrying explicit `ask:`
  rules — is refused at compile time (C176) and again before the CLI is
  launched. Silently downgrading an operator's `ask` to `deny` would be the same
  class of lie as running ungated.
- **Sandboxed runs.** Neither backend forwards credentials into a container
  today (`ResolveEnv` / `SandboxEnv` are nil on both protocols), so a shadow
  home built on the host is meaningless inside one. A gated node on a sandboxed
  grok/kimi run should refuse rather than degrade.

---

## 5. Reproducing

Everything above was produced with the CLIs already installed on the host
(`~/.grok/bin/grok`, `kimi` from `@moonshot-ai/kimi-code`). The two CLIs ship
their own documentation locally:

- grok: `~/.grok/docs/user-guide/{10-hooks,22-permissions-and-safety,09-plugins,05-configuration,14-headless-mode}.md`
- kimi: no shipped docs; the contract was read out of
  `$(npm root -g)/@moonshot-ai/kimi-code/dist/main.mjs`
  (search `runHook`, `resultFromExitCode`, `HOOK_EVENT_TYPES`, `resolveKimiHome`).
