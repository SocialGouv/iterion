# ADR-165 — Codemode: the model's code runs in a credential-less child process whose only capability is the node's own tool door

- **Status**: Accepted — the operator decided the mandate on 2026-11-10 (codemode in iterion; cloud before local, web and desktop; claw as the reference harness; the fold as library-only). CM-2 built (`d13f6391e`, #3154): the tool dispatcher and the context contract. This record is CM-0. CM-1 (the script host), CM-3 (declarations and discovery), CM-4 (the loop binding) and CM-5 (the DSL switch) were to come when this record was written; the amendments below say what landed.
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

## Amendment — 2026-11-14: what the in-pod bench measured, and two legs it refused to invent

#3155's bench exists and holds: seven legs on the SHIPPED binary inside its
OWN image, under the runner's real securityContext, on docker AND containerd.
[The posture bench](../agents/testing/deployment-posture-bench.md) carries the
method; this records what it changes in this ADR.

**It does not replace the unit suite, and the first draft of this amendment
said it did.** `pkg/scripthost`'s suite does not go green without Landlock — it
fails loudly, skipping only when a lane sets
`ITERION_CODEMODE_ALLOW_NO_LANDLOCK=1` to go on record (measured: 5 passes
become 5 failures under ENOSYS and 5 under EPERM). What it cannot do is judge
the shipped image under the deployment's posture, which is the only thing the
bench adds.

**The feared answer was not the one we got.** The confinement this record
mandates rests on `landlock_create_ruleset`, and a container's seccomp profile
can refuse a syscall it does not list — `Confine` names that shape in its own
comment, and collapsing it with absence once produced a child with a full
filesystem and no error. Measured on `ghcr.io/socialgouv/iterion@sha256:0eeb55…`
(v6.0.0) under `runAsUser` 10001, capabilities dropped,
`allowPrivilegeEscalation: false` and the daemon's default seccomp profile
(`Seccomp: 2` with a filter loaded, read back from the child's own
`/proc/self/status` before anything was concluded): **`landlock_abi: 4`,
`all_threads: true`**, both bombs biting. The posture's non-seccomp parts do
not disturb Landlock.

**Both runtimes, because `RuntimeDefault` is not one profile.** It names
whatever the runtime defaults to, so a bench measuring only docker would answer
for containerd by assumption. The containerd arm is a real Pod carrying the
chart's securityContext on a local `kind` cluster (a devbox package of this
repo): containerd 2.1.1, `seccompProfile: RuntimeDefault`, `landlock_abi: 4`,
`all_threads: true`, container exit 0. Each arm reads its filter back against an
`Unconfined` twin reporting `Seccomp: 0`, so a silently unconfined container
cannot pass for a confined one — and that twin also isolates `NoNewPrivs: 1` as
coming from `allowPrivilegeEscalation: false` rather than from seccomp.

Seven mutants, six killed and one assumed, and the two that a SECOND
adversarial round added are the instructive ones. The seccomp pair — `EPERM`
and `ENOSYS` for that one syscall — reddens both arms and differently for each
(no confinement reported vs `landlock_abi: 0` plus the `allow_without_landlock`
remedy); in-cluster they are Kubernetes `Localhost` profiles. A Landlock that
installs nothing while still reporting ABI 4 reddens the escape leg. No
Landlock plus `-allow-without-landlock` exits 5, *it is not a pass*. `--uid 0`
is refused before any leg runs.

**THREE adversarial rounds, and the lesson is that each round killed a guard
the previous one had just written.** Round 2 defeated two round-1 fixes: the
same stubbed Landlock **plus `RLIMIT_NOFILE: 0`** made all three escape
attempts fail with `EMFILE`, and a leg asking only *whether* they failed
printed "too many open files" and passed — so the oracle requires `EACCES`,
the errno Landlock refuses with on ABI 4, connect included. And a **global
lock around every run** serialised the concurrency leg while still reporting
the maximum overlap of timestamp windows, because those windows include the
wait in queue.

Round 3 then defeated round 2's replacement. Counting live children is
satisfied by **either group alone**, so a scheduler admitting the three
siblings together and then the three bombs together passed with a peak of 3
and zero meetings. The kinds are now told apart by `argv[0]` — the bombs run
through a second `Host` whose `Binary` is a symlink of the same file, which
`command()` puts in the child's argv — and some 5 ms sample must see both at
once: 0 under that mutant, ~330 honest.

