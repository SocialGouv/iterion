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
2. **The engine cannot bound itself.** ADR-098's F19 (`:713-715`) closed on exactly this: *"goja's interrupt does not preempt native calls and bounds no heap: transforms stay refused until the limits are enforceable"*. goja is vendored, but only as a **parser** — `pkg/botimport` imports `goja/ast` and `goja/token`, and nothing in the tree calls `goja.New()`, `RunString` or `RunProgram`. Codemode introduces JS *execution* here for the first time, with no confinement pattern to inherit.
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

**The host.** A hidden subcommand, `iterion __script-host`, in the shape ADR-029 established for `__claw-runner`: a child process speaking NDJSON over stdio. It holds one host binding — "call a tool" — plus an embedded JS prelude. Values cross as **JSON only**; no Go object is reflected into the VM. `SetRandSource` and `SetTimeSource` are pinned, the clock is UTC, and there are no timers, so a script is deterministic given its inputs and its tool answers.

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

- **In-process goja with interrupt-based bounds.** Rejected by F19 on the evidence that closed it: the interrupt does not preempt a native call and bounds no heap. A native regex bomb would hang the runner, not the script.
- **A WASM runtime (QuickJS-in-WASM, as Pi uses).** Defensible and stronger on memory, rejected for v1 on two grounds: it adds a toolchain and a second language runtime to an image that already ships one, and the kernel primitives above give a real cap without either. Revisit if the in-pod bench cannot hold the memory axis.
- **Granting the script a credential so it can call a model directly.** Refused: it would put a live key beside model-authored code, and the sub-agent path already exists behind the dispatcher with its own accounting.
- **Letting a script pause the run.** Refused: a pause is a conversation-level event and a script is not a conversation; the deadlock in the deferred-tool case is the concrete reason.
- **Proving cloud readiness on the sandbox crossing first.** Rejected because it proves the posture nobody runs (fact 1). The crossing is CM-6, after the in-pod bench.

## Watch

- **The Landlock fallback is a judgement, not a proof.** "A node that already holds `class:shell` gains nothing" is true of filesystem reach and false of CPU and memory: a shell tool is bounded by its own timeout, a script by the bounds above. If the bench shows the memory axis unenforceable without Landlock, the fallback becomes a refusal.
- **`store()` is durable state and owes its cloud twin in the same change** (philosophy 3), including the `DeleteRun` sweep and a shared conformance case.
- **The tool-call quota is counted launcher-side.** External-effect tools are exempt from the permission gate today (`executor_build_task.go`, `permission.go`), so an injected line that can place one card can place a hundred; the quota that bounds that has to be counted where the launcher sees it, not where the runner reports it.
