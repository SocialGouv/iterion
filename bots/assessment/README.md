# assessment (Assessy) — phase 0 of a modernisation campaign

Surveys a repository at the START of a campaign and writes the contract the
execution bot then carries out: the state of the repository, the programme
proposed for it, its measured size, and `.modernize/plan.yaml` itself.

Today that contract is written by hand. This bot is the missing step before
it.

## What it produces

| artefact | where | what it is |
|---|---|---|
| `.modernize/plan.yaml` | target repo | the programme contract, every lot validated |
| the outcomes file beside it | target repo | what the programme OWES, each with a `check` command |
| `.modernize/survey.json` | target repo | the declared perimeter every published number was computed over |
| `docs/assessment/*.md` | target repo | the state of the repository and the proposed programme, for a human |

All of it is committed. A published number whose perimeter is not in the tree
beside it cannot be recomputed by anyone, and an assessment nobody can
recompute is a claim.

## Running it

```bash
iterion run bots/assessment/ --var workspace_dir=/path/to/repo
```

The repository must carry a **brief** at `.modernize/brief.yaml` — objectives,
target versions, support policy, permitted changes, decisions already taken.
Without one the run ends at `BRIEF_ABSENT`, named: no tree states why a
campaign exists, and a programme invented from a tree is indistinguishable, on
the page, from an agreed one. The shape is in
[`skills/assessment-brief.md`](skills/assessment-brief.md).

| var | default | what it is |
|---|---|---|
| `workspace_dir` | `${PROJECT_DIR}` | the repository to assess |
| `brief_path` | `.modernize/brief.yaml` | the declared entry — required |
| `plan_path` | `.modernize/plan.yaml` | the contract this bot writes |
| `out_dir` | `docs/assessment` | where the documents land |
| `survey_path` | `.modernize/survey.json` | the materialised perimeter |
| `profile_path` | *(empty)* | an operator's own measurement profile; empty = the bundle default |
| `scratch_dir` | `${PROJECT_SCRATCH_DIR}/assessment` | out-of-tree extractor output |

## Two things worth knowing before reading its output

**A declaration is a number.** The survey agent declares what the agnostic
floor cannot see — which subtrees are first-party, what is deployed, which
systems are talked to. Every declaration is re-verified against the tree at
the pinned commit, deduplicated by canonical identity, and refused when two
overlap. That is not ceremony: measured on this bundle's own fixture,
declaring ONE deployable four times — tree untouched, every consistency check
green — moves the published index from 0.658 to 0.931, a third of the way up
the scale and across a band wherever a repository sits near one.

**A size letter is relative to a profile.** The letter comes out of a
versioned measurement profile (metrics, canonical exclusions, domain of
applicability, synthetic anchor, thresholds) and is always published with that
profile's id and version. Outside the profile's declared domain — a library
with no entrypoint, a repository under the floor — there is no letter at all:
the raw measurements and `not-applicable`. See
[`skills/measurement-profile.md`](skills/measurement-profile.md).

## Adding a stack

Drop a `skills/stack-<id>.md` carrying an `iterion:extractors` block and one
`iterion:script` block per extractor. The workflow reads the block, runs the
scripts for the stacks the survey named, and derives the coverage gate's
expectations from those same blocks. There is **no DSL edit** in that
sentence, and a stack nobody shipped a skill for is reported `unsupported` by
name — never as a zero, which reads as a clean repository.

## What it deliberately does not do

**It does not project effort in hours.** A projection consumes the cost ledger
of a MEASURED campaign, and at the first assessment there is none. Rendering
one would invent its own input.

**It does not decide the business.** Every decision the brief does not resolve
is written into the contract as a PROPOSAL with an identifier, never as a
settled lot. A review step can be skipped; a step that can be skipped must not
be what makes a business decision binding.
