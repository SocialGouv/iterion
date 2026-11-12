# Codemode — the model writes code that calls the node's own tools

One tool whose argument is JavaScript. It runs harness-side, in a child process
that holds no credential, and its only capability is calling the tools the node
already declares. The script loops, filters and aggregates where the model's
context would otherwise carry every declaration and every result; what comes
back is what the script chose to return.

**Read the decision first: [ADR-165](../../adr/165-codemode-runs-the-models-code-in-a-child-process-behind-the-tool-door.md).**
This page is the operational half — what is built, what is not, and the
contract anything new has to honour.

## What is built, as of 2026-11-12

Three pieces. **The door**: `toolDispatcher` in
[pkg/backend/model/generation_toolexec.go](../../../pkg/backend/model/generation_toolexec.go)
holds a node's dispatch state as a value and `dispatch(ctx, block)` *is*
`executeOneTool`, so a nested call takes the pipeline the model's own loop
takes — lifecycle hooks, the `allowed-tools:` command scope,
`permission.Policy`, secret materialization, the events, the `tool_search` pin.

**The confined host**: [pkg/scripthost](../../../pkg/scripthost) plus the
hidden `iterion __script-host` sub-command. A caller hands it JavaScript, it
runs in a child process with an empty environment under kernel bounds, and one
JSON document comes back. Its bounds, the kernel mechanism behind each and the
witness that was seen to fail without it are in
[codemode-confinement](codemode-confinement.md) — which also records the five
statements measurement refuted, three of them in this page.

**And the binding between them**: descriptors 3 and 4, newline-delimited JSON,
one call in flight. The child writes a call, the parent dispatches it through
`d.dispatch` and writes the result back; a script sees one host function,
`call(name, input)`, blocking, because a bare `goja.Runtime` pumps no job queue
and a promise it returned would never settle. `scriptToolServer` lives in
`pkg/backend/model` because it must: the dispatcher, its context key and its
fields are unexported, so a broker outside would need the funnel widened —
which is giving the funnel a second door.

Measured against the shipping `CGO_ENABLED=0` binary: a five-iteration loop
crossed the pipe five times and returned **one** number — compose outside the
model's context, return the distilled result.

**No model can reach it yet, and the compiler says so.** `codemode: on|off`
is declared on an agent, a judge and a workflow and carried to the IR — and
read by nothing: no capability cell, no registered tool. The compiler warns
**C338** on an explicit `on`, and such a node runs exactly as it did without
the line. Treat any claim that codemode is *usable* as false until this
section says otherwise; the slice that registers the tool deletes that
warning, not the attribute. Epic
[#3153](https://github.com/SocialGouv/iterion/issues/3153) (slices #3175,
#3182, #3195); the in-pod bench that will declare it usable is #3155.

### Four decisions the binding owes its reviewers

- **The script source keeps its `{{placeholders}}`.** The script host is in
  `keepsPlaceholders`, so a model writing `{{SECRET}}` does not get the
  cleartext substituted into the text that crosses the pipe into a child built
  to hold no credential. Each nested call is materialised at **its own**
  dispatch instead, with `rewrite.Off` set for that call alone rather than for
  the whole script.
- **The withheld set is a CLASS, not a list of names.** `permission.IsInfrastructureTool`
  keys on the namespace and normalises every separator spelling, so the same
  tool cannot be reached as `ask_user`, `askUser`, `mcp__iterion__ask_user` or
  `mcp.iterion.ask_user`. Withheld: the pause family (a script cannot wait for
  an operator), `send_user_message` (permission-exempt, so the call budget
  would be its only bound), `structured_output` (it answers for the node), the
  `mcp_iterion` control surface, `agent` (a sub-agent needs its own reserve) and
  the script host itself.
- **Discovery answers with the set the script will be served.** `dispatch`
  applies a narrowing the caller declared, so `tool_search` cannot offer a
  script a tool the server then refuses — two answers to one question is how a
  model learns to retry a call that can never succeed. The filter is derived
  from the predicate that refuses, so the two cannot drift.
- **The call budget is the PARENT's.** The child counts too, to save a round
  trip, but a child is what is being bounded and cannot be what bounds. An
  over-budget call never reaches the server, so no hook fires and no permission
  verdict is computed for it.

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

**The binding list is not the surface**, which is why the host was written
rather than borrowed. claw's own `internal/runtime/workflow.go` runs
model-authored JavaScript on a `goja_nodejs` event loop, and
`eventloop.NewEventLoop()` installs `require`, `console` and the timers by
default — measured, `typeof require` is `function` and
`require('<path>.json')` reads a file off the host filesystem. So "no Go object
is reflected in" and "a script is deterministic" are properties
[pkg/scripthost](../../../pkg/scripthost) **builds** on a bare `goja.New()`;
they are not inherited. A seam that crosses claw is still improved in its
`.works/` worktree and bumped — never forked here.

`GOMEMLIMIT` is a GC target. It is not a cap, and it is not an answer here.

Where Landlock is missing on an in-pod runner, codemode is granted only to a
node that **already** holds a `class:shell` tool, and is otherwise refused as
`codemode_unavailable`. Two corrections to the reasoning that wrote that rule:
Landlock costs nothing to adopt — `golang.org/x/sys/unix` is already a direct,
vendored dependency carrying the syscalls and every constant, no cgo — and it
is **not filesystem-only**. ABI 4's `LANDLOCK_ACCESS_NET_CONNECT_TCP`, also
already vendored, lets the child deny its own TCP connect unprivileged. So
where Landlock is present a script holds a network guarantee a shell tool does
not, and where it is absent it loses one: the fallback is re-argued against the
bench's numbers, not against the shell comparison alone. And nothing in this
repository mentions gVisor or `runsc` — no part of the confinement comes from a
runtime class here.

## The cloud posture, which is not the one people assume

Production runs are **not** sandboxed. `charts/iterion/values.yaml` ships
`runner.sandbox.enabled: false` and the configmap renders
`ITERION_SANDBOX_OVERRIDE: "none"`, saying why: *"pod is the isolation
boundary"*. So the script host, the bash a node already holds and the runner
are siblings in one pod under one uid.

In that posture `__script-host` is also the **first** credential-less child
iterion spawns: the `__claw-runner` precedent is started only when a run is
sandboxed, which production never is. ADR-029 is the template for the IPC and
the hidden-subcommand convention, not for the spawn — and `SysProcAttr` carries
no rlimit field, so the child applies its own limits after `exec`.

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
