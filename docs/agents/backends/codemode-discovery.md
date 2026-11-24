# Codemode discovery — the index the model reads, the declarations a program asks for

How a model learns which tools a codemode program can call, and how the program
reads their signatures (CM-3, #3443, ADR-165). The code is
[pkg/scripthost/toolindex](../../../pkg/scripthost/toolindex), the child binding
in [toolcall_child.go](../../../pkg/scripthost/toolcall_child.go), and the
parent's composition in
[generation_codemode.go](../../../pkg/backend/model/generation_codemode.go).

## What the model reads

The codemode tool's description is a constant preamble followed by the node's
**index**: one line per tool a program can call, sorted by name, each naming the
tool's top-level parameters (optional ones marked `?`) and the first sentence
of its description.

```text
Tools a program can call:
- list_dir {path?}: List a directory.
- read_file {path, offset?}: Read a file from the workspace.
```

The description sits in the cached prompt prefix (`buildRequest` marks the tool
array). So it is a pure function of the node's script-callable tools: no limit,
counter or workspace detail, and nothing that depends on registration order.
Two turns of one node read the same bytes. `TestTheCodemodeDescriptionIsByteStable`
holds it.

## What a program asks for

Two methods hang on `call` itself, so `call` stays the program's only global
(the rule `toolBridge.install` states: the script surface is in the cache
prefix):

- `call.describe(name)` returns the tool's TypeScript declaration, the overload
  of `call` a program writes against:
  `declare function call(name: "read_file", input: { path: string; offset?: number }): Result;`.
  An unknown name throws a `TypeError` that points at `call.search`.
- `call.search(query, limit?)` ranks the tools by BM25 and returns the best
  `limit` matches (default 5, at most 20), each `{name, summary, declaration}`.
  The ranking weighs the name, the description, and the parameter names and
  descriptions. A query that spells a tool's exact name puts that tool first.

Neither method is a tool call. The child answers both from the catalogue the
request carried, so discovery costs the parent nothing and spends nothing
from `tool_calls`.

## One predicate, one snapshot

The index, the catalogue and the door all derive from the same two things.
The **snapshot** is `opts.Tools` taken when `installCodemode` runs, after the
list is final and before codemode joins it. The **predicate** is
`scriptWithheld`, the withheld class that the script tool server refuses by
and the dispatch pin narrows by. So the index cannot list a tool the door
refuses (`TestTheIndexListsWhatTheDoorServesAProgram`), and a withheld tool is
not discoverable from a program either.

## The defer split (CM-14, #3450)

A node that declares `codemode: defer: on` (the DSL block property, resolved
node-over-workflow like `mode:`) moves its **author-declared** tools out of
the request's tool array and into the catalogue: `buildRequest` — the flag's
only visibility reader — skips them on the wire, while the tool map, the
door, the index and the catalogue keep serving them. The program calls one by
name; `call.search` finds it; `call.describe` returns its declaration; the
model sees none of its bytes. The rule is `author-declared AND not
withheld AND not runtime-appended`: the runtime's own appends (the ask
channel, `todo_write`, the board/runs/watch families, sub-agents, memory) and
the withheld classes stay declared whatever the deferral says — an author
entry the runtime would append anyway included, through one record
(`runtimeAppends`) the append site writes and the marker reads — so the node
never loses its posture or its way back to the operator. Two more vetoes
keep the deferral from ever removing reach, both read in `installCodemodeOn`
between the one catalogue bound and the description:

- **An Ask-able tool is never deferred.** Inside a program an Ask degrades
  to a refusal, so a tool the node's policy can ask about keeps its direct
  call: `(*permission.Policy).CanAskAbout` on the tool_use name, against
  `opts.Permission` — the object the dispatcher gates with (ask mode, ask
  rules, argument-scoped ones included). Under `permission: ask` nothing is
  demoted, and no complement is paid.
- **Only what the bound keeps whole is demoted.** `toolindex.Bound` leaves
  out tools past 256 and over-long names, and drops oversized schemas; such
  a tool stays declared, so it is never off the wire AND out of the index.

A child conversation does not inherit the split (`filterSubagentTools`
clears it): it has no program. A direct call naming a deferred tool still
executes. The
description gains one constant complement sentence where at least one tool
was demoted — a pure function of the demoted set, byte-stable like the rest
of the prefix — and claw's `tool_search` (whose catalogue is wider than
any node's list) marks a catalogue-only match `program_only` without its
schema: the declaration is what `call.describe` returns inside the program,
not bytes the conversation re-spends.

