# modernize-scope — Scopy 📐

Frames a modernisation **before** it starts. Scopy answers three questions
about a repository it has never seen — what is this today, what would a
programme have to carry, how big is the project — and leaves behind the entry
Morphy is missing: a `.modernize/plan.yaml` draft derived from the inventory
rather than written by hand.

**Status: 0.1.0 — skeleton.** The graph, its refusals and its coverage gate are
wired and compile; the extractors are stubs that emit their envelope and
declare themselves unimplemented. `enabled: false` until they are real.

## The one design decision

Every published figure is computed by a deterministic extractor reading pinned
git objects. The agent's job is to say **where to look and which extractor
applies** — it never counts, because a number an agent produces is a number
nobody can recompute.

That split has to survive the catalogue's universality bar (adding a language
must require zero DSL edits), so it follows `sec-audit-source/scan_health`:

| layer | what it does | what the gate demands |
|---|---|---|
| floor | stack-agnostic reads: tree, file counts, tags, CI presence, deployables proven by a file | output on **any** repository — missing means the extractor crashed, not that the repo is empty |
| per stack | the extractor the matching `lang-<id>` skill prescribes | every **detected** stack produced its artifact, else visible degradation |
| published | index, profile, documents | reads artifacts and the declared perimeter — never the agent |

A stack with no skill is reported `unsupported`. It is never silently zero.

## What it refuses rather than guesses

- a **shallow or grafted clone** — a reachability read there measures the clone,
  not the history: at `--depth=1` a repository of thousands of commits reports a
  squashed import, and the run would publish that as a fact;
- a **reference that is a tag name** with no pinned SHA — moving the tag moves
  every figure without changing the configuration;
- a **declared claim whose evidence is absent** from the tree;
- **the same declaration made twice.** A declaration is a figure: measured on
  this bot's private predecessor, declaring one deployable four times — evidence
  untouched — moved the amplitude index from 3,84 to 4,20 and the published size
  letter from M to L, with the freshness check still green.

## Two phases, and why the second is absent here

Sizing needs a **measured** campaign to calibrate against. On a first pass no
such measurement exists, so Scopy renders amplitude and profile but no
projection: rendering one before a single lot has run would be inventing its own
input.

## What it is not

Not Morphy (which runs the programme), not Goldy (which builds the behavioural
net), not Evoly (which sets a long-horizon vision on a settled codebase). Scopy
does not modify the target's sources; it reads, and it writes documents plus a
contract **draft** the operator still owns.
