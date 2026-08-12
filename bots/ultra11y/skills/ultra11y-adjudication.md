---
name: ultra11y-adjudication
description: |
  The decision protocol for the accessibility criteria a static pass cannot
  decide: the four verdicts and what each one must carry, how to rule on the
  judgment criteria you will actually meet (alt relevance, link purpose,
  reading order, headings, forms), and why `manual` is a respectable answer
  where a guessed `C` is a fabrication.
---

# Ruling on what the engine could not decide

You are filling `ADJUDICATE.todo.json`. Every item is a criterion the engine
refused to decide, with the evidence it harvested. Fill each `verdict`, then
write the whole file back to `<run_dir>/adjudication.json`.

**If the file carries a `contract` key, read it first** — it states the verdict
vocabulary and what each verdict additionally requires, and it is authoritative
over this page if the two ever disagree. When it is absent (older engines did
not emit it), the table below is the contract.

## The four verdicts

| verdict | means | must carry |
|---|---|---|
| `C` | you **verified** it holds | a `justification`; and where the item offers `citations`, an anchor from its own evidence |
| `NC` | it is violated | ≥1 finding with `file`, `line`, `message` **and** a `normativeRef` naming the failed test of the active standard |
| `NA` | no such content exists in scope | a one-line `reason` in the `justification` |
| `manual` | you genuinely cannot decide statically | `reason`: `needs-rendered-dom` or `undecidable` |

**Write the verdicts exactly as spelled above** — `C`, `NC`, `NA` upper-case,
`manual` lower-case. Recent engines accept other casings, older ones refuse the
whole adjudication over it. Exact spelling is right on every version.

## The rule that carries the whole audit

**`manual` is a respectable answer. `C` is not its polite synonym.**

A criterion you did not verify is not conforming — it is *undecided*, and the
report has a section that says exactly that. Marking it `C` moves it into the
conformance rate, where it silently props up a number an auditor will quote.
That is the one way to turn this bot back into the thing it replaces: a clean
bill of health nobody earned.

The gate downstream fail-closes on an unjustified verdict, but it cannot tell
an honest `C` from a lazy one — a plausible sentence satisfies it. You are the
only check on that. When the evidence does not settle it, say so.

Equally: **never invent a non-conformity.** An `NC` you cannot ground in a real
file and line, citing a real test, is a fabrication that will be refused — and
if it were not refused, it would send someone to fix nothing.

The third failure mode is quieter. A good practice that no normative test
requires is **not** an NC. It belongs in `recommendations` (where the item
supports it), which the report renders as a non-normative recommendation and
which never touches the conformance rate.

## The criteria you will actually meet

### Alt-text relevance (1.1.1)
The engine already flagged every *missing* alt. What is left is whether the
present ones are any good.

- Decorative image, `alt=""` and no information lost → `C`.
- `alt` that repeats the filename, says "image"/"photo"/"logo" alone, or
  describes nothing the surrounding text does not already say → `NC`.
- An image whose meaning depends on rendered content you cannot see (a chart, a
  screenshot of data) → `manual`, `needs-rendered-dom`.
- Informative image whose alt conveys the same information as the image → `C`,
  citing the evidence anchor.

Read the surrounding text before ruling. `alt="chart"` on a decorative flourish
is fine; on a data visualisation it is a blocker.

### Link purpose in context (2.4.4)
The evidence carries each link's text, its `href`, and the nearest preceding
heading.

- Text that names its destination on its own → `C`.
- "Read more", "click here", "→", a bare URL, an icon with no name — where the
  surrounding context does not disambiguate → `NC`.
- Ambiguous only because you cannot see the rendered layout (several "Read
  more" links whose cards you cannot reconstruct) → `manual`.

The test is not "would a sighted reader work it out" — it is whether the
purpose is available from the link and its programmatically-determined context.

### Headings and structure (1.3.1, 2.4.6, 2.4.10)
Level *skips* are the engine's. Yours is whether the headings describe their
sections, and whether visually-styled text is doing a heading's job without
being one. Read the file; a `<div class="title">` above a section is an `NC`
against 1.3.1, not a style preference.

### Reading and tab order (1.3.2, 2.4.3)
Decidable from source when order is set by the document; `manual` when it
depends on CSS you cannot resolve (flex/grid reordering, absolute positioning).
Be honest about which case you are in — visual order is a rendering fact.

### Forms (3.3.2, 1.3.5)
Missing labels are the engine's. Yours: whether the label actually describes the
field, whether required-ness and format are communicated in text and not by
colour or placeholder alone, and whether `autocomplete` carries the right token
for a field collecting the user's own data.

### Anything needing a rendered page
Contrast (1.4.3), visible focus (2.4.7), zoom (1.4.4), reflow (1.4.10), text
spacing (1.4.12), content on hover (1.4.13), target size (2.5.8). **This bot
launches no browser.** These are `manual` / `needs-rendered-dom` — always.
Literal colour pairs in the evidence are a hint, never a verdict: the computed
value depends on the cascade you cannot evaluate.

## Under a country standard

When a pack is active (`--standard rgaa`), you rule on the *pack's* criteria and
cite the pack's own tests in `normativeRef` — `"11.2"` or `"11.2.1"`, not the
WCAG id, which looks alike and denotes something unrelated. The pack also
**defines** the words its tests turn on; look them up rather than reasoning from
the everyday sense:

```sh
<engine_cmd> criteria --standard rgaa --glossary "pertinent"
```

## Prior pushback

When an earlier fixer answered a previous review of this pull request, you are
given its reply. Read it before ruling on the findings it contests — it may
carry a fact you cannot see from source (a decorative image really is
decorative; the label really is set at runtime).

Their disagreement does not decide the criterion. The evidence does. If they
are right, rule accordingly and say why in the justification; if they are not,
re-state the finding with its anchor.

## Before you finish

- Every item has a non-null verdict. A missing one fails the whole fold.
- Every `C` and `NA` has a justification that says what you checked — not that
  you checked.
- Every `NC` finding has a resolvable `file`, `line` and a `normativeRef` that
  belongs to the criterion being ruled.
- Every `manual` has one of the two accepted reasons.
- You did not mark anything `C` that you did not actually verify.
