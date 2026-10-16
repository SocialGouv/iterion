---
name: adjudication-protocol
description: |
  How to rule on one batch of criteria the engine could not decide: reading
  a brief and an evidence file, the four verdicts and what each must carry,
  the honest use of `manual`, the no-subject case, citations, the re-ask,
  and the exact shape to return. The vocabulary comes from the engine's
  `contract` in your input; this page never restates a criterion.
---

# Ruling on what the engine could not decide

## Before the first verdict

1. Read the `contract` you were given. It states the verdicts and what each
   additionally requires. It is authoritative over this page.
2. For each id of your batch, read the brief (`<briefs_dir>/<id>.md`) BEFORE
   the evidence: the tests are numbered there, the methodology says what to
   look at, the glossary defines the words the tests turn on (« pertinent »,
   « si nécessaire », …). Reason from those definitions, not from the
   everyday sense.
3. Then read the evidence file (`<items_dir>/<id>.json`) and open the cited
   sources when the snippet does not settle it.

## The four verdicts

| verdict | means | must carry |
|---|---|---|
| `C` | you verified that every test holds on the evidence shown | a `justification` and `citations[]`, each copied from THIS criterion's own `evidence[]` (`file`, `line`, `snippet`) |
| `NC` | at least one test fails on a real element | `findings[]`, each with `file`, `line`, `message`, and a `normativeRef` that is the criterion id or one of its `testIds` — never a W3C technique code, which the fold refuses as fabricated |
| `NA` | the subject of the criterion exists nowhere in the audited scope | a `justification`; plus `citations[]` whenever evidence was shown (it is out of scope, not cleared) |
| `manual` | you genuinely cannot decide from source | `reason`: `needs-rendered-dom` or `undecidable` |

Write every `justification` and every finding `message` in French: they are
rendered verbatim in the French report and in the board issues.

## The rule that carries the audit

**`manual` is a respectable answer. `C` is not its polite synonym.** A
criterion you did not verify is not conforming; it is undecided, and the
report says so. Marking it `C` moves it into the conformance rate an auditor
will quote. The fold refuses a C without citations, but it cannot tell an
honest C from a lazy one. You can.

Equally: never invent a non-conformity. An NC you cannot anchor on a real
file and line, under a real test, is refused — and if it were not, it would
send someone to fix nothing. A good practice no test requires is not an NC;
it goes in `recommendations[]`, anchored like a citation (below).

## Three cases you will meet

- **No subject** (`population` all zeros, no evidence): NA, confirmed by one
  grep for the subject, with a one-line justification. A `manual` here
  leaves the report inconsistent and comes back to you.
- **Needs a rendered page** (`needsRenderedEvidence`): `manual` with
  `needs-rendered-dom`, unless the test is decidable from source alone.
  Literal colour pairs in the evidence are a hint, never a verdict.
- **Evidence present but inconclusive**: open the source. If it still does
  not settle it, `manual` with `undecidable` and say why.

## Citations

A citation is one of the anchors the engine harvested for THIS criterion:
copy its `file`, `line` and `snippet` from the evidence file. The bot checks
that every citation is among those anchors and sends the verdict back
otherwise, before the engine ever sees it. The second reader's trial grounds
a conformity's citations as stored, with no fallback to the anchor: copy the
snippet exactly (an excerpt of it is fine). Before the real fold the bot gives
a conformity's retyped or missing snippet its anchor's own where the engine
accepts both versions and the restored text grounds; any other snippet is
folded as written, and one the engine neither finds in the source nor
recognises as its anchor is refused.

## Recommendations

A recommendation is a good practice no test requires: it never changes the
verdict, and the report renders it as an advisory. Each one is an object
`{"file", "line", "snippet", "message"}` whose `file` and `line` are one of
THIS criterion's evidence anchors, copied exactly as for a citation, with
the `message` in French. The engine refuses the whole verdict over a single
free-text recommendation, so the bot drops any recommendation that is not
such an object or stands on no anchor of the criterion, and counts it in the
annex: a dropped recommendation costs nothing but itself.

## The re-ask

When `prior_issues` names your criterion, the engine (or the bot) refused
the previous verdict for the reason stated. Fix exactly that — add the
citation, ground the finding, give the reason, decide NA where there is no
subject — and return the corrected verdict. There is one re-ask; a verdict
refused twice stays « à évaluer » and is named in the annex.

## What to return

```json
{
  "batch_id": "r0-b03",
  "verdicts": [
    {"criteriaId": "6.1", "verdict": "C", "justification": "…", "citations": [{"file": "index.html", "line": 12, "snippet": "<a href=\"/contact\">Nous contacter</a>"}], "recommendations": [{"file": "index.html", "line": 12, "snippet": "<a href=\"/contact\">Nous contacter</a>", "message": "…"}]},
    {"criteriaId": "6.2", "verdict": "NC", "justification": "…", "findings": [{"file": "index.html", "line": 10, "message": "…", "normativeRef": "6.2"}]},
    {"criteriaId": "4.1", "verdict": "NA", "justification": "…"},
    {"criteriaId": "10.7", "verdict": "manual", "reason": "needs-rendered-dom", "justification": "…"}
  ],
  "notes": "…"
}
```

One object per id of the batch, nothing for any other id.
