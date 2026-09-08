# modernize-scope — Scopy 📐

Frames a modernisation **before** it starts. Scopy answers three questions
about a repository it has never seen — what is this today, what would a
programme have to carry, how big is the project — and leaves behind the entry
Morphy is missing: a `.modernize/plan.yaml` draft derived from the inventory
rather than written by hand.

**Status: 0.2.0 — the perimeter is real, the extractors are not.** `scope_write`,
`scope_lint` and the agnostic floor run; the per-stack extractors and the
renderers are still stubs that emit their envelope and declare themselves
unimplemented. `enabled: false` until they are real.

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

## The perimeter, and why it is guarded like a figure

Everything published is a measure over a PERIMETER, so the perimeter is the one
declaration that changes every number at once. It is built once, by
`scope_write`, from what the surveyor declared; `scope_lint` checks it against
the tree; the floor re-checks it before measuring. Deleting a layer degrades the
diagnosis, never the guarantee — each one re-runs what it depends on.

Paths are normalised to one spelling before anything is compared, because
`vendor/x`, `./vendor/x` and `vendor//x` are three declarations to a textual
check and one to the tree. Duplicates are then refused rather than collapsed:
folding four declarations into one silently repairs the very thing being
guarded.

`scope_write` also drops an audit copy of the perimeter at
`<out_dir>/scope.json`, and `scope_lint` compares it byte for byte against the
object the floor will consume. A reader who recomputes a figure works from that
file; if it is not what was measured, they get a different answer and nothing
says so.

## Falsification

`scope.py --selftest` fires every named refusal at an input it must refuse.
`--falsify` is what makes that count mean something: it neutralises each `raise
Refusal` in the source in turn and demands the bench redden. It is enumerated
from the parsed source rather than from a hand-kept list, so a refusal added
tomorrow is either covered or reported. Measured while writing this bundle: ten
guards were exercised and seventeen further refusal sites were not, and every
run was green.

```sh
python3 bots/modernize-scope/scope.py --selftest    # 26 checks, 38 guards
python3 bots/modernize-scope/scope.py --falsify     # 34 refusals, 0 survivors
python3 bots/modernize-scope/sync.py                # regenerate the inlined copies
```

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
  letter from M to L, with the freshness check still green;
- **an exclusion that removes no file another exclusion does not already
  remove** — the same repetition one indirection away, invisible to a textual
  check because the two globs differ;
- **a perimeter emptied by an over-broad glob.** Excluding everything makes
  every figure zero, every gate green and every document confident.

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
