---
name: assessment-survey
description: What a survey declaration is, the kinds it may take, the evidence each kind owes the lint, and the machine-readable extractor block a stack-<id> skill carries. Read before declaring anything an assessment will publish.
---

# The survey, and why a declaration is a number

The assessment measures a repository in two layers. The **floor** is agnostic
and always runs: it reads git objects at one pinned commit and counts what
every repository has — files, lines by extension, history, tags, the presence
of continuous integration. It needs no knowledge of any stack and it must
produce output on any tree; when it does not, the tool failed.

Everything above the floor needs a judgement no walk can make: which files are
first-party and which are vendored, what counts as a deployable, which module
is a test harness rather than a product. That judgement is the **survey**, and
it is where you come in.

Here is the thing to understand before writing a single entry, because it
decides the shape of every rule below.

**A declaration is not documentation. It is an input to an arithmetic whose
result is published.** Measured on this bundle's own fixture: declaring ONE
deployable four times — repository untouched, evidence unchanged, every
consistency check green — moves the size index from 0.658 to 0.931, a third of
the way up the scale, and across a band wherever the repository sits near one.
Nothing lied. A number was simply counted four times.

So the lint refuses, rather than warns, on four things: a declaration whose
evidence it cannot re-verify at the pinned commit, two declarations naming the
same artefact, two declarations whose file sets overlap, and a file that no
declaration and no exclusion accounts for. And there is one failure no lint
can catch, which is why this skill exists: **omission**. A deployable you never
declared has no declaration to refuse. Nothing goes red. The document simply
describes a smaller project than the one in front of you.

## What you return

```json
{
  "stacks": [
    {"id": "<stack id>", "evidence": "<path that proves it>",
     "supported": true, "reason": ""}
  ],
  "declarations": [
    {"id": "<canonical id>", "kind": "<kind>", "path": "<path in the tree>",
     "pattern": "<optional regexp>", "note": "<one line>"}
  ],
  "notes": "<one paragraph: what you could not establish, and why>"
}
```

`stacks` is an OPEN list. Name every stack you see, including the ones this
bundle ships no `stack-<id>.md` for: mark those `"supported": false` with the
reason, and the coverage gate reports them as **unsupported**. Unsupported and
zero are different results — a stack measured at zero and a stack never
measured read identically in a table, and only one of them is a fact about the
repository.

## The kinds, and the evidence each owes

| kind | what it declares | evidence the lint re-verifies |
|---|---|---|
| `deployable` | one artefact that is deployed and runs on its own | `path` exists at the pinned commit; `pattern` matches inside it when given |
| `system` | one distinct system the application talks to — a datastore, a broker, an external service | `path` exists; `pattern` matches |
| `first_party` | a subtree that IS the product's own source | `path` is a directory in the tree |
| `excluded` | anything that is NOT first-party — vendored, generated, locked, data, fixtures, documentation | `path` is a file OR a directory in the tree, and `note` says which of those it is |
| `tests` | a subtree that holds tests | `path` is a directory in the tree |
| `entrypoint` | a declared way in — an HTTP surface, a CLI, a scheduled job, a queue consumer | `path` exists; `pattern` matches when the count comes from a pattern |

**Every top-level entry of the tree must be claimed** by one of `first_party`,
`excluded` or `tests` — files at the repository root included. The lint refuses
a survey that leaves one unclaimed, and the reason is in the next section: it
is the only mechanical handle anybody has on omission.

`id` is lower-case, dash-separated, stable, and unique across the whole list.
Stable means: the same repository surveyed twice yields the same id for the
same artefact. An id derived from the path is usually the right answer; an id
carrying a count, a date or a version is never one.

## Three ways a survey goes wrong, all of them quiet

- **Declaring the same thing twice under two spellings.** A service declared
  once by its directory and once by its manifest file is one deployable
  counted twice. The lint catches the exact duplicate; it catches the
  overlapping file set; it cannot catch two declarations of the same *concept*
  whose paths are disjoint. That one is yours.
- **Declaring a subtree first-party because it compiles.** Generated code,
  vendored dependencies and lock files all compile. If a file declares itself
  generated in its own header, it is not first-party, and counting it inflates
  the lines and therefore the published size.
- **Declaring nothing where you saw nothing.** An empty `entrypoint` list on a
  repository that clearly serves traffic is the omission failure. Say so in
  `notes`: "entrypoints not established — the routing appears to be configured
  at runtime from <path>, which this survey cannot resolve". A stated gap is
  worth a great deal; an unstated one is worth less than nothing, because the
  document reads as complete.

## The extractor block a `stack-<id>.md` carries

A stack skill ends with a machine-readable block, plus one fenced script per
extractor. The workflow — not you — reads them for every stack you named, runs
each script with `$WORKSPACE_DIR`, `$SCRATCH_DIR` and `$BASE_SHA` in the
environment and cwd at the workspace, captures its standard output into
`output`, and then verifies that the file exists and parses as JSON. A script
that exits 0 and writes nothing is a silent coverage gap, which is why the
artefact is what gets checked rather than the exit code.

````
<!-- iterion:extractors
[
  {"id":"<extractor id>",
   "output":"<file name written under $SCRATCH_DIR>",
   "emits":["<fact key this extractor is responsible for>"],
   "interpreter":"python3"}
]
-->

<!-- iterion:script <extractor id> -->

```python
# writes one JSON document to standard output
```
````

The script lives in the fenced block that FOLLOWS its anchor. An extractor
declared in the spec block with no script block after it is an error the
runner reports by name — never a silently skipped extractor.

The same block is the coverage gate's expectation, which is what keeps the
per-stack check in lockstep with what actually ran without a list of languages
anywhere in the workflow. **Adding a stack is dropping a `stack-<id>.md`
file** — there is no DSL edit in that sentence, and if you ever find yourself
wanting one, the design has gone wrong.

Write the `cmd` to be deterministic and to write valid JSON. It runs against
the tree at the pinned commit; it must not fetch anything over the network,
and it must not depend on the time of day.
