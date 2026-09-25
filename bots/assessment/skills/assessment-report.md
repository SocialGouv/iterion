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
The repository carries [[fact:metric.first_party_lines]], and it publishes
[[fact:metric.entrypoints]].
```

Double SQUARE brackets. The engine's own double-BRACE form is resolved long before
this renderer sees the text, so a placeholder written that way would reach a
signed document as literal characters with no identifier ever checked — the
renderer refuses that shape by name.

**A fact carries its own noun.** `[[fact:size.index]]` renders "an index of
0.93", not "0.93". Write your sentence around what the fact says and never
re-describe it: "[[fact:size.index]] critical findings" would publish a
measured number certifying something nobody measured, and the substitution
would look exactly as sound as a true one.

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

## No digit outside a placeholder, and one explicit exception

Substituting by identifier keeps a figure you invented out of the document. It
does nothing at all about a figure you TYPE beside the placeholders, and "4 200
tests" or "98% of the suite" reads on the page exactly like a measurement. So
the renderer refuses any digit that is not inside a placeholder, and names the
line.

The one legitimate way a digit belongs to prose is a cross-reference. It has
its own form: `[[ref:§2]]` renders as its own text. That form is a SECTION
reference and only that — a section number (`§2`, `§ 3.1`), or text carrying no
digit. A figure written inside it (`[[ref:1 240 findings]]`) does not match, so
it stays on the line and is refused exactly like a typed one: the escape hatch
cannot be used as a way round the rule it is an exception to. Nothing else is
admitted — an ordered-list marker is layout and is ignored, a version number
belongs to a fact, and a count in words belongs to a fact too.

**Every field cites at least one fact.** A judgement block states what the
measurements mean; one that names none of them either says nothing about this
repository or says it in words nobody checked.

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

So a digit filter is not THE rule, and it was never going to be: the mechanism
is the inverted one, and the renderer simply cannot render an assertion that no
identifier backs.

It is still half of it, though, and the half that closes a hole the other half
leaves open — a typed number. The two are layered, and each is refused with its
own message: a digit outside a placeholder, and an identifier nobody measured.
What NEITHER catches is a count written in letters: "two majors behind", "a
handful of routes", "almost no tests". It is named here rather than pretended
away, because it is exactly the sentence a reviewer must strike. A count in
words is a count. Give it its identifier, or drop it.

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
