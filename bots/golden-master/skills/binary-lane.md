---
name: binary-lane
description: Capture generated documents (PDF, spreadsheets, CSV) without building a blind judge — the triple assertion, the volatile fields to strip, and why a rendering comparator is only as good as its renderer's font access.
---

# The binary lane

Documents an application generates — PDF exports, spreadsheets, CSV — are the surface where
golden masters most reliably go blind, and for a reason that does not apply to the other lanes:
**the comparison runs through a renderer or an extractor whose own configuration decides what it
can see.** An HTTP body is bytes you either compare or do not. A document is compared through a
tool, and a misconfigured tool reports "identical" with total confidence.

## The failure this lane exists to prevent

A public-sector modernisation shipped a PDF comparator that, for an entire milestone, validated
pages carrying **not one character**. A word changed in the database still went green. The
comparator rasterised each page and diffed pixels; the renderer emitted no glyphs, so every page
came out blank, and blank matched blank.

Why the renderer emitted nothing is worth knowing, because it generalises. Their PDFs were
produced by a library that references only the 14 standard PDF fonts and **embeds no font file**
— `/FontFile` appears zero times in the document. A renderer given no font data draws nothing.
The text was in the content stream the whole time; only the *rendering* was empty.

That is the shape of the trap: **the document was fine, the judge was not.** And the check that
would have caught it costs one line.

## The triple assertion

Every binary reference asserts three things — and the reason is sharper than "one method is not
enough", because that framing gets the lesson wrong.

**Measured, on the very application described above.** A comparator using **raster only, with no
text assertion at all**, detected the `content_empty` mutant perfectly and passed a full gate.
Rasterising is not a weak method. What made the milestone-long failure possible is that *their*
renderer had **no font data** — pdf.js in a hermetic context, `disableFontFace`, no standard-font
URL — so it drew nothing, and blank matched blank. The same comparator, run against a renderer
with fontconfig access, catches everything.

That is the real lesson, and it is worse than "use several methods":

> **A rendering comparator is exactly as good as its renderer's font access — and that property
> is invisible in the diff.** Nothing in a green result tells you which of the two situations you
> are in.

The text assertion is what makes the difference visible, because it does not depend on rendering
at all. And the `content_empty` mutant is what tells you *which* renderer you have: if it escapes,
your renderer is blind; if it is caught, your renderer is sound. It is a positive diagnostic, not
only a trap detector.

Every binary reference therefore asserts:

1. **Extracted text is non-empty.** The cheapest possible check, and the one that would have
   caught the failure above on day one. If a document that should carry words yields an empty
   string, the extraction is broken — stop and fix it before comparing anything.
2. **The canonicalised text hashes as expected.** This is what detects a changed value.
3. **The rendered raster hashes as expected.** This is what detects a layout change that leaves
   the text identical.

Assertions 2 and 3 answer different questions. Text catches a wrong value with the same layout;
raster catches a moved element with the same words. Neither subsumes the other, and either alone
is a blind spot you chose.

If rendering is not available in the environment, **say so in the report and ship the lane
without assertion 3** — a documented gap is honest. Silently dropping to text-only and calling
the lane covered is not.

## Volatile fields, per format

**PDF.** Strip `/CreationDate`, `/ModDate`, `/ID`, `/Producer`, and any XMP timestamp. These
change on every generation and neutralising them is mandatory. Do **not** strip the page count,
the text, or the object structure.

**Spreadsheets (xlsx).** The file is a zip: entry mtimes change on every write and must be
ignored. `docProps/app.xml` and `docProps/core.xml` carry generation timestamps and a producer
string. Compare **cell content**, not the archive bytes — a zip differs from itself on two
consecutive writes.

**CSV.** Line endings and trailing newline vary by generator; column ORDER and row ORDER do not —
they are business, and sorting them away destroys exactly the signal a database migration
produces.

## Required mutants

Two archetypes are enforced for the `binary` surface, and each is blind to a different judge
defect:

- **`content_empty`** — produce a document that is structurally valid and carries no extractable
  content. This is the direct reproduction of the failure above. A comparator that passes this
  mutant is blind in the most consequential way, and the gate must go red.
- **`value_change`** — change one word that must appear in the rendered document.

Write the `content_empty` mutant **first**. It is the cheapest to build and the most damning if
it escapes.

## Tooling

The bundle ships `pdftotext` and `pdftoppm` (poppler). `pdftotext -layout` preserves column
structure, which makes a diff readable; without it a table collapses into an unreadable stream.
Rasterise at a **fixed** resolution (`pdftoppm -r 100`) — a resolution that varies between
captures makes every hash differ.

Spreadsheets need no extra tool: Python's `zipfile` opens an xlsx, and the cell values live in
`xl/sharedStrings.xml` and `xl/worksheets/sheet*.xml`. Parsing those two with the stdlib is
enough, and it avoids a dependency the sandbox may not have.

## Sanity check before recording

Run the extraction by hand once, on one real document, and **look at the output**. If it is
empty, or if it is 40 bytes where you expected a page of text, the lane is already blind and no
amount of comparison logic will fix it. That single look is what the milestone described above
never took.