Measured on the real in-process families through the real registry
(`TestTheDeferralMeasuredOnTheRealFamilies`): the deferral pays when the
author's tools carry real schemas — a hundred synthetic 1 KiB-schema tools
shrink the wire by 93 % — and can cost a few hundred bytes on a node whose
author tool is a bare builtin, where the complement sentence plus the index
entry outweigh the removed schema-less entry. It is additive on the compiled
form: a consumer built before the field reads nothing and serves every tool
declared, which is the same fallback a runner gets — the field does not
cross the sandbox envelope, so a sandboxed node runs today's behaviour by
construction, and the build warns once per node. On iterion.cloud that
means every bot of a source the isolation floor sandboxes: the deferral is
an unsandboxed-only optimization today.

## The catalogue bounds

The catalogue crosses into the child inside the request document. The child
reads that document before it is confined, so the catalogue has a bound of its
own, and `MaxRequestBytes` reserves room for it. `Host.Run` applies
`toolindex.Bound` itself: no caller can send past it.

| bound | value | past it |
|---|---|---|
| tools | 256 | the rest stay callable; the index says how many it does not list |
| name | 256 bytes, encoded | the tool is left out of the catalogue and counted the same way |
| description | 2 KiB, **encoded** | cut on a rune boundary, with `…` |
| one schema | 32 KiB | omitted; the declaration says how large it was |
| all schemas | 1 MiB | the largest are omitted first, ties broken by name |
| encoded catalogue | 2 MiB | an error: the bounds above leave far less, so reaching it means a bound is wrong |

Descriptions are bounded on their JSON encoding, not their raw bytes: escaping
grows a control character six-fold. Mutant M4 (a raw-byte bound) made a
worst-case catalogue encode to 4.25 MB.

## The declarations

The renderer is tolerant by contract. A schema comes from whoever wrote the
tool, an MCP server included, so whatever it does not follow becomes `unknown`
at that point, never an error and never a dropped tool. It handles:

- local `$ref`s, rendered as `type` aliases registered **before** their body
  is rendered, so a recursive schema renders as a recursive alias and
  terminates;
- 2020-12 `prefixItems` and `$defs` beside their draft-07 forms, plus `const`,
  `enum`, type arrays, OpenAPI's `nullable`, `anyOf`/`oneOf` unions,
  `allOf` intersections, and maps from `additionalProperties`;
- a non-object input, which renders as its own type.

Two more rules keep the output safe and small. Text that would close a doc
comment is escaped. A declaration over 16 KiB is rendered again, shallower.

## Witnesses

There are 11 mutants, each seen red on its own assertion:

| mutant | test that turned red |
|---|---|
| first registration kept | `TestAToolRegisteredTwiceIsDescribedAsItsLastRegistration` |
| index not sorted | `TestNewIndexesInNameOrderWhateverItIsGiven` |
| smallest schema dropped first | `TestTheSchemaBudgetDropsTheLargestFirst` |
| raw-byte description bound | `TestTheWorstCaseCatalogueEncodesUnderItsBound` |
| alias registered after its body | `TestACyclicRefRendersAsARecursiveAlias` |
| no comment escape | `TestADescriptionCannotCloseItsComment` |
| no exact-name boost | `TestSearchPutsAnExactNameFirst` |
| catalogue keeps withheld tools | `TestTheIndexListsWhatTheDoorServesAProgram` |
| install drops the catalogue | `TestAProgramDiscoversTheNodesToolsThroughTheDoor` |
| `Run` sends the catalogue unbounded | `TestRunBoundsTheCatalogueItSends` |
| the child builds no index | `TestAProgramDiscoversAToolAndCallsIt` |

The last three run through the real child, and the last two through the
shipping binary.

`Proto` is 2. A version-1 child would run a program that uses discovery into
"not a function", so the handshake refuses it as a different build instead.

## Decision and rejected alternatives (operator, 2026-11-20)

The choice is **an index in the description, plus `describe`/`search` in the
program**. Two alternatives were rejected:

- **Every declaration inlined in the description.** The description would grow
  with tool count × schema size, which contradicts ADR-165's "one tool plus a
  discovery index". It would also leave CM-14 (deferred tools, #3450) nothing
  to defer.
- **A separate model-facing search tool.** It would add a second tool to the
  declaration budget and duplicate claw's `tool_search`, giving two answers to
  one question.
