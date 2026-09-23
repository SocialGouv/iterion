---
name: assessment-report
description: How the assessment documents are written — factual assertions inserted by fact identifier and substituted by the renderer, judgement confined to marked blocks, and why a digit detector was the wrong mechanism.
---

# Writing the judgement, without writing the facts

The assessment publishes two things that must not be confused: what was
**measured**, and what someone **concludes** from it. The first is arithmetic
over a declared perimeter and is reproducible by anyone with the repository.
The second is a reading, it is arguable, and it belongs to whoever signs it.

The documents keep them apart mechanically.

## Facts are inserted by identifier

Every measured value the renderer can publish exists as a **fact**: an
identifier, and the exact text that replaces it. You are given the whole list
before you write. When you need to state something measured, you write the
placeholder:

```
The repository carries {{fact:floor.first_party_lines}} of first-party source,
spread over {{fact:floor.first_party_files}}.
```

The renderer substitutes each placeholder from the facts file. **An identifier
the facts file does not carry is a refusal**: the run ends at `RENDER_REFUSED`
naming the unknown identifier, and the document is not written. That is the
whole mechanism. A figure you remember, approximate or infer never reaches a
reader over a signature that is not yours, because it stops the run instead.

Two consequences worth stating plainly:

- If you want to say something and no fact backs it, you cannot say it. Say
  something else, or say that it was not established.
- If a fact exists but reads awkwardly in your sentence, rewrite the sentence.
  You may not retype the value to make it fit.

## Judgement is what is left, and it is marked

Everything that is not a substituted fact is judgement: what the measurements
mean together, which risk dominates, what order the work should take, what the
numbers do not say. Judgement lives in the fields you write and the renderer
places it under headings that name it as such. A reader must be able to tell,
without knowing how the document was produced, which sentences are arithmetic
and which are an opinion.

## Why a digit detector is not the rule

An earlier form of this rule was "no figure in the prose", checked by
refusing digits. It does not work, and the reason generalises:

- **"two majors behind" carries no digit.** Nor do "a handful of routes", "no
  continuous integration at all", "almost no tests". They are quantitative
  assertions written in letters, and a digit filter passes every one.
- **A digit filter rejects legitimate text.** A cross-reference like "§ 2", a
  version named in a quoted command, an identifier containing a number — all
  rejected, none a violation.

Widening the pattern makes the second problem worse and never closes the
first. So the mechanism is inverted: the renderer does not hunt for forbidden
spellings, it simply cannot render an assertion that no identifier backs. A
sentence saying "two majors behind" without a placeholder is still possible to
write — and it is exactly the sentence a reviewer must strike, which is why it
is named here rather than pretended away. A count in words is a count. Give it
its identifier, or drop it.

## The size letter never travels alone

The size band is relative to a **measurement profile**: a set of metrics,
canonical exclusions, a domain of applicability, a synthetic anchor and a set
of thresholds, published together under a version. The renderer always emits
the band with the profile's identifier and version beside it.

Do not write the letter into your prose. A letter quoted without its profile
is a false quotation — two repositories sized under different profiles are not
comparable, and the letter looks like a property of the project when it is a
convention attached to a scale. When the repository falls outside the
profile's declared domain, there IS no letter: the document publishes the raw
measurements and says "not applicable", and your judgement should say what it
could say instead.

## Shape of what you return

- `state_judgement` — what this repository is, read whole. At most three
  paragraphs. Markdown. Placeholders wherever you state a measurement.
- `plan_judgement` — what modernising it has to face, in what order, and what
  fixes that order. Name the compatibility edges you can defend, and say when
  an edge is a guess.
- `open_questions` — one bullet per decision this assessment cannot take, each
  addressed to a named role or to the brief's owner. A question you can answer
  from the facts is not an open question; delete it.
