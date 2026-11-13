# ADR-165 — Codemode: the model's code runs in a credential-less child process whose only capability is the node's own tool door

- **Status**: Accepted — the operator decided the mandate on 2026-11-10 (codemode in iterion; cloud before local, web and desktop; claw as the reference harness; the fold as library-only). CM-2 built (`d13f6391e`, #3154): the tool dispatcher and the context contract. This record is CM-0. CM-1 (the script host), CM-3 (declarations and discovery), CM-4 (the loop binding) and CM-5 (the DSL switch) to come.
- **Date**: 2026-11-10
- **Deciders**: jo (the mandate and its four arbitrations), Claude (exploration, design, the five corrected facts)
- **Extends**: [ADR-029](029-sandboxed-claw-via-hidden-claw-runner-ndjson-ipc.md) — a hidden subcommand reached over NDJSON IPC is already how this tree runs a contained child; codemode adds a second such child with a narrower capability than claw's runner.
- **Relates to**: [ADR-098 (connector catalog)](098-connector-catalog.md) F19 (goja bounds no heap — the finding this record answers) · [ADR-132](132-a-run-reads-no-environment.md) (the switch reads no environment) · [ADR-146](146-secret-boundary-v2-grants-and-sinks.md) (what a script may never hold) · [ADR-151](151-board-actions-behind-the-action-seam.md) (`capabilities:` is the one grant surface) · [ADR-162](162-the-fork-review-lane-is-a-gesture-on-floors-that-land-first.md) (the fork lane is the one pool where a sandbox exists by default)
- **Serves**: [#3153](https://github.com/SocialGouv/iterion/issues/3153) (epic), [#3155](https://github.com/SocialGouv/iterion/issues/3155) (the in-pod gating bench)

## Context

A tool call costs context twice: the declaration the model reads and the result it reads back. A node with thirty tools spends its window describing them, and a loop over forty files spends it again on forty results it mostly discards. Codemode is the answer the field converged on in 2026: give the model **one** tool whose argument is code, run that code harness-side, and let it call the node's tools in a loop, keeping the volume out of the conversation and returning a distilled answer.

What makes it a security decision rather than an ergonomic one is that the code is written by a model reading untrusted text. Five facts, each re-verified in this tree, decide the shape:

1. **In production cloud, nothing is sandboxed.** `charts/iterion/values.yaml:254-255` ships `runner.sandbox.enabled: false`, and `charts/iterion/templates/configmap.yaml` renders `ITERION_SANDBOX_OVERRIDE: "none"` with the reason in its own comment — "pod is the isolation boundary and the runner image ships the toolchain". So the brain/hands separation codemode is usually described with **does not exist** in the shipped posture: a script host, the bash a node already has, and the runner would be siblings in one pod under one uid.
2. **The engine cannot bound itself — and we already own a host that proves it.** ADR-098's F19 (`:713-715`) closed on exactly this: *"goja's interrupt does not preempt native calls and bounds no heap: transforms stay refused until the limits are enforceable"*. What makes that concrete rather than theoretical is that **claw-code-go already executes model-authored JavaScript**, and claw-code-go is first-party and is being folded into this tree (flux MG): `internal/runtime/workflow.go:129,134` runs a prelude and then the model's script on a goja runtime over a `goja_nodejs` event loop, bound to `args`, `log`, `phase`, `agent`, `__wfResolve`, `__wfReject`, capped at 512 KiB of source, 15/60 minutes and 200 `agent()` calls. No iterion node can reach it — `workflow` is a claw tool iterion does not advertise — but it exists, it is ours, and it is the machinery CM-1 adopts rather than writes. Two differences decide CM-1's actual work: that host binds **`agent`**, so a script orchestrates sub-agents and never touches the node's tools; and it runs **in process**, so its caps are the engine-level ones F19 says are not enforceable. CM-1 is therefore "move that host into a child process under kernel limits, and swap the `agent` binding for a `tools.*` binding backed by `toolDispatcher.dispatch`" — and per AGENTS.md a seam that crosses claw is improved in its `.works/` worktree and bumped, not forked here. (Outside the vendored tree nothing executes JS: `pkg/botimport` imports `goja/ast` and `goja/token` for parsing only.)
3. **A nested call is only a direct call if it is literally the same call.** CM-2 made that enforceable rather than documented: `newToolDispatcher` returns the context it vouches for and `dispatch` refuses any other, because four per-node facts ride the context and three fail **open** — the `allowed-tools:` narrowing (#1717), the child-ask degradation (#2877), the parent policy a child inherits (absent, a child spawned through `agent` is ungated entirely) and the parent loop shape that bounds a child's tools.
4. **An undeclared `tools:` list is no tool at all on claw**, not the full surface (`pkg/backend/toolcatalog/declared.go`). A script that discovers tools must therefore discover the node's **declared** set, never the registry's catalogue.
5. **A bound with no ceiling is a bound a tenant raises.** The precedent is already in the tree: `applyCloudBudgetCeiling` (`pkg/runner/loop.go:3442`, called at `:2613`) clamps a workflow's budget to a platform ceiling the bundle cannot lift.

## The laws (non-negotiable, each to be pinned by test)

1. **A call a script makes goes through `toolDispatcher.dispatch` and nowhere else.** Not "also honours the gate" — the same function, so hooks, command scope, `permission.Policy`, secret materialization and events cannot drift. The AST guard `toolFunnelCallers` (landed with CM-2) refuses a second path into the funnel.
2. **The script host holds no credential.** Not a redacted one, not a materialized one: its environment and its `/proc` carry none, and it has no path to the model.
3. **Confinement is kernel primitives in a child process, applied BEFORE the VM starts.** Never engine bounds (law 2's source is F19). `GOMEMLIMIT` is a GC target and is not a memory cap.
4. **Every bound has an operator override AND a platform ceiling the tenant cannot raise.** Philosophy 1 forbids a limit with no escape hatch; cloud multi-tenancy forbids an escape hatch with no ceiling.
5. **"Usable in cloud" is declared by an in-pod bench, not a sandboxed one** — because in-pod is what ships (fact 1).

## Decision

**The host.** A hidden subcommand, `iterion __script-host`, in the shape ADR-029 established for `__claw-runner`: a child process speaking NDJSON over stdio. Its engine, prelude shape, event loop and resolve/reject bridge are claw's `internal/runtime/workflow.go` moved rather than reinvented — the binding set is what changes. It holds one host binding, "call a tool", in place of claw's `agent`; values cross as **JSON only** and no Go object is reflected into the VM. `SetRandSource` and `SetTimeSource` are pinned, the clock is UTC, and there are no timers, so a script is deterministic given its inputs and its tool answers.

Claw's existing caps (source size, wall clock, a runaway backstop on child calls) carry over as the *declared* axes they already are; what they lack is CPU and address space, which only the kernel can hold.

**Confinement, in order, before the interpreter exists.** `RLIMIT_DATA`/`RLIMIT_AS` for a real memory cap, `RLIMIT_CPU`, `no-new-privs`, non-dumpable, a parent-death signal, and Landlock in deny-all where the kernel offers it. Where Landlock is absent on an in-pod runner, codemode is granted **only** to a node that already holds a `class:shell` tool — it adds no reach that node lacks — and is otherwise refused as `codemode_unavailable`. A typed refusal, never a silent degradation.

**Two named cloud topologies.** *In-pod* is the default and the one that must be proven first: the host is a child of the runner process, confined by the primitives above, and the pod remains the outer boundary. *Sibling pod* is the stronger form for pools that want it, and it is a deployment choice, not a per-bot one.

**The bounds, each with an override and a ceiling** (values set in CM-1 and CM-5, not here): wall-clock per script, CPU time, address space, output bytes returned to the model, tool calls per script, concurrent tool calls, nested-agent children per invocation and per node, and `store()` entries and bytes per run. Each reads from the same cascade as the rest of the DSL (ADR-132: no environment), and each is clamped the way `applyCloudBudgetCeiling` clamps a budget.

**What a script may not do.** It cannot pause the run: `ask_user` inside a script is a typed refusal that names the direct call as the way to ask, because a deferred tool whose verdict can be `Ask` would otherwise be reachable only from a place that cannot honour it. It cannot choose a child's model, and a scriptable `tools.agent` requires a finite reserve, booked before the child runs rather than recorded when it returns.

**Durability is a separate decision.** A journal-and-replay script that survives a pod restart depends on mid-node resume (#1403) and is explicitly **out of v1**. v1 scripts are re-run, not resumed.

## Consequences

- The model's declaration budget drops to one tool plus a discovery index, and result volume leaves the conversation. That is the win, and it is why the switch defaults off until CM-17 measures it against a named abandonment criterion written **before** CM-5 lands.
- Codemode is a second place a tool call originates. The census that keeps that honest already exists (`toolExecuteSites` and `toolFunnelCallers`, CM-2): a third path has to declare itself.
- The in-pod posture means a confinement bug is a pod-level compromise, not a sandbox escape. #3155 is therefore a **gate**, not a nice-to-have: memory bomb and native-regex bomb killed while a second run in the same pod completes, no credential in the child's environment or `/proc`, no orphan after a drain, and `oom_score_adj` plus RSS probing working under the runner's `securityContext`.
- Every bound is a new refusal surface. A false refusal in a blocking tool costs as much as a hole and is harder to see, so each bound ships with a witness on both arms — refused above, allowed below.

## Alternatives rejected

- **In-process goja with interrupt-based bounds** — which is what claw's own `workflow` tool does today, so this is not a hypothetical but a choice to *not* copy one half of a design we own. Rejected by F19 on the evidence that closed it: the interrupt does not preempt a native call and bounds no heap, so a native regex bomb hangs the runner rather than the script. Acceptable where the host is a developer's own CLI and the blast radius is their terminal; not acceptable where it is a multi-tenant pod that is itself the isolation boundary.
- **Exposing claw's `workflow` tool as-is once the fold lands.** Tempting, because it would be free — and refused: its binding is `agent`, not the node's tools, so it composes sub-agents instead of tool calls, it is in-process, and it was never gated for an iterion node (iterion does not advertise it). Adopting its *machinery* is the decision; adopting its *surface* would ship the unbounded half.
- **A WASM runtime (QuickJS-in-WASM, as Pi uses).** Defensible and stronger on memory, rejected for v1 on two grounds: it adds a toolchain and a second language runtime to an image that already ships one, and the kernel primitives above give a real cap without either. Revisit if the in-pod bench cannot hold the memory axis.
- **Granting the script a credential so it can call a model directly.** Refused: it would put a live key beside model-authored code, and the sub-agent path already exists behind the dispatcher with its own accounting.
- **Letting a script pause the run.** Refused: a pause is a conversation-level event and a script is not a conversation; the deadlock in the deferred-tool case is the concrete reason.
- **Proving cloud readiness on the sandbox crossing first.** Rejected because it proves the posture nobody runs (fact 1). The crossing is CM-6, after the in-pod bench.

## Watch

- **The Landlock fallback is a judgement, not a proof.** "A node that already holds `class:shell` gains nothing" is true of filesystem reach and false of CPU and memory: a shell tool is bounded by its own timeout, a script by the bounds above. If the bench shows the memory axis unenforceable without Landlock, the fallback becomes a refusal.
- **`store()` is durable state and owes its cloud twin in the same change** (philosophy 3), including the `DeleteRun` sweep and a shared conformance case.
- **The tool-call quota is counted launcher-side.** External-effect tools are exempt from the permission gate today (`executor_build_task.go`, `permission.go`), so an injected line that can place one card can place a hundred; the quota that bounds that has to be counted where the launcher sees it, not where the runner reports it.

## Amendment — 2026-11-11: what the CM-1 reconnaissance refuted in this record

Four parallel readers were sent to inventory the ground CM-1 stands on, with probes run against the engine versions this tree pins. They refuted five statements above. All five are corrected here rather than quietly reworded, because each would have made an implementer build the wrong thing.

**The binding list is not the surface, and determinism is a property to build.** This record described claw's host as bound to `args`, `log`, `phase`, `agent`, `__wfResolve`, `__wfReject`. A script also sees `require`, `console` and `setTimeout`/`setInterval`/`setImmediate`/`clear*`, injected by `eventloop.NewEventLoop()`'s defaults (goja_nodejs `eventloop/eventloop.go`, measured: `typeof require` is `function`). So "there are no timers, so a script is deterministic" is a property CM-1 must **construct** — `EnableConsole(false)`, a `require.Registry` with a refusing loader, and the timer functions deleted — not one it inherits. "No Go object is reflected into the VM" is already false of the inherited host, because `require` is one.

**That host has a filesystem READ primitive, not merely no filesystem restriction.** `require()` falls back to `require.DefaultSourceLoader`, which is `os.Open` on the host filesystem, and a `.json` path is returned parsed. Executed: a script read a probe file from disk and returned its contents. Since claw's own credential stores are JSON, this is a finding in claw independent of codemode, and it is why killing `require` cannot wait for Landlock — Landlock is absent on many in-pod kernels.

**F19 reproduces, and the inherited timeout cannot return on the class it names.** A regexp2-routed catastrophic regex (a backreference or a lookbehind, which goja does not route to Go's linear-time engine) wedges the loop goroutine; `vm.Interrupt` does not preempt it and `eventloop.Stop()` then blocks — measured at over 8 s. `executeWorkflow` registers `defer el.Stop()` before its select, so on that class the function that decided "timed out" cannot return. Claw's own `TestWorkflowTimeoutInterruptsBusyLoop` passes because `for(;;){}` **is** preemptible: the test covers the easy arm and reads as proof of the hard one. Consequences: CM-1 lets the child's death be the stop, and #3155's bench uses a regexp2 bomb rather than a busy loop.

**"Move the host" cannot be an import.** `executeWorkflow`, `workflowPrelude` and `workflowAgentBinding` are unexported in claw's `internal/runtime`; its public `pkg/runtime` facade is type aliases for compaction, prompt and provider only; and iterion imports zero `internal/` paths from claw. Counted here, per package path, over `*.go` outside `vendor/` and the worktrees: **193 files import `pkg/api`** (240 import lines), **13 import `pkg/runtime`**, **0 import anything under `internal/`** — the only two textual mentions of an `internal/` path are a comment and a test's source-file constant. So CM-1a opens with a scoping decision — claw grows an exported script-host package in its `.works/` worktree and iterion bumps, or iterion writes the host with claw's file as the reference text.

**In cloud this is the FIRST such child, not the second.** The `__claw-runner` precedent is spawned only under `task.Sandbox != nil`, and fact 1 above (`ITERION_SANDBOX_OVERRIDE: "none"`) makes it nil in production. So the ADR-029 shape holds for the IPC and the hidden-subcommand convention and **not** for the spawn, which has no precedent in this tree and has to be written. Everything the sandbox crossing does for safety is on a path production never takes.

### Two corrections in the other direction

**Landlock is free, and it is not filesystem-only.** `golang.org/x/sys/unix` is already a direct, vendored dependency carrying the syscall numbers and all the constants, with no cgo — no library to add. And ABI 4 includes `LANDLOCK_ACCESS_NET_CONNECT_TCP`, which is already vendored: on a current kernel the child can deny its own TCP connect with no privilege and no proxy. That makes the network arm of the escape suite cheap, and it makes the `class:shell` fallback reasoning above **incomplete**: where Landlock is present codemode holds a network guarantee a shell tool does not, and where it is absent it loses one. The fallback is re-argued when the bench measures it, not before.

**gVisor/runsc is nowhere in this repository** — zero word-boundary occurrences of `gvisor`, `runsc` or `runtimeClassName` outside this record. Grep it as a word: case-insensitively and as a substring, `runsc` returns 416 hits in 97 files, every one of them a `RecentRunsCard` or a `NodeRunsChip`. Whatever runs in the cluster, no part of CM-1's confinement can come from it in any topology this tree can express, and no test or chart here can assert it.

### What the recon changed about the plan, not the record

CM-1's binding must land inside `pkg/backend/model`, or widen the walk of `toolFunnelCallers` in the same change: that guard reads `os.ReadDir(".")`, so a host in `cmd/iterion` is invisible to the very check that exists to refuse a second dispatch path. And the one existing IPC tool proxy — `ClawBackend.multiplexerHandler`'s `OnToolCall` — ends at `td.Execute` with the gate re-applied by hand and never constructs a dispatcher, so the nearest working example is the anti-pattern. Since the dispatcher is a per-node value built inside `GenerateText`, the script host's parent is the node's tool-execution site, not the backend's crossing.

Seams to implement rather than write: `pkg/backend/delegate/envelope.go` (one framing, plus its fail-closed convention), `pkg/internal/proc` (binary location, process group, wait delay, work dir — and the new self-confinement verb, since `SysProcAttr` carries no rlimit field), `pkg/config/scrub_linux.go` (`PR_SET_DUMPABLE`, already written, with a witness that reads a child's `/proc/<pid>/environ`), `internal/proctest`'s `NoProcessLeaks` for the no-orphan arm, and `pkg/operatormcp/spawn.go` as the stdio-child template. No rlimit seam exists anywhere in the tree.

## Amendment — 2026-11-13: the abandonment criterion CM-17 measures against

The record above says the switch "defaults off until CM-17 measures it against
a named abandonment criterion written **before** CM-5 lands". The attribute
landed inert and self-declaring (`bfb031129`: `codemode: on|off` carried to the
IR, a C338 warning that no build served it), which is why that slice could land
without this. The slice that gives the switch an EFFECT cannot. Here is the
criterion, named, before any measurement exists to be fitted to.

**What is measured.** `TurnUsage.InputTokens` per node turn, read from the run
store (`pkg/store/turn.go`) — no new instrumentation. Only turns where the
backend reported a real split count: a turn carrying `AggregateTokens` instead
is **excluded from both arms**.

**The void condition, corrected 2026-11-13 after an adversarial round showed
the first draft voided itself.** That draft read "if the two arms do not
retain comparable counts of countable turns (within 10%) the measurement is
void" — and collapsing many tool-call turns into one program turn IS
codemode's mechanism, so the arms' absolute turn counts diverge by
construction and the only reachable verdict was `void`. A criterion that can
only return one answer is not a criterion. What the clause was reaching for is
that neither arm may lose its EXPENSIVE turns to exclusion, so it is stated as
a **retention rate**: countable turns ÷ total turns, computed per arm, and the
measurement is void if the two rates differ by more than 10 points. The arms
may then differ in turn COUNT as much as codemode makes them — which is the
thing being measured — while a backend that silently stopped splitting its
counts on one arm still voids the run.

**And a rate alone is not enough, which a sixth adversarial round showed by
constructing the cheat the clause is named for.** Two arms can hold the same
90% retention while one drops only CHEAP turns and the other drops its single
expensive program turn: the rate reports a 0.0-point difference and the median
falls 98%, a win manufactured from nothing but which turns each arm lost. A
rate is blind to WHICH turns it drops. So the second half of the clause: an
excluded turn still carries a number — `TurnUsage.AggregateTokens` — and the
measurement is also void if, in either arm, the median `AggregateTokens` of
the EXCLUDED set and the median `InputTokens` of the countable set differ by
more than 2×. In the constructed cheat that is 1,000 against 50,000. Still no
new instrumentation: the number was already in the store and was being thrown
away.

**On what.** At least three catalogue bots whose output is contractual, and at
least twenty runs per arm per bot, same inputs, same models. Two arms: the
same bots with `codemode: off` and with `codemode: on`.

**The threshold.** The MEDIAN input tokens per countable turn must fall by at
least **20%** in the `on` arm. The median rather than the mean: one run that
avoids a single enormous tool result would carry a mean on its own, and the
thesis is about the ordinary turn, not the outlier.

**At equal verdict quality.** Each bot's own contract must still hold —
`review-pr`'s gate verdict and finding count, every other bot's declared
output schema and deliverables. A regression in any bot's contract voids the
win whatever the token number says. The win is not "fewer tokens"; it is
"fewer tokens for the same work".

**And if the threshold is not met: the switch is REMOVED** — the attribute,
its two diagnostics and the tool registration — rather than shipped off by
default. A bound nobody can justify is a maintenance cost and a bound with no
override (philosophy 1 cuts both ways): the honest end of a measured
experiment that failed is deletion, not a dial left at zero.

## Amendment — 2026-11-13: the capability is refused, never degraded

The slice that registers the tool had to answer a question the record above
left open: what happens to a node whose author wrote `codemode: on` and whose
backend serves no codemode tool. Three decisions, each against a plausible
alternative.

**It is a typed refusal, not a silent degradation.** Philosophy 1 says warn
rather than reject, and the first draft of this check was a warning. It is
wrong here, and `ambient_context` is the precedent that says why: a declared
value the backend cannot honour is refused at dispatch
(`ErrCapabilityUnsupported`), while an inherited DEFAULT is not. A node that
silently ran without the capability its author asked for would keep its whole
loop in context and bill the very cost codemode exists to avoid, with nothing
in the run saying so — the "fallback silencieux" the conventions forbid. The
DEFAULT is never refused, because a fleet inheriting nothing must not start
failing the day a registry cell moves.

**The refusal is TERMINAL, and an earlier draft of this amendment said
otherwise.** It claimed a `fallbacks:` route would carry a node past an
`ErrCapabilityUnsupported`; an adversarial round executed that and found no
route is tried — a refused PRIMARY never enters the chain (its task is built
outside it), and a refused FALLBACK ends the chain rather than being skipped,
which the twin's own `TestAmbientContextRefusesAFallbackThatCannotHonourIt`
already asserted. In this codebase the class ends a node. Being typed is what
lets a reader tell a missing capability from a transport failure; it is not a
recovery path. The claim is corrected here rather than quietly dropped,
because it was one of three legs the decision stood on; the other two carry
it — silent degradation is forbidden, and the operator keeps two escape
hatches (`codemode: off` on the node, or a route to a backend that serves it).

**What a dialled route is judged as.** The compiler resolves
`backend: "${VAR:-name}"` to the name written in the SOURCE — deliberately, so
one artifact compiles the same on every machine — while the dispatch expands
the same text from the pod's environment. The two can name different backends,
so C339's verdict says when it was read from a source default, and a
deployment that dials a route elsewhere is judged at dispatch.

**Migration, owed because C338 invited the declaration.** `codemode: on` on a
non-claw backend COMPILED in v5.32.0 and v5.32.1: C338 warned that nothing
served it, and its hint read "keep the declaration if you are preparing a
bot". It is now refused. A `.bot` written in that window — and an already
queued run carrying one — must re-route the node to a backend that serves
codemode or declare `codemode: off`. The wire version fence does not catch
this: such a message is inside the accept window, so an upgraded runner
recompiles it, C339 refuses it, and the run parks on the DLQ.

**The cell is the capability ceiling (law 4).** `claw` is `shipped`, every
other backend `typed-refused`, and the discriminator is not model quality but
structure: codemode's guarantee is that a nested call IS a direct call, which
only a loop that owns the tool dispatcher can offer. A CLI-agent backend owns
none. The compiler reads the same registry the engine reads
(`ir.CodemodeServedBy`), so the two cannot disagree — a compiler that accepted
what the engine refuses would cost an operator a green gate and a failed run.
The bounds ceiling is unchanged: `scripthost.Ceiling`, clamped in the parent.

**C338 is retired, not renarrowed.** The warning that said nothing served the
attribute shipped one day earlier and its own text promised this slice would
delete the emission. Its successor, C339, asks a narrower question and is an
ERROR rather than a warning, because what it diagnoses now fails the dispatch
instead of running degraded — C267's discriminator. A code names one defect
for as long as anyone can read it in a log, so the number is burned rather
than reused.

**The wire version rises with it (v31).** `codemode:` adds no RunMessage
field — it rides `IRCompiled`, which already travels — but a pre-codemode
runner's decoder drops the key and runs the node without the tool, which is
the additive-field rule's case: a knob the caller set, that a stale runner
silently falls back from. `MinSchemaVersion` stays 27.
