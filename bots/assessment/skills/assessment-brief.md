---
name: assessment-brief
description: The .modernize/brief.yaml declared entry — what a repository cannot state about its own modernisation, the fields that carry it, and why an absent brief is a named refusal rather than a default.
---

# The declared brief

`.modernize/brief.yaml` lives in the **target** repository and carries what no
tree states: why this campaign exists, what it is allowed to change, and which
decisions are already taken. The assessment refuses to run without one.

That refusal is the point, not a formality. Three families of fact decide a
modernisation programme and none of them is recoverable from a repository:

- **A future or external fact.** "This runtime loses support in eighteen
  months", "the platform team will provide a managed datastore", "the client
  will not fund a rewrite of the reporting module". Nothing in any commit
  certifies a fact that has not happened yet.
- **A permission.** "Observable behaviour may change here, never there",
  "dropping support for the old client is agreed", "the API contract is
  frozen". A permission is somebody's signature.
- **A decision already taken.** A target version, a chosen engine, an ordering
  imposed by a compatibility matrix somebody read. A bot that re-derives these
  from the tree re-derives them wrong in the direction that costs a campaign,
  because the tree records what happened and never what should.

A `source:` field pointing at a document does not repair this: the assessment
can check that a file exists, never that the claim inside it is true.

## Shape

```yaml
version: 1

## Why this campaign exists. Prose, one paragraph, read by a human first.
objective: |
  ...

## What the campaign must reach, one entry per goal. `id` is referenced by
## the contract's outcomes; `statement` is what "met" means in one line.
goals:
  - id: supported-runtime
    statement: "the served application runs on a runtime under active support"
    rationale: "support for the current one ends on the date below"

## Target versions and series, as DECIDED — never as guessed. `component` is
## whatever the target repository calls the thing; this file names it, the
## assessment never invents one.
targets:
  - component: "<the name used in the repository>"
    current: "<version in the tree, as declared where it is declared>"
    target: "<the decided version>"
    series: ["<the intermediate steps, in order, when the path is imposed>"]
    decided_by: "<who signed this>"
    decided_on: "2026-01-31"

## Support policy — the dates that make a goal urgent rather than desirable.
support_policy:
  - component: "<name>"
    supported_until: "2026-12-31"
    source: "<where this date is published>"

## What the campaign MAY change, and what it may not. The execution bot reads
## the contract, not this file; the assessment turns these into lot intents
## and `rebaseline_allowed` flags, so write them as permissions.
permitted_changes:
  - "dependency majors, when the behaviour net stays green"
  - "the build tool and its layout"
forbidden_changes:
  - "the public API surface of the <name> module"
  - "anything observable by the <name> client before its own migration"

## Decisions already taken. Anything NOT here is an open question, and the
## assessment writes it into the contract as a PROPOSAL with an id — never as
## a settled lot.
decisions:
  - id: datastore-engine
    decision: "<the chosen engine>"
    decided_by: "<who>"
    decided_on: "2026-01-31"

## Who arbitrates what the assessment cannot decide.
owner: "<role or name>"
```

Only `version`, `objective`, `goals` and `owner` are required. Every other
block may be absent — and its absence has a consequence the assessment states
rather than smooths over: no `targets` means every version distance is an open
proposal; no `support_policy` means no goal can be called urgent; no
`decisions` means the whole programme is proposed rather than agreed.

## An unresolved decision stays a PROPOSAL

The assessment writes what the brief resolves, and proposes the rest. A
proposal carries an id, the question in one line, the options it sees, and a
recommendation. It is never promoted to a lot by a review step: a review can
be skipped — `action: skip` is a legal answer — and a step that can be skipped
must not be the thing that makes a business decision binding.

So, reading a produced contract: a lot is something the brief authorised; a
proposal is something waiting for the brief's owner. If those two are not
distinguishable in the file, the file is not auditable.

## Refusal, and what it says

An absent, unparseable or incomplete brief ends the run at `BRIEF_ABSENT`,
naming the file and the missing field. That is a NAMED refusal and not a
degraded mode, for the same reason the execution bot refuses a contract it
cannot read: a bot whose whole job is to write a programme from declared
objectives, run with no objectives declared, would publish a programme it
invented — and an invented programme is indistinguishable, on the page, from
an agreed one.
