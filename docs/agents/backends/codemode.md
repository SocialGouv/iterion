# Codemode — the model writes code that calls the node's own tools

One tool whose argument is JavaScript. It runs harness-side, in a child process
that holds no credential, and its only capability is calling the tools the node
already declares. The script loops, filters and aggregates where the model's
context would otherwise carry every declaration and every result; what comes
back is what the script chose to return.

**Read the decision first: [ADR-165](../../adr/165-codemode-runs-the-models-code-in-a-child-process-behind-the-tool-door.md).**
This page is the operational half — what is built, what is not, and the
contract anything new has to honour.

## What is built, as of 2026-11-10

Exactly one piece: **the door**. `toolDispatcher` in
[pkg/backend/model/generation_toolexec.go](../../../pkg/backend/model/generation_toolexec.go)
holds a node's dispatch state as a value and `dispatch(ctx, block)` *is*
`executeOneTool`, so a nested call takes the pipeline the model's own loop
takes — lifecycle hooks, the `allowed-tools:` command scope,
`permission.Policy`, secret materialization, the events, the `tool_search` pin.

**Nothing of the runtime exists.** There is no script host, no JS execution, no
DSL attribute, no bound, no bench. A grep for `__script-host`, `tsdecl` or
`toolindex` returns nothing, and the two occurrences of the word "codemode" in
Go are comments pointing here. Treat any claim that codemode works as false
until this section says otherwise. Epic [#3153](https://github.com/SocialGouv/iterion/issues/3153);
the in-pod bench that will declare it usable is [#3155](https://github.com/SocialGouv/iterion/issues/3155).

## The contract — a host dispatches with the context the dispatcher vouches for

`newToolDispatcher(ctx, …)` returns the dispatcher **and** a context, and
`dispatch` refuses any context that does not descend from it. That refusal is
the gate's own shape: a synthetic `isError` tool_result, not a panic and not a
silent pass.

It exists because four per-node facts ride the context rather than the struct,
and **three of them fail open**:

| fact | absent from the context ⇒ |
|---|---|
| the `allowed-tools:` command scope (#1717) | nothing is narrowed |
| the child-conversation Ask degradation (#2877) | a child's Ask pauses the whole run instead of degrading |
| the parent policy a child inherits | a child spawned through `agent` is **ungated entirely** |
| the parent loop shape bounding a child's tools | the child falls back to the run's whole registry |

`tool.HostCapabilitiesFrom` is the fifth and the exception — it fails closed,
and it is the precedent the guard follows.

Two things the guard does **not** do, because they have bitten already:

- It checks **provenance, not content**. A dispatcher built from a factless
  context vouches for a factless context. Supplying the node execution's
  context to the *constructor* is the caller's job; on the model loop's own
  path `GenerateText` guarantees the scope (no generation runs scopeless).
- Capturing those facts and re-pinning them inside `dispatch` was tried and is
  **wrong**: writing a captured scope over a caller's live one erases a
  narrowing that was present, and writing a captured "asks allowed" over a
  child's context restores the pause #2877 removes. Both pass the whole suite.
  Do not reintroduce it.

A second path into the tool-running funnel is refused mechanically:
`toolFunnelCallers` in
[generation_toolexec_callers_test.go](../../../pkg/backend/model/generation_toolexec_callers_test.go)
pins `executeOneTool` to `dispatch` and `runToolExecution` to `executeOneTool`.
A lot that wants its own path has to widen that map and say which controls it
holds instead.

## Confinement comes from the kernel, not the interpreter

goja cannot bound itself. ADR-098 F19 closed on the measurement:
*"goja's interrupt does not preempt native calls and bounds no heap"*. A native
regex bomb hangs the runner, not the script. So the host is a **child process**
and the limits are applied before the interpreter exists — `RLIMIT_AS` /
`RLIMIT_DATA` for a real memory cap, `RLIMIT_CPU`, `no-new-privs`,
non-dumpable, a parent-death signal, and Landlock in deny-all where the kernel
offers it.

**We already own the host half.** claw-code-go — first-party, vendored, and
being folded into this tree — executes model-authored JavaScript today:
`internal/runtime/workflow.go` runs a prelude and then the script on a goja
runtime over a `goja_nodejs` event loop, with a resolve/reject bridge and caps
(512 KiB of source, 15/60 minutes, 200 `agent()` calls). No iterion node can
reach it: `workflow` is a claw tool iterion does not advertise.

Two differences are the whole of CM-1's work, so read them before writing any:
that host binds **`agent`**, so its scripts orchestrate sub-agents and never
touch the node's tools; and it runs **in process**, so its caps are exactly the
engine-level ones F19 says are unenforceable. CM-1 moves that machinery into a
child process under kernel limits and swaps `agent` for a `tools.*` binding
backed by `dispatch`. A seam that crosses claw is improved in its `.works/`
worktree and bumped — never forked here.

`GOMEMLIMIT` is a GC target. It is not a cap, and it is not an answer here.

Where Landlock is missing on an in-pod runner, codemode is granted only to a
node that **already** holds a `class:shell` tool — it adds no filesystem reach
that node lacks — and is otherwise refused as `codemode_unavailable`. That
fallback is a judgement about filesystem reach only: a shell tool is bounded by
its own timeout, a script by the bounds above, so if the bench cannot hold the
memory axis without Landlock the fallback becomes a refusal.

## The cloud posture, which is not the one people assume

Production runs are **not** sandboxed. `charts/iterion/values.yaml` ships
`runner.sandbox.enabled: false` and the configmap renders
`ITERION_SANDBOX_OVERRIDE: "none"`, saying why: *"pod is the isolation
boundary"*. So the script host, the bash a node already holds and the runner
are siblings in one pod under one uid.

Two named topologies: **in-pod** is the default and the one that must be proven
first; **sibling pod** is the stronger form and a deployment choice, not a
per-bot one. The sandbox crossing is a later lot, for the sandbox pools and the
ADR-162 fork pool — not the default posture.

This is also why the first bench is in-pod: a bench on the sandbox crossing
would prove a posture nobody runs.

## What a script may not do

- **Pause the run.** `ask_user` inside a script is a typed refusal naming the
  direct call as the way to ask. A deferred tool whose verdict can be `Ask`
  would otherwise be reachable only from a place that cannot honour it.
- **Choose a child's model**, or spawn children without a finite reserve booked
  before they run.
- **Hold a credential.** Its environment and its `/proc` carry none.
- **Survive a restart.** Journal-and-replay depends on mid-node resume
  ([#1403](https://github.com/SocialGouv/iterion/issues/1403)) and is out of
  v1: a script is re-run, never resumed.

## Bounds

Every bound has an operator override **and** a platform ceiling the tenant
cannot raise — philosophy 1 forbids a limit with no escape hatch, cloud
multi-tenancy forbids an escape hatch with no ceiling. The precedent to copy is
`applyCloudBudgetCeiling` in [pkg/runner/loop.go](../../../pkg/runner/loop.go),
which clamps a workflow's budget to a ceiling the bundle cannot lift.

The axes: wall clock, CPU time, address space, bytes returned to the model,
tool calls per script, concurrent tool calls, nested children per invocation
and per node, and `store()` entries and bytes per run. Values are set with the
lots that enforce them, and each ships a witness on **both** arms — refused
above the bound, allowed below — because a false refusal in a blocking path
costs as much as a hole and is harder to see.

## Two traps worth knowing before you touch the guards

Both were paid on this seam, and both make a green test meaningless:

- **A mutation bench reads the whole package.** Measuring which tests a mutant
  reddens under a `-run` filter hides the characterization suite: a claim of
  "only test X reddened" was 1 where the truth was 2 and 21. Publish the
  mutant × witness matrix, not a sentence.
- **`go test -overlay` cannot mutate a guard that reads the filesystem.** The
  census walks `os.ReadDir(".")`, so an overlay hands it a file the walk never
  sees and the mutation comes back **falsely green**. Write the probe to disk,
  run, remove it, and check `git status`.
