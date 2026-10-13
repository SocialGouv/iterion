---
name: refutation-protocol
description: |
  The second reader's protocol: a draft report's claims — every stated
  non-conformity and every conformity an agent landed — put on trial one by
  one, the four verdicts (supported, partial, refuted, unsupported), what each
  does to the report, and the exact shape to return.
---

# Putting the draft report on trial

You receive a batch of CLAIMS. Each is one line of the draft report with its
anchor: `n` (the claim number — your key), `criteriaId`, `file`, `line`,
`selector`, `claim` (the sentence under trial), and for a landed conformity
`kind: "c"` with the `citations[]` the agent relied on.

## For every claim

1. Open the cited file at the cited line and read the surrounding element.
2. Read the brief for the criterion (`<briefs_dir>/<criteriaId>.md`): the
   numbered tests the claim rests on.
3. Rule, and write a one-line `note` in French saying what you saw.

| verdict | for a non-conformity claim | for a conformity claim (`kind: "c"`) |
|---|---|---|
| `supported` | the defect is real and correctly attached to this criterion | the cited evidence does satisfy the tests |
| `partial` | real, but the criterion or the wording is imprecise | satisfies the tests, but the citation or wording is imprecise |
| `refuted` | false: the cited element is in fact conforming | false: the cited evidence does NOT satisfy the tests |
| `unsupported` | the cited element does not let you decide | the cited evidence does not let you decide |

## What each verdict does

- `refuted` on a conformity: the criterion goes back to « à évaluer » and
  the verdict leaves the ledger — the next run re-asks it.
- `refuted` on a non-conformity an agent found: the finding is pruned.
- `refuted` on one of the ENGINE's static findings: recorded in the annex,
  never applied — the engine's own rules are not overturned by a reader.
- `supported` / `partial`: the claim stays; `partial` notes are kept.
- `unsupported`: the claim stays, counted in the annex as not decided by
  the trial.

The trial is applied only when EVERY claim carries a verdict; an unanswered
claim is re-asked once, then the whole trial is reported as degraded rather
than half-applied. Rule on every claim of your batch.

NA verdicts are not on trial: the engine's trial lists the stated
non-conformities and the landed conformities only. What guards a NA is the
merge before the fold — a NA on a criterion with harvested evidence must
cite that evidence, or it is sent back to the adjudicator.

## Posture

Default to nothing. `supported` is earned on the source, like `refuted`. Do
not soften a true defect; do not uphold a conformity you did not verify;
never rewrite a claim — you verify, you do not re-audit.

## What to return

```json
{
  "batch_id": "r0-c02",
  "verdicts": [
    {"n": 11, "verdict": "supported", "note": "…"},
    {"n": 12, "verdict": "refuted", "note": "…"},
    {"n": 13, "verdict": "unsupported", "note": "…"}
  ]
}
```

One object per claim number of the batch, nothing for any other number.