Round 3 also found the posture guard reading only the EFFECTIVE capability
set, which an unprivileged uid holds empty whatever it may acquire: a
`--privileged` container with a permissive seccomp profile reported every leg
held, with the host's disks visible inside it. It now requires the BOUNDING
set empty too — `CapBnd 0000000000000000` under the chart's
`capabilities: drop: [ALL]`, `000001ffffffffff` in that container.

Round 4 found the same shape once more, in the one input this gate exists to
judge. `Seccomp: 2` names the MODE and never the PROFILE: an **allow-all
seccomp profile alone** — no `--privileged`, nothing else — reported every leg
held with exit 0, because a filter that denies nothing reports exactly the two
fields the guard read. The profile comes from the daemon's `--seccomp-profile`
and the kubelet's `RuntimeDefault`, and nothing was reading it, which is the
defect this bench already refuses for the runner image and the kind node
image. So the posture now asks the KERNEL: `add_key` must answer `EPERM`,
which a runtime default refuses and a permissive profile lets through
(measured: `EPERM` under the daemon default, `ENOKEY` under allow-all);
`ENOSYS` is reported as not covered. Round 4 also found `--user 10001:0`
accepted, the evidence files citing a `CapBnd` they did not contain, and a
run with all seven legs executed answering the code reserved for "nothing was
tested".

One round-1 guard went the other way and was REMOVED: an asserted margin on
the spinner compared wall time to a CPU bound and flipped its own verdict at
14.9 s and 17.0 s over a healthy deployment.

**Four rounds, four rounds with findings, and the pattern is the record's real
contribution**: every round killed a guard the previous had just written, and
twice the guard had been fixed in ONE FIELD of a two-field class. The mutant
register is therefore a page of its own
([posture-bench-mutants.md](../agents/testing/posture-bench-mutants.md)) with a
standing rule — adding a guard to that bench means adding the mutant that
proves it.

The assumed survivor is named: with `applyParentDeathSignal` turned into a
no-op the drain leg stays green, because that function's own doc calls
`Pdeathsig` a backstop rather than the orphan bound — so that leg witnesses
the property a drain needs and nothing about which holder delivers it.

**What is still owed**: the production node's own containerd build, since a
default profile is a property of that build. The structural question is settled
— a containerd `RuntimeDefault` does not refuse `landlock_create_ruleset` — so
what remains is re-running this bench against a production node and recording
its version, not designing an experiment.

**Two legs of the gate name mechanisms this codebase does not have — but the
axis they protect is OWED, not covered.** The Consequences section asks for
`oom_score_adj` and RSS probing to work under the runner's securityContext, and
neither exists here (zero production sites, every spelling). So those legs
cannot be written as specified. What must not follow — and the first draft of
this amendment did follow it — is that the axis is therefore handled:
`Confine`'s own comment says `RLIMIT_DATA` bounds the Go heap and that *a
`MAP_SHARED` or memfd mapping is not charged to it, so a native payload's real
ceiling is the pod's memory cgroup*, which is exactly the ground those legs
covered. The concurrency leg bounds each bomb by its own `RLIMIT_DATA` and
cannot observe an eviction. The requirement stands as written and is
unsatisfied; what is retired is the SHAPE of the two legs, not the axis.

**And the bench found a gap in the product while measuring itself**:
`Limits.Clamp` enforces no FLOOR on `Memory`. A bound below what the child's Go
runtime needs makes it crash — measured at 64 MiB, `script_host_died` with a
SIGSEGV where the caller asked for `memory_exceeded`. A tenant can therefore
write a `memory:` that produces an unexplained crash instead of a typed
refusal. Its own ticket.

## Amendment — 2026-11-15: the bounds get an author surface, and the A/B that did not show a gain

Two things landed together, and only one of them is good news.

### The surface (CM-5d, #3301)

