# Codemode — what the run view shows

## What the run view shows

A program has no event type of its own. The run's ordinary tool events carry
it, and the studio's Tools tab reads those — no journal beside them, no second
store. Three event shapes, written by the store hooks in
[pkg/backend/model/hooks.go](../../../pkg/backend/model/hooks.go):

| Event | Carries |
|---|---|
| the `codemode` call's `tool_started` | The program: its `input` is `{"source": …}`, stored and redacted like any tool input — inline up to 4 KB, a sidecar blob past it, read with load more. |
| each nested call's `tool_started`, `tool_called`, `tool_error` | `tool_use_id` = `codemode_<26 chars>_<frame>`, the frame being the child's own request number — program order, a run-state request spends one too. `parent_tool_use_id` = the program's call. `batch` = `{first, size, index}` for a `call.parallel` entry. |
| the `codemode` call's `tool_called` or `tool_error` | `codemode`: `limits` (the bounds the host REQUESTED, after the clamp, by wire name), `adjusted` (`{field, requested, applied}` per bound the clamp moved, floor or ceiling), `confinement` (what the child's kernel ENFORCED — `cpu_seconds`, `memory_bytes`, `memory_base_bytes`, `landlock_abi`, `sockets_denied`, `all_threads`; a deployment's stricter rlimit wins, CPU rounds up to a whole second), `effective_wall_ns` (the wall the program really had when the node's deadline was nearer than `wall_ns`), `refusal` (the scripthost code), `calls` (requests the parent served, run-state included), `budget_refused`, `state_calls`, `source_bytes`. Keys with nothing to say are omitted; `refusal` is absent on success and on a host error, which the event's `error` names; `confinement` is absent when no child reported one; the child states it
before its script runs (scripthost Proto 6), so a child a deployment's
stricter bound kills mid-script still reported it. A call whose input does not decode as a program, or that found no door, ends before any program ran and carries no `codemode`. |

Every refusal of a nested call — withheld, unknown tool, hook-blocked,
narrowed, command-scoped, refused on the call budget by the parent — is a
`tool_error` with its own id and its parent. On a sandboxed node, the
launcher's refusal of a forwarded call is one more `tool_error` beside the
runner's, marked `emitted_by: launcher`, under the id and parent the runner
sent on the `tool_call` envelope (`ToolCallData.tool_use_id`,
`parent_tool_use_id`). Both are the runner's CLAIM: the mark is set by the
launcher alone — the relayed path cannot carry it — and no reader merges
the event into another, takes it as a parent, or reads the input remembered
under its id. An older runner sends neither, and its refusal stays id-less
and top-level. Not
visible: a call the child refuses itself once its budget latched never
crosses the channel, and discovery (`call.search`, `call.describe`) is
child-local and free.

The studio groups by `parent_tool_use_id` and nothing else
(`groupToolCalls`, `studio/src/components/Runs/detail/toolCallModel.ts`): an
event without the key — a call the model made, or any event older than the
field — stays flat, never regrouped by the shape of an id or by order, and so
does a nested call whose program is outside the window, names an id held
by several calls (a provider reusing `call_0` across steps), or sits on a
loop of parents only a forged stream makes: no view drops a call. A
completion pairs with the one open start of its id, never with one of
several. The launcher's refusal is its own entry, under the parent it names
when that resolves, its text intact. The in-flight footer closes a start by
its own id only, never on the launcher's refusal; the chat banner counts the
calls the model made. `iterion inspect node` nests a program's calls under
it, at any depth, by the same rules, with one summary line per program.

A sandboxed node's events cross the launcher relay with every field above. A
program over the relay's 1 MiB field budget crosses as
`{"_iterion_sandbox_relay_omitted_bytes": N}`, which the card shows as not
carried, with its size.

`llm_request.tool_count` counts the node's tools, never the synthetic
structured-output tool: on the tool loop's requests that is what went on the
wire; on a structured-output request, which strips the node's tools and sends
the synthetic one alone, it is still the node's count — so a node with no
tools reports 0. A deferring node leaves its catalogue-only tools out of it
and adds `catalogue_only_count` beside it.

Witnesses: [codemode_inspect_test.go](../../../pkg/backend/model/codemode_inspect_test.go),
the census in [generation_toolcall_report_test.go](../../../pkg/backend/model/generation_toolcall_report_test.go).
