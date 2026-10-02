# DSL quick reference — workflow dials, edges, budget

The exact syntax of the cross-cutting fields, one line each. The per-node
reference is [docs/dsl.md](../../dsl.md); the product page stays the source.

**Top-level blocks:** `vars:`, `attachments:`, `prompt <name>:`, `schema <name>:`, `cursor <name>:`, node declarations (`agent`, `judge`, `router`, `human`, `tool`, `compute`, `emit`, `wait`, `await_answers`), `workflow <name>:`

**`compress:` field** (`on|ultra|off`) — command-output compression (the `rewriter` plugin kind, rtk by default) on the `workflow` block and on `agent`/`judge`/`tool` nodes. **Opt-OUT on agent/judge nodes**: when a rewriter plugin is enabled and its binary is present, compression defaults **on** (so rtk is used out of the box); disable per-run with `--compress off` (or the studio toggle) or globally with `iterion plugin disable rtk` / `ITERION_COMPRESS=off`. **Tool nodes stay opt-IN** (a review loop's `git diff` is never silently compressed). See the plugins section in
[plugins-compression.md](../backends/plugins-compression.md) + [docs/plugins.md](../../plugins.md).

**`auto_memory:` field** (`on|off`) — the backends' own auto-memory
(`MEMORY.md`) on the `workflow` block and on `agent`/`judge` nodes. **Off by
default**, so a run is hermetic: without it, claude_code's own default is *on*
and every node would read and write the operator's personal
`~/.claude/projects/<cwd>/memory/`. When on, iterion resolves ONE space
(visibility `bot`, reserved name `auto-memory`, keyed on the **repo root** so a
`worktree: auto` run doesn't start empty), materialises it on disk, and points
**claude_code, claw and pi** at that same directory — `--settings
autoMemoryDirectory` for claude_code (which has auto-memory of its own), a
rendered `# Auto memory` prompt section for claw and pi (which have none) —
then folds the agent's edits back through `knowledge.MemoryStore`, which is
what makes it survive a cloud pod. Precedence mirrors `compress:`:
`--auto-memory` → node → workflow → `ITERION_AUTO_MEMORY` → off — and unlike
`compress:` the run-level override travels all the way onto the cloud queue
(`RunMessage.auto_memory`, schema v6) and into a detached subprocess, so an
operator's `off` is never quietly replaced by a bot's `on`. It is not persisted
on the run, so `iterion resume --auto-memory` must re-state it. Diagnostics
C131/C132. A **copy-based sandbox** (kubernetes) refuses the feature with a
warning: it has a push seam but no per-file read-back, so the agent's notes
could not be synced. Distinct from the `memory:` block (iterion's own tools +
scopes). See [docs/memory-and-knowledge.md](../../memory-and-knowledge.md).

**`permission:` field** (`off|ask|deny`) + `allow:`/`ask:`/`deny:` rule lists — opt-in **tool-permission gate** (the anti-prompt-injection boundary). Mode and rule lists (Claude-Code `Tool(pattern)` syntax, e.g. `Bash(go test:*)`, `Read(**)`, `Edit(pkg/**)`) both on the `workflow` block AND on an `agent`/`judge` node. A node list **REPLACES** the workflow list of the same kind, independently per kind — never a union, because a union can only widen and the shape a heterogeneous workflow needs is a narrowing; the run-level `--permission-allow|ask|deny` stay additive on top of whichever list won. Resolution happens at one chokepoint, `resolvePermissionPolicy`; the route screens (C136/C176) read the node's effective ask list through the same helper (`ir.EffectiveAskRules`), so they admit a backend against the rules the runtime will gate on — DSL lists only, the run-level ones being screened at dispatch. Orthogonal to `tools:`, and measurably not inert on claude_code: a non-empty `tools:` becomes `--disallowedTools` over Claude Code's closed 14-name native roster (every roster tool the list omits is removed, `Skill` included; tools outside it are untouched), which is why a subtractive per-node `deny:` exists. The compiler does NOT try to reconcile a rule against `tools:` — that would require agreeing with three alias tables in three packages. C154 instead refuses an entry the gate's own parser cannot read. `off` (default) = today's bypassPermissions; `ask` pauses for human approval on any call not allow-listed; `deny` hard-blocks it (headless). The SAME resolved `permission.Policy` ([pkg/backend/permission](../../../pkg/backend/permission/permission.go)) drives claude_code's `wirePermissionHook`, claw's `executeToolsDirect` gate, pi's embedded RPC extension, and the external PreToolUse hook adapter used by Kimi/Grok. Claude Code, claw, and pi support `ask|deny`; Kimi and Grok are admitted for host-side `deny` only — their hook is an EXTERNAL process, so it can hard-block but cannot pause the run for `ask`. Both earned admission with a live denial (a real model's real tool call, a filesystem sentinel as the oracle), never by declaration. Unsupported primary routes are refused too — a declared gate must never become inert. Precedence (mirrors `compress:`): CLI `--permission`/`--permission-allow|ask|deny` → node → workflow → `ITERION_PERMISSION` → off. Diagnostics C110/C111/C112/C136/C154/C176. Three shapes C111 now gets right: a node that turns the gate on makes the workflow's lists live (the old predicate wrongly called them inert); a `tool` node's Verified-Action recovery agent rung is a reader of them (it declares none of its own), gate or no gate; and the shadow verdict counts readers mode-independently, because `--permission` can arm a node the DSL leaves off and that node then inherits every list it does not declare. See [docs/permissions.md](../../permissions.md).

**Edge syntax:**
```
src -> dst                              # default edge
src -> dst when <field>                 # conditional (boolean field from src output)
src -> dst when not <field>             # negated condition
src -> dst else                         # explicit fallback (fires only when no sibling `when` matched and no back-edge with work left)
src -> dst as loop_name(5)              # bounded loop (max 5 iterations)
src -> dst with {field: "{{ref}}"}      # data mapping
```

**Reference syntax:** `{{input.field}}`, `{{vars.name}}`, `{{outputs.node_id}}`, `{{outputs.node_id.field}}`, `{{artifacts.name}}`

**Convergence:** nodes with multiple incoming branches declare `await: wait_all` or `await: best_effort`; aggregation is a property of the downstream agent/judge/human/tool/compute node, not a separate `join` declaration.

**Budget block:** `max_parallel_branches`, `max_duration`, `max_cost_usd`, `max_tokens`, `max_iterations`. Each is overridable at run time without editing the `.bot` via the matching `iterion run`/`resume` flag (`--max-cost-usd`, `--max-tokens`, `--max-duration`, `--max-iterations`, `--max-parallel-branches`) — non-zero flag wins, zero inherits; precedence is DSL → recipe/preset → CLI flag. Lets you re-budget any bot per run (e.g. `--max-cost-usd 120 --max-duration 4h`) and is the mechanism behind the "budget exceeded → raise the cap + resume" recovery.

A loop's **back-edge is declined when the budget cannot fund another
iteration** — priced by what the previous one consumed, on every capped
axis. The run then leaves through its own exit path (the fall-through
that also serves loop exhaustion), so a campaign bot's delivery tail
still opens its PR with the work committed in stride, instead of the run
dying mid-pass on `BUDGET_EXCEEDED` and stranding it on a clone that
dies with the pod. A loop is priced from its own **entry** (and re-priced
on re-entry), so a second-phase or nested loop is never charged for the
work that preceded it; the prices ride the checkpoint. Visible as a
`budget_warning` carrying `reason: loop_budget_guard`. Precedence mirrors
`compress:` minus the node level (a loop is not a node): CLI
`--loop-budget-guard` → workflow `loop_budget_guard:` →
`ITERION_LOOP_BUDGET_GUARD` → on. Diagnostic C133. Like `auto_memory:` and
unlike `compress:`, the run-level override **travels onto the cloud queue**
(`RunMessage.loop_budget_guard`, schema v7) and into a detached subprocess,
so a pod never re-decides it. The 90%-hard-limit and exceeded checks remain
the backstop for a single node that overruns. See
[docs/dsl.md](../../dsl.md#budget-and-loop-back-edges).

That guard covers overruns caused by iteration COUNT; a single node that
overshoots the cap on its own is covered by the **exit grace**
([pkg/runtime/budget_exit_grace.go](../../../pkg/runtime/budget_exit_grace.go)).
Once a cap is *spent*, the run may walk **forward** — never around a
declared `loop`; a `foreach` back-edge is bounded by its collection, not
priced, so only the declared-loop form promises "it cannot iterate again" —
spending up to `cap × 1.1` to reach a terminal node, so work it has already
paid for gets delivered instead of dying on disk. The ceiling is
PROPORTIONAL (a small cap grants a small grace) and past it the run fails as
`BUDGET_EXCEEDED` as before. Both *exceeded* stop-paths — the pre-exec check
and the deferred overrun after a node that succeeded — go through one
decision (`graceOrFailBudget`), so a node is never refused by a stricter
rule than the one that admitted it; a node whose OWN spend crosses
`cap × 1.1` still completes and then ends the run. The 90% hard limit (`budgetHardThreshold`,
refusing a new node while an axis is in `[90%, 100%)`) is a SEPARATE,
un-graced path reached only when nothing is exceeded yet — so a run refused
at 92% gets no grace while one at 105% may walk on, which is surprising
until you see that the grace begins where the cap ends. The grace is REFUSED outright in two cases: when
the loop budget guard is off (the "no further iteration" half of the safety
argument is that guard's), and when the cap was CLAMPED by an authority
outside the run (`ir.Budget.CapImposed`, set at the single choke point
`Budget.ClampToCeiling` — platform ceiling, credential-pool donor allowance;
the marker travels the queue as `BudgetOverrides.cap_imposed`) — an imposed
cap is an absolute promise to a third party. `ITERION_BUDGET_EXIT_GRACE`
overrides the ratio and fails **closed** (`0`/`off` = absolute caps; an
out-of-range or unparsable value also means 0, with a one-time stderr
warning). Every graced node emits `budget_exit_grace {dimension, used,
limit}`, rendered by `iterion report`: a deliberate overspend has to be
visible in the events, not discovered on the invoice.

