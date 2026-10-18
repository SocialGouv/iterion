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
permissions* + *Handle approvals and user input*): workflow rules are
evaluated **deny rules → ask rules → allow rules → mode default**, and
unmatched calls fall through to human approval. An engine-owned grant created
by an explicit operator approval is evaluated after a deny rule and before an
ask rule, so the approved retry proceeds without changing the workflow DSL.

One shape inside that order is deliberately not a pause: an `ask:` rule
naming just the tool (`Bash`) is beaten by an `allow:` rule naming one exact
call (`Bash(git diff)`), so one audited invocation stays automatic under a
tool-wide pause. A scoped or wildcard `ask:` is never beaten, and a prefix or
bare `allow:` never beats one — `Policy.Evaluate` step 4, and the mode does
not change it.

## Modes

Set on the `workflow` block, per node, the CLI, or the environment.
**Opt-in: the default is `off`** — existing bots are unchanged.

| iterion `permission:` | Claude Code analog | Behavior |
| --- | --- | --- |
| `off` (default) | `bypassPermissions` | No gate (today's behavior). |
| `ask` | `default` | allow-rules auto-approve; **deny**-rules hard-block; everything else **pauses the run and surfaces the call to the human** (resumable). |
| `deny` | `dontAsk` | allow-rules approve; **ask**-rules still pause (the lists are read the same way in both armed modes); everything else is **hard-denied with no pause** — the policy boundary for headless / cloud / cron runs with no human attached. |

## Rule syntax

Rules use Claude Code's syntax — a bare tool name matches any use, or a
scoped `Tool(pattern)` matches an argument:

Rule lists use iterion's inline-array syntax (like `capabilities:`):

```iter fragment
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

`agent` and `judge` nodes take the same three lists, and the mode
(`permission: deny`) as an override. A `tool` node takes the mode alone,
and it is inert there: the gate evaluates *LLM-issued* tool calls (see
Status).

### Per-node rule lists — a node list REPLACES the workflow's

One list cannot serve a workflow whose nodes need different bounds. So a
node's `allow:` / `ask:` / `deny:` **replaces** the workflow list of the
**same kind**, independently per kind; the kinds it does not declare are
inherited. Never a union: a union can only widen, and the shape that needs
expressing is a *narrowing* — a converge node that must not hold the shell
its reviewers cannot work without.

```iter fragment
agent reviewer:
  model: "anthropic/claude-opus-5"
  system: reviewer_system
  user: reviewer_user

agent converge:
  model: "anthropic/claude-opus-5"
  system: converge_system
  user: converge_user
  deny: ["Bash"]

workflow review:
  entry: reviewer
  permission: deny
  allow: ["Read(**)", "Bash(git diff:*)", "Bash(git log:*)"]
  deny: ["Read(.env*)"]
  reviewer -> converge
  converge -> done
```

`reviewer` keeps both workflow lists. `converge` keeps the `allow:` — it
declared none — but its own `deny:` **replaces** the workflow's, so
`converge` loses the shell *and* loses the `.env` guard: a node that
declares a kind must restate anything from the workflow's list of that kind
it still wants. That is what replacement costs, and it is the price of being
able to narrow at all.

Three properties are worth stating because each was a choice:

- **Declared means non-empty.** `deny: []` is the same as no `deny:`, not
  "clear the workflow's". The AST's JSON seam carries these lists with
  `omitempty`, which erases nil-from-empty, so a rule built on that
  distinction would hold in a `.bot` and break through the studio.
- **Replacement is per kind, not per node.** A node declaring only `deny:`
  still inherits the workflow's `allow:` and `ask:`.
- **The run-level lists stay additive.** `--permission-allow` / `-ask` /
  `-deny` append to whichever list won — node or workflow. They are the
  operator's live escape hatch over a `.bot` they may not own, and a
  replacement would take it away.

The resolution happens once, in `resolvePermissionPolicy`
(`pkg/backend/model/executor_build_task.go`) — the only place in the engine
that builds a `Policy` from DSL and run inputs, which is why every backend's
gate sees the same rules. The route screens (C136, C176) read the node's
effective ask list through the same helper (`ir.EffectiveAskRules`), so the
ask rules they admit a backend against are the ones the runtime will gate on.

Two bounds on that sentence, both deliberate. The screens judge the **DSL**
lists only: `--permission-ask` is appended after them and is screened at
dispatch instead, where an ask-incapable backend refuses the node. And C111
answers a different question — "is this list declared at all" — which it
shares with the resolver through one predicate rather than by re-deriving it.

### What a `tools:` list costs, measured

`tools:` and the policy are **orthogonal**, and authoring one as if it
were the other is the mistake this section exists to prevent. `tools:`
bounds what *exists*; the policy bounds what may *run*; nothing joins them
at run time.

On `claude_code` a `tools:` list is **not** inert. The hard-restrict flag
`--tools` is deliberately unused (see `pkg/backend/toolcatalog`), but iterion
turns the list into `claudesdk.WithDisallowedTools`, which the SDK spells as
`--disallowedTools`: every tool on Claude Code's **closed native roster**
that the list does not name is removed. The roster is fourteen names —
`Bash Read Glob Grep Write Edit MultiEdit NotebookEdit Task WebFetch
WebSearch ToolSearch TodoWrite Skill` (`claudeNativeTools`). Measured on a
reviewer node, counting only what the list itself contributes:

```
no tools: list                          the list adds nothing
tools: []                               the list adds all fourteen
tools: [bash, read_file, glob, grep]    the list adds
  [Write Edit MultiEdit NotebookEdit Task WebFetch WebSearch ToolSearch
   TodoWrite Skill]
```

**`tools: []` is a declaration, not an absence.** An empty list is the author
saying *this node has no tools*; a node with no `tools:` line leaves the
surface undeclared, which is what "the list adds nothing" above means. The two
used to be byte-identical — the parser returned nil for `[]`, the AST's JSON
seam dropped it and `iterion fmt` deleted the line — so a node that asked for
no tools kept the whole native roster (#1615). They are told apart from the
parser down, through one predicate (`toolcatalog.ToolsDeclared`) and one
explicit fact on the task (`delegate.Task.ToolsDeclared`).

Three backends receive the list and narrow on it (`toolcatalog.ReceivesToolList`):
claw resolves zero tool definitions from it, claude_code disallows all
fourteen names of `claudeNativeTools`, codex drops to the `read-only` sandbox.
pi, kimi and grok are driven through the CLI-agent seam, which never passes
the list to the agent: there the bound is dropped and **C270** says so at
compile time. Because an older engine reads `tools: []` as an absent list —
the opposite bound — a bundle that spells it asks for
`requires.iterion >= 3.190.0`, and an older runner refuses the bundle rather
than inverting it.

**What `tools: []` is not.** It is a narrowing, not a proof that the node
holds nothing, and nothing in the engine treats it as one — the parallel-branch
scheduler in particular still reads such a node exactly as it reads an
undeclared one. Two measured reasons. `claudeNativeTools` is a hardcoded
enumeration of a roster iterion does not own, and the same package names tools
outside it — `orchestrationTools`' `Agent`, `TaskOutput` and `Monitor` — which
the roster therefore does not name. Whether one survives is the CLI's call:
the pinned CLI resolves the roster's legacy `Task` to `Agent`, so a declared
list withholds the subagent tool there, while `Monitor` survives. MCP tools
are not on that roster either, so a node's `mcp_servers:` stay reachable. (MCP tools do
not reach the structured-output spawn either, but by a different mechanism: it
passes no `--mcp-config`, and `--strict-mcp-config` stops the CLI falling back
to the host scopes — set `ITERION_CLAUDE_CODE_STRICT_MCP=0` and they come
back.) (`Workflow`
is the exception that proves the shape: it is *not* on the roster and is
withheld separately, from every spawn, ultracode included, whatever the list
says — with the schedulers `ScheduleWakeup`, `CronCreate`/`CronDelete`/
`CronList` and `RemoteTrigger`, which no one-shot session can use.)
The one spawn where none of them survives is the **structured-output pass of a
GATED node**: it cannot carry the permission hook, so it withholds the roster,
the whole orchestration surface this package enumerates (`Agent`, `Task`,
`TaskOutput`, `Monitor`, `Workflow` — ultracode or not, because nothing there
can run the policy the author wrote), and the names the live CLI was measured
still registering afterwards: `EnterWorktree`/`ExitWorktree` (they move the
worktree the session acts in), `CronCreate`/`CronDelete`/`CronList` and
`ScheduleWakeup` (they schedule future work), `SendMessage`, `RemoteTrigger`,
`BashOutput`, `KillShell`, `TaskStop`. `StructuredOutput` is the one tool that
pass needs and is deliberately not withheld. That withholding is the enforced
half and it is **incomplete** — it names a roster iterion does not own, so the
tools outside those lists survive it (#1651). The sentence that pass also
carries — *"Do not call any tool other than StructuredOutput; just return the
JSON."* — covers those names whatever their spelling, but only while the model
cooperates, which under a prompt-injection threat is the weaker control: it is
defence in depth beside the flag, never in place of it. On claw,
`assembleEffectiveTools` adds `ask_user` when `interaction:` is set, then
`todo_write`, then `read_file`/`write_file`/`glob` under `auto_memory:` — so a
node that declared no tools can end up holding a file writer, which **C270**
warns about at compile time. A bound that must hold is a `deny:` rule,
evaluated by the gate — and when the bound is about what a tool DOES rather
than which tool it is, the rule names the class: `deny: ["class:shell"]`
bounds every registered built-in that executes model-supplied code, present
and future ([tool classes](#tool-classes)).

(Every node also carries `Workflow` and the schedulers on that flag whatever
its `tools:` says — unrelated to the list. And a tool *outside* the roster is
never removed by a `tools:` list.)

The bound is subtractive **only by enumeration**: naming four tools costs
ten, `Skill` included. "Everything except Write and Edit" is nearly
expressible — the other eleven aliases spelled out — but it costs
`MultiEdit` too, because no alias grants it without `edit`; and every roster
change silently re-opens whatever the list forgot to name. A per-node `deny:`
says the same thing in one rule, and survives the roster changing.

One cost worth knowing before substituting one for the other: the
parallel-branch scheduler reads `tools:` and not the policy, so a node
bounded by `deny:` alone counts as possibly-mutating and loses read-only
fan-out eligibility. On claude_code the `tools:` list does not buy it back
either — the route takes the list as `--disallowedTools` over a roster
iterion does not own, so admission reads the node as possibly-mutating
whatever it declares (#1652). What buys the eligibility there is the
author's own assertion, `readonly:`, which admission honours; the list is
the bound on claw, and on codex for the names its sandbox reads as
read-only — `git_diff`, `tree` and the rest of the advisory vocabulary buy
the `workspace-write` mode, so a codex node declaring them counts as
possibly-mutating (#1727). The list read there is the one the route
RECEIVES, not the one the author wrote: `assembleEffectiveTools` appends
`ask_user` under `interaction:`, the board/runs MCP tools under
`capabilities:`, and `read_image` on every non-claw backend when a prompt
references an image `attachments:` entry — none of which is in codex's
read-only set, so those fields cost a codex node its read-only fan-out
eligibility too. `readonly:` buys it back, on codex as on claude_code. The
static reading (`iterion validate --exec`) answers the same, since all three
are decided by the program: `model.NewProgramExecutor` carries them.

A rule is a *bound on what runs*, never a grant of what exists: a node
whose `tools:` omits a tool cannot call it however its `allow:` reads. The
compiler still does not try to reconcile the two, and the reason is now a
measure rather than a fear of drift: a workflow `allow:` list is shared by
nodes with *different* `tools:` lists, so a rule inert on one node exists for
its siblings. Over the 81 shipped `.bot` files, a check of that shape fires 13 times on
`allow:` rules and **13 of 13 come from a workflow-level list** — it would
tell an author to change a node because of a rule written for its siblings
(#1579).

What the two fields no longer do is disagree about a *word*. Both read one
spelling table (`toolcatalog.CanonicalToolName`): a `tools:` entry grants
through a projection of that key onto each backend's roster, and a rule
matches the key a call canonicalises to. Before they shared it, `run_command`
granted native `Bash` and matched no rule, and `deny: ["tool_search"]` did not
bound a `ToolSearch` call.

Matching semantics (`pkg/backend/permission`):

- **Bash** patterns match the `command`; `prefix:*` is a prefix match,
  a bare wildcard `*`/`**` is a greedy match, no wildcard is exact.
- **Read / Edit / Write / NotebookEdit** patterns match the file path;
  `pkg/**`, `*.go` etc. work as gitignore-style globs.
- **WebFetch** patterns match `domain:<host>`, `<host>`, or the full URL.
- **Tool-name globs**: `*` (any tool) and `mcp__<server>__*`.
- **Tool classes**: `class:shell` matches by the registry's capability
  metadata — the member set, not a spelling ([tool classes](#tool-classes)).

**Cross-backend parity.** The same rule gates the matching tool on every
supported route: a single `Bash(...)` rule covers claude_code's `Bash`,
claw's `bash`/`shell`, pi's `bash`, Grok's `run_terminal_command`, and Kimi's
`Bash`; `Edit(...)` covers `Edit`/`edit_file`/`file_edit`/Grok's
`search_replace`; `Read(...)` covers `Read`/`read_file`; `Bash(...)` also
covers `run_command`, `Edit(...)` codex's `apply_patch`, `Write(...)`
`file_write`, and `ToolSearch` claw's `tool_search`; etc. (see
`toolcatalog.CanonicalToolName`, the one table, and `canonicalToolName` for
the MCP-name handling layered on it).

A row of that table asserts that two spellings ARE the same tool, never that
one suggests the other: `allow:` widens, so collapsing a narrower intent onto
a broader tool would grant more than the author wrote. `workspace_grep` is
therefore not `grep` and `diagnostic_shell` is not `bash` — `bots/copilot`
allows the first of each pair while denying the second. For the same reason a
second shell is not a spelling of the first — aliasing `repl` onto `bash`
would let `allow: ["repl"]` grant the whole shell — the table stays about
spellings, and what a rule bounds BEYOND one spelling is a class.

### Tool classes

A rule may name a CLASS of tools instead of one tool: `class:shell` in an
`allow:` / `ask:` / `deny:` row matches every registered built-in that
executes model-supplied code — today `bash`, claw's `repl` (which runs
python, node and shell) and `diagnostic_shell` (the bash executor under its
own approval name). The membership is the registry's own capability
metadata (`toolcatalog`): the enumeration lives with the tools it describes,
never in the gate, so a built-in that registers with the capability later is
a member of every class row already written — no permissions change, no
workflow edit. "Executes code" means executes code the MODEL supplies: claw's
`computer_use` and `skill_install` spawn processes too, but on fixed argv
(xdotool/import, git clone/fetch/reset) — the model supplies coordinates,
keys and a URL, never a command line — so they are not members, and that is
the invariant a future member must defend: a built-in that starts taking a
model-written command line belongs in the class, and one capability row in
the registry is the whole change. The spelling of a class row normalises
like a tool spelling: `CLASS:shell`, `class:SHELL` and `class: shell` (any
case, a space after the colon) all name the same class.

- `deny: ["class:shell"]` bounds the whole class, present and future. This
  is the row that closes what `deny: ["Bash"]` cannot: a spelling row
  bounds its canonical tool (`Bash`, `sh` and `run_command` all land on
  `bash`), and claw's `repl` is a SECOND shell no spelling bounds.
- `ask: ["class:shell"]` gates the class the same way: every member call
  pauses for a human, in every mode (an explicit ask precedes the mode
  default), future members included. The pause is the unsandboxed mechanism:
  on a sandboxed claw node a policy that can produce an ask refuses the NODE
  at dispatch (a sandbox has no seam to pause the run for a human), and an
  unsandboxed headless run stalls at the call until a human answers the
  approval — an asked tool executes nothing unattended in either shape.
- `allow: ["class:shell"]` grants the class including future members —
  `allow:` widens. Accepted, not refused: the row is the author's explicit
  choice, and a refusal would replace it (**C167** warns instead, and
  points at `ask:`).
- A class row composes with an `(arg)` scope (`class:shell(git push:*)`)
  and coexists with spelling rows without conflict — each rule is matched
  on its own, and neither narrows the other.
- A class row that matches no tool a node can call is a no-op there, like
  `deny: ["Bash"]` on a node that never runs bash: rules are inert where
  they match nothing, and a workflow-level row may exist for its siblings.
- An unknown class is refused at authoring (**C154**), naming the known
  classes.

The bound is workflow-local, and it follows the runs the node itself starts.
A subbot child is a separate run compiled from its own `.bot`: the parent's
rows do not flow into it, so a delegation across a subbot boundary needs the
class row restated in the child bot (the same workflow-local reading every
permission diagnostic has — **C111**, **C154**, **C135**). A claw subagent
is the opposite case, the same run: the child conversation the `agent` tool
spawns inherits the policy the node's own loop is gated by, so a denied or
asked class bounds the delegation too. Shipped hosts wire no child runner —
the `agent` tool answers a typed refusal (**C159** names the shape) — so
that inheritance is wired where the proxy is and takes effect the moment a
host wires it.

An operator grant outranks every row: approving a pause writes a grant into
the run's policy, evaluated after a deny rule and before every ask or allow
row (the ordering section above). The compiler sees workflow text and not
run state, so a grant is invisible to it — an ask row counts as a bound
because a human holds every call behind it, and the grant is that human
answering.

**C167** is the discovery half: when a gated claw node can call a
shell-capable built-in that its effective deny and ask rows do not bound —
`deny: ["Bash"]` beside `repl`, the cell this section closes — it names the
uncovered tools and suggests the class row. An ask row counts as a bound
(an asked tool pauses for a human in every mode); an allow row counts as
none. Measured over the shipped corpus — a CI fact, held by
`TestShippedCorpusStaysSilentOnClassCoverage`, which compiles every shipped
`.bot` as a unit and fails on the first fire: **0 fires** today, the copilot
bot asking `diagnostic_shell` and rightly silent among them. C167 also warns
on an `allow:` row granting a class.

### Claude Code diagnostic bridge

`diagnostic_shell` remains a Claw-only alias in the normal tool catalogue. A
node that explicitly declares it may, on Claude Code only, request one native
`Bash` command through that alias's approval card. The bridge maps only a
nonempty, single-line command, only when no explicit `deny: ["Bash", …]`
rule matches. The card and its one-time grant are scoped to the command alone;
model-authored descriptions do not broaden or break the retry. Multi-line
commands and every node that did not declare the alias remain ordinary native
`Bash` calls and follow the workflow's normal policy (typically `deny`).

The bridge is for bounded read diagnostics and source-nonmutating verification
such as a targeted test or `iterion validate`. It is not a generic shell
capability: do not use it for redirects, installs, network access, Git/source
writes, or a command whose effect cannot be inspected from the single-line
approval card.

**Infrastructure exemption.** iterion's own interaction/capability
plumbing — `ask_user`, the board / control / watch MCP families — is
never gated (or `ask` mode would pause on the very tool used to ask the
human).

## Precedence

Mode resolves with the same precedence as `compress:`:

```
CLI --permission  >  node permission:  >  workflow permission:  >  ITERION_PERMISSION  >  off
```

Rule lists resolve in two steps, which are **not** the same operation:

```
per kind:  node allow:/ask:/deny:  REPLACES  workflow allow:/ask:/deny:
then:      + --permission-allow / --permission-ask / --permission-deny
```

A node list wins over the workflow's when it declares one, per kind; the
run-level flags are appended to whichever won.

The studio Launch dialog captions the permission select with the
resolved mode and the level it came from ("effective: ask · from
workflow") — see [settings-precedence.md](settings-precedence.md). The
inspector edits both DSL levels: the workflow block's mode and its three
rule lists under Workflow Settings, a node's on the agent/judge form —
the two kinds that may declare their own. A `tool` node's inert
`permission:` (C112) gets no control, so no form offers a gate that would
never run; the workflow block is where its recovery rung is gated, and
the Workflow Settings help says so.

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
  aborts with `delegate.ErrAskUser` so the run pauses. **Sandboxed claw
  enforces the same gate**: the policy crosses the IPC as a pre-task
  `permission_policy` envelope (`permission.PolicyConfig` — raw rule
  strings re-parsed in-container by the same parser), and the
  `__claw-runner`'s own tool loop applies it to local builtins and
  proxied tools alike. The pre-task position makes a mixed-version
  fleet fail CLOSED: an older runner fatals on the unknown envelope
  instead of running the node ungated. What cannot cross is an **Ask
  decision** — nothing inside the container can pause the parent run —
  so a policy that can produce one (mode `ask`, or any explicit `ask:`
  rule, which outranks mode `deny`) is refused loudly at dispatch, and
  C136 warns about the coupling at compile time.
- **opencode** — cannot enforce the gate in any mode: it exposes no
  `PreToolUse` hook, so a node with an armed gate is refused at compile
  time (C176) and again at dispatch rather than run ungated. Its own
  declarative policy (`OPENCODE_PERMISSION`) is a plausible route to
  native `deny`, deliberately not wired — membership in the gate table is
  earned by a live denial, never declared. Note that a headless opencode
  run auto-*rejects* an `ask` verdict (measured on 1.1.19; later builds add
  a `--auto` flag that auto-*allows* it instead).
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
policy the hook cannot decode fails **closed**, and so does a panic: the hook
recovers and still emits a deny, because a process that dies with empty stdout
is read as *allow* by both CLIs — which would turn any future bug in the
evaluator into a silent gate bypass.

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

**The hook binary must live outside the workspace.** It is the third thing the
gated agent must not be able to reach, and the sharpest: unlike the frozen argv
it is re-executed on *every* tool call, and both CLIs fail open on a spawn
failure — so corrupting the file, not replacing it with a working one, is
enough. `proc.LocateIterionBinary` resolves next to `os.Executable()` first,
which in the repo-root shape (`./iterion run …`, or `task studio:dev` pinning
`ITERION_BIN` to a freshly built `./iterion`) is inside the very workspace
being gated. The path is absolutised before that check and before it is frozen
into the hook argv: a relative `ITERION_BIN` used to defeat
`pathInsideCheckout` (which cannot relate an absolute workspace to a relative
path) and would then resolve against the CLI's cwd — the gated workspace — at
spawn time. iterion refuses an in-workspace binary and points at `ITERION_BIN`
on a stable install path outside the repo.

**The hook process must not read the workspace either.** Both CLIs spawn it
with cwd = the project and re-execute it on every tool call; a timeout is an
ALLOW. So `__permission-hook` skips `loadDotEnvFromCwd` and `errtrack.Init`
(an unbounded `.env` the agent can write would be enough to blow grok's hook
timeout, and a `SENTRY_DSN` line in the same file would point the gate at an
endpoint the operator did not choose), and the registered command `cd`s into
the shadow home before `iterion` starts.

**Windows is refused.** The hook `command` is quoted for a POSIX shell; Node-based
CLIs on Windows run that string through `cmd.exe`, which does not treat `'` as
quoting. A spawn failure is an ALLOW on both CLIs, so enabling Developer Mode
for the symlink half of the seam would still leave the node ungated. Use
`permission: off`, or a backend with an in-process gate (`claude_code`, `claw`).

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
  **Its rung 4 is the exception:** under `policy: recover`,
  `recovery.max_agent_attempts > 0` hands the goal to a synthesised agent
  with real tools, and that agent declares no mode and no rules, so the
  **workflow** block gates it — the node's own reserved `permission:` is not
  copied onto it. Both halves of that condition are load-bearing: the ladder
  runs rungs 3-4 only under `policy: recover`, so the bound alone leaves the
  run stopping at the recipe. A workflow whose only node is such a `tool`
  node therefore gets no C111 on its rule lists: they are read, by the agent
  that runs after a deterministic recipe already failed.
  C111's reader test is **wider than the gate** — it counts a tool node on
  `recovery.max_agent_attempts > 0` alone, policy included or not — so the
  same silence covers a node that declares the bound under any other policy
  (`required`, `best_effort`, or none), where no rung ever runs and the lists
  really are inert. There, **C106**
  names the dead recovery config, which is the finding the author needs:
  the bound, not the rule lists, is what is unreachable.

## See also

- `pkg/backend/permission/` — the matcher + Policy (single source of truth)
- `docs/plugins.md` — the sibling opt-in `compress:` field this mirrors
- Diagnostics: **C110** (invalid permission mode), **C111** (a rule list
  that reaches no gated reader — including a workflow list every gated
  reader replaces), **C112** (tool-node `permission:` — parsed but not
  enforced), **C154** (an entry the gate's parser cannot read, refused at
  compile time instead of at dispatch), **C136** / **C176** (a route that
  cannot serve the gate in force for the node, node-declared `ask:` rules
  included).