Seven of the bounds this record calls for are now declarable: the `codemode:`
block, `Form: BlockOrIdent` on `sandbox`'s precedent, hosted on the workflow,
agents and judges. Operational detail in
[dsl.md](../dsl.md) and
[codemode-confinement](../agents/backends/codemode-confinement.md#the-authors-surface);
the decisions this record owns are these four.

- **`ir.CodemodeBounds` IS `scripthost.Limits`**, not a struct that maps onto
  it. One type means one spelling of the seven numbers, so the `wall_ms`
  defect the host's own field doc records — a hand-written bound enforced a
  million times tighter — cannot be reintroduced by a second declaration.
- **The rungs resolve FIELD BY FIELD**, node over workflow, not
  nearest-rung-wins. The workflow rung is a *default* by its own field doc; a
  workflow raising `tool_calls` for a whole bot must not be erased by a node
  that only wanted a longer `wall`. It is the per-field filling `Limits.Clamp`
  already does one rung down, so the ladder is one rule applied twice.
- **Clamped, never refused.** The parent reduces a request above `Ceiling` and
  reports it; the compiler says it earlier (**C347**), and that is all it does.
  A diagnostic that also lowered the value would give the bound two owners and
  silence `OnClamp` on exactly its own case. **C348** is bounds beside a
  `mode: off`, **C349** a duration that is not one.
- **Schema v32.** The bounds ride `ir_compiled`, as the switch does. A stale
  runner drops them and runs the DEFAULTS — tighter, and its refusal is loud —
  which satisfies the rollout runbook's first exemption condition but not its
  second: the exemption also asks that the field carry no operator choice, and
  declared bounds are the rule's own named class, *budget caps*.

### The A/B, and why it is reported as a non-result

The switch was activated on a read-only audit node — `assessment.survey`'s
shape, `tools: [read_file, glob, grep]`, no shell — on `claw` +
`openai/gpt-6-sol`, over this repository's own bot catalog pinned by commit,
two `.bot` files differing by one line, three runs per arm interleaved.

**It showed no gain.** Arm B was worse on every axis: +56% turns, +75% tokens,
+76% cost, +76% wall. And that number is not a result either — per-run spreads
were 1.5× to 2.8× within each arm, and the answer was wrong in 3 of 6 runs, so
the task sat at the model's competence limit and a share of what was measured
is the model. The honest statement is that **this measurement does not show a
gain and, at n=3 with that variance, cannot show one in either direction**. The
first pair alone read −32% tokens; the replicates reversed the sign. Spend:
$1.37.

**This is a gap in the criterion above, not only in the measurement.** That
amendment specifies what to count, which turns to exclude and three ways a run
voids itself — and says nothing about **n**. A criterion whose sign flips
between n=1 and n=3 is not yet a criterion. CM-17 owes a replicate count and a
spread it refuses to conclude under, decided before its own measurement runs.

### What the measurement did deliver

- **The 64-call default is the wall, measured.** One program surveying 39 files
  spent **40 of 64**; another attempt at the same task crossed it and came back
  *"the script has spent its budget of 64 tool calls. Aggregate in the script,
  or ask the operator to raise the bound"*. That sentence named a bound no
  author could raise, which is what CM-5d answers — the surface arrived from a
  refusal rather than from this record's reasoning.
- **Two premises of this record are refuted.** A no-shell node is not unable to
  loop: it keeps `grep`, which aggregates server-side, and claw **batches
  parallel tool calls** (2 to 4 per turn was observed). So "one call per turn",
  the phrase the tool's own description uses, is not what the baseline does,
  and the turn axis is not where codemode wins on this kind of task.
- **A second product defect, from measurement**: the tool description never
  said `call()` is synchronous, so the model wrote `await call(...)` and lost a
  program to a `SyntaxError`. Its own ticket
  ([#3311](https://github.com/SocialGouv/iterion/issues/3311)); the sentence is
  fixed in this slice because the description was already being touched.


## Amendment — 2026-11-16: the bounds measured at scale, and what that does to CM-17

The 11-15 amendment reported the small A/B and its non-result. The second
measurement ran the same read-only audit node — `assessment.survey`'s shape,
`claw` + `openai/gpt-6-sol` — at a scale chosen so the question could not be
answered by the baseline's aggregation, and in **three arms** instead of two:
arm A without codemode, arm B with codemode at the **default** bounds, arm C
with the bound **declared** (`tool_calls: 256`). B and C differ by exactly one
declaration, so that pair is immune to "grep aggregates well" and measures
this record's surface itself.

**The scale.** The corpus is this repository's Go tests: 21 478 matches across
98 top-level directories of `pkg/`. A whole-repository grep result is
**2 148 KiB against the 64 KiB per-tool-result bound**; 7 of the 98
directories overflow it on their own; the mechanical strategy — one grep per
directory plus a sub-split — costs ~130 calls. **Above the default 64, below
256**: the bound the surface declares sits exactly across the wall.

**The bound bit, measured.** Arm B hit the tool-call budget **14 times** in
three runs (one run collapsing into eight blind retries and 206 k tokens);
arm C hit it **twice**. Arm C finished **−29 % turns, −17 % wall, −39 % mean
error at equal token cost** (845 k vs 838 k). The ceiling's doc sentence —
*"ask the operator to raise the bound"* — now names a bound an author can
actually raise, which is what #3301 delivers. Control: on the pre-CM-5d
binary, arm C is a **syntax error** (`E002`/`E003`), arm B compiles — the
third arm provably exercises the new surface.

**The oracle was wrong, and the arms caught it.** The stated question counts
test functions declared `(t *testing.T)`; the first oracle also counted the
83 `TestMain(m *testing.M)` functions, so it answered 21 478 where the true
answer is **21 395**. The discrepancy surfaced because all three arms
clustered just below the wrong number — a gap too regular for noise. An
oracle is subject to the same rule as any sentence a fix writes: it executes,
or it is the next finding.

**What this measurement does NOT establish, stated as firmly**: **no arm was
exact.** The best run of arm B landed 23 off; arm C's best two were ~250 off;
and one run of B and one of C collapsed onto the **same wrong 4 161** — a
shared partial-coverage path, not noise. The task again exceeded the model's
competence, so what the scale run establishes is the bound's bite and its
cost, **not a CM-17 win for the declared surface**. Together with the 11-15
amendment the criterion gap is now precise: CM-17 owes a replicate count **n**,
a spread it refuses to conclude under, **and a reliability precondition** —
the task must be one the model completes exactly, measured before the run,
or the measurement keeps billing for the model. Spend: **$10.67** for this
run, on top of the first measurement's $1.37.

## Amendment — 2026-11-16 (bis): CM-17 measured on a synthetic corpus; the default stays opt-in

The 11-15 amendment demanded a reliability precondition before any further
measurement; the operator decided the direction the same day: measure the
MECHANISM on a synthetic corpus, where the counting task's answer is exact by
construction and the model's competence stops being the confound.

**The corpus**: 98 directories × 42 files × 5 functions = 20 580 declarations,
generated deterministically — every property the scale measurement relied on
holds: a whole-corpus grep is **744 KiB against the 64 KiB per-result bound**,
no single directory overflows it, and the mechanical strategy costs ~100
calls — above the default 64, below 256.

**Three arms, interleaved, the headline pair differing by one declaration**
(B: codemode at the default bounds; C: `tool_calls: 256`):

- **All six runs EXACT** — 20 580 total, 98 directories, per the manifest.
  The reliability precondition is met by construction; the competence
  confound is gone.
- **The bound's bite, clean**: budget refusals — 3 runs × 1 in B, **0 in C**.
  C: **−21 % turns, −31 % mean cost (1.09 vs 1.57 $), −22 % wall**, at equal
  mean tokens (527 k vs 766 k — the spreads overlap).
- Arm A (no codemode, n=1, context only): answered **1 050** — it covered 5
  of 98 directories after its whole-corpus grep hit the truncation bound,
  and said so in `method` while leaving `bound_hit` empty.
- Side lesson from #3327's first CI attempt, recorded for the next witness
  author: a real-child witness that resolves the INSTALLED binary fails in
  CI ("exec: no command"); re-execute the test binary (childHost) and assert
  what the seam owes — Clamp fires OnClamp before any spawn attempt.

**The decision**: codemode's default **stays opt-in**. The mechanism is
proven — the raised bound removes the wall and every budget refusal while
every run lands exact — but the win axes are refusals/wall/cost, **not
turns** (−21 % only), and the 11-13 abandonment criterion was written on the
turn axis. That criterion stays AS WRITTEN with this caveat now recorded:
the turn axis was refuted as codemode's win axis on BOTH corpora, so it will
never fire a flip; a flip, if ever, would be decided on the measured axes
per workload. An operator reading this record decides per workload; the
measurement is the base. Total measurement spend across the three runs of
records: **$1.37 + $10.67 + ~$8**.

## Amendment — 2026-11-17: the launch gets a rung above the DSL (CM-5e)

The bounds an author declares are static per `.bot`; the CM-17 measurement
recorded the failure shape that makes a LAUNCH-time rung necessary: a run
stops at *"the script has spent its budget of 64 tool calls … ask the
operator to raise the bound"* — and the operator had nothing to raise. The
launch request now carries the override, the strongest rung of both ladders:

- **Mode**: `--codemode on|off` / API `codemode`. It wins over node and
  workflow DSL in BOTH directions — an operator may keep a declared
  capability off for one run, and may arm it on a node whose author stayed
  silent (with the default bounds, since the author set none; a backend that
  serves no codemode tool refuses at dispatch, and the launch preview screens
  that under `route_refusals` before the run). An explicit launch choice is a
  declaration in the `Declared()` sense — the terminal refusal applies to it
  exactly as to an authored one.
- **Bounds**: `--codemode-tool-calls` and its six siblings / API
  `codemode_bounds`, field by field with the ONE rule the authored rungs
  already follow: a field the launch set wins, a field it left at zero keeps
  the authored value. `ResolveCodemodeBounds` walks three rungs with the same
  `pick` closure — "one rule applied three times rather than three rules that
  happen to meet". The platform ceiling and the 64 MiB memory floor still
  apply behind it: the launch can WARN above the ceiling (the compile-time
  twin C347 named in the warning) but never bypass a bound by asking.
- **No environment rung**, again and deliberately (ADR-132 §1): the launch
  request and the queue message (schema v33 — the `off` direction fails open
  on a stale runner, the v21 class) are the only carriers above the DSL.
- **Persisted and replayed**, the direction `docs/resume.md` names for
  ADR-based work: the launch stamps its choice on the run doc
  (`CodemodeOverride` / `CodemodeBoundsOverride`), and every resume that
  does not re-state it replays that decision — CLI, studio, cloud runner,
  retry sweeper. The sibling knobs (ambient, auto-memory) carry the
  recompute-on-purpose caveat because nothing persists them; codemode's
  launch choice is authoritative run intent for the same reason the
  permission gate's is, and a resume that said nothing must not re-arm a
  capability the operator killed at launch. An explicit resume re-state
  still wins.
- **Launch warnings**, not refusals, for the two shapes the compiler already
  warns about (C347 above ceiling, C348 bounds beside an off): the runtime
  still enforces the truth, so the launch names the compile-time twin in its
  warning instead of refusing — philosophy 1, a bound with two owners is the
  defect, not a warning.

## Amendment — 2026-11-18: the network wall is seccomp, not Landlock (#3371)

The 2026-11-10 reasoning that "Landlock is not filesystem-only" overstated what
it gives: Landlock has exactly two network rights, `BIND_TCP` and
`CONNECT_TCP`, from ABI 4. Measured with the real `Confine`, a confined child
could therefore send **UDP on every kernel**, and on the production nodes
(kernel 6.1, ABI 2, #3299) **open TCP as well**. `codemode-confinement.md`
already recorded the UDP residual as "not this slice"; the production kernel
made it the whole network.

**Decision (Jo):** the child denies itself socket **creation** with a seccomp
filter installed by `Confine` after the Landlock domain — `socket`,
`socketpair` and the `io_uring` trio answer `EACCES`, on every thread
(`SECCOMP_FILTER_FLAG_TSYNC`), and a syscall under another architecture or the
x32 ABI kills the process. The child needs no socket: its channel to the parent
is two pipes, and it refuses to run with an inherited socket, which would walk
around a creation-only filter. A socket can also be **borrowed** from another
process: `pidfd_getfd` duplicates one out of the runner, and `ptrace` or
`process_vm_writev` make the runner act. Landlock's ptrace scoping refuses all
of them from ABI 1, but the ABI-0 posture has only Yama, a host setting; so the
same filter denies those four calls too (found by revi on #3374). The filter
needs no privilege and holds on any
kernel with seccomp, ABI 0 included; it was measured to install and refuse all
of the above under docker's and containerd's default profiles with every
capability dropped. amd64 and arm64 carry rows; any other architecture fails
the confinement rather than run without the wall, and `confinementVerdict`
offers no allowance for a posture without it.

Rejected, each measured: a **network namespace per child** — the tighter wall,
refused `EPERM` under `RuntimeDefault` (`CLONE_NEWUSER` needs `CAP_SYS_ADMIN`
there; `unshare` from a running Go process is `EINVAL` anyway), so it would
cost the runner pod's own posture; a **kernel upgrade** alone — ABI 4 still
leaves UDP open; a **NetworkPolicy** — the child shares the runner's network
namespace, so a policy cannot tell them apart. The escape suite's network arms
no longer depend on the ABI and run on every lane, which retires the CI record
`ITERION_CODEMODE_ALLOW_NO_LANDLOCK_NET`.

## Amendment — 2026-11-18 (bis): the memory bound is room, not a total (#3383)

`Limits.Memory` was applied as an absolute `RLIMIT_DATA`. The shipped script
host holds ~86 MiB of data segment before the interpreter exists, because the
Go 1.26 runtime maps a 64 MiB heap arena read-write and only ~9 MiB of it is
resident. So at the 64 MiB floor the child started **above** its own bound.
Every mapping the runtime then needed was refused: a GC span-queue buffer, a
memory-profile bucket, an arena. A script pushing 10 000 strings died of a
SIGSEGV in the garbage collector 5 runs out of 5 (`script_host_died`), while
128 MiB passed.

The same class turned up as three CI flakes on 18/11. A test-binary child at
the floor died 2 runs out of 60 on `1+1`, and a test helper that lowered its
own limit to 32 MiB died 3 runs out of 40, whenever the profiler's random
sample asked for a chunk. It also explains the 768 MiB race floor the tests
carried.

**Decision (Jo):** `memory:` is the program's **growth** budget. `Confine`
reads its own `VmData`, which is `mm->data_vm`, the very counter the limit is
compared with, before the deny-all ruleset closes `/proc`. It then sets
`RLIMIT_DATA = base + Memory`, and reports the base as
`Applied.MemoryBaseBytes`. This is the meaning the parent's resident-set probe
already had (growth from a baseline, #3299). The floor, default and ceiling
keep their numbers and now mean room, whatever the binary weighs. Measured on
the shipped binary at the floor: the 10 000-push script completes, and a memory
bomb dies with the runtime's own `out of memory`, the typed `memory_exceeded`.
The race floor is retired: the race suite passes at 64 MiB of room, three runs
out of three.

Consequence for sizing: a child can hold its base plus `memory:`, ~86 + 256 MiB
under the default. The base is mostly reserved, not resident, so a pod budget
should count the resident base (~9 MiB) plus the room per concurrent child.
`Ceiling().Memory` still caps the room a tenant may ask for.

## Amendment — 2026-11-20: discovery is an index in the description, and two methods on `call` (CM-3, #3443)

The consequence above promised "one tool plus a discovery index". CM-3 builds
it.

**What the model reads.** The codemode tool's description is a constant
preamble plus the node's **index**: one line per tool a program can call,
sorted by name, giving its top-level parameters and the first sentence of its
description. The description sits in the cached prefix, so it is a pure
function of the node's script-callable tools and holds nothing per turn.

**What a program asks for.** `call.describe(name)` returns the tool's
TypeScript declaration (an overload of `call`). `call.search(query, limit)`
ranks the tools by BM25 and returns `{name, summary, declaration}` for each
hit. Both are methods of the one global, as CM-1b's binding requires. The
child answers them from a catalogue the parent sends with the request, so
neither is a tool call. The catalogue is bounded by `toolindex.Bound`, applied
inside `Host.Run`, and `MaxRequestBytes` reserves room for it. `Proto` moves
to 2.

**One predicate, one snapshot.** The index, the catalogue and the dispatch pin
all read `scriptWithheld`, over the `opts.Tools` snapshot taken at install. A
tool the door refuses therefore cannot appear in the index.

**Decision (operator, 2026-11-20)** — chosen over two alternatives:

- inlining every declaration in the description: it grows with tool count ×
  schema size, contradicts the declaration budget above, and leaves CM-14
  nothing to defer;
- a second model-facing search tool: a second tool in the budget, and a
  duplicate of claw's `tool_search`.

The details, the bounds table and the 11 mutants seen red are in
[codemode-discovery](../agents/backends/codemode-discovery.md).

## Amendment — 2026-11-21: explicit, bounded tool batches (CM-9/CM-10)

`call.parallel` is a synchronous join over a short batch, not a new global or
an event loop. The parent reserves the call budget before dispatch and runs
only explicitly audited tools concurrently. The zero-value tool capability
is exclusive; `concurrent_tool_calls` defaults to one and is clamped to four.
Batches preserve input order even when work completes out of order. The same
node dispatcher still applies scope, permission and lifecycle hooks.

The launcher keeps reading events and session captures while tools run, with
bounded admission and reply queues. Placement and policy remain its decisions;
the runner does not attest concurrency eligibility. This does not certify
codemode's sandbox crossing (CM-6). See [the batch contract](../agents/backends/codemode-concurrency.md).
