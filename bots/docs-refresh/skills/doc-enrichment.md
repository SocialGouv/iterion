---
name: doc-enrichment
description: Discipline for the code→docs half of a Doki campaign — what deserves documentation, where to put it, how to write it, when to dismiss instead, and how to tell obsolete prose from a deliberate unfulfilled promise.
---

# Doc enrichment — writing the missing documentation

Apply this skill to every `undocumented` / `undocumented_area` candidate in
the drift manifest, and whenever you notice an undocumented capability while
verifying another candidate. The mission is exhaustive DOC-side alignment:
docs follow code — repair what is stale, WRITE what is missing. You never
touch code. The manifest's containment scan is a heuristic candidate
generator; you are the adjudicator. Read the code first, always: a section
written without having read the code it describes is a façade.

## What deserves documentation

Document, in rough priority order:

1. **User-facing capabilities** — anything an operator or integrator can
   invoke, configure, or observe: commands, endpoints, UI surfaces,
   file formats the tool reads or writes.
2. **CLI surface** — commands, subcommands, flags, exit codes, diagnostic
   codes (an `undocumented` candidate names these directly; verify the
   thing still exists and how it actually behaves before writing).
3. **Configuration and environment surface** — config files, env vars,
   defaults and their precedence.
4. **Workflows** — how to build, test, run, release; anything a new
   contributor must know to be productive.
5. **Extension points** — plugin seams, public interfaces, hooks, APIs
   other code is expected to implement or call.
6. **Significant code areas** — a top-level or src-root subdirectory doing
   real work deserves at least a one-line entry in the repo's module map /
   architecture page (an `undocumented_area` candidate usually resolves to
   exactly that: one accurate line in an existing overview).

Do NOT document (dismiss instead, with the reason in the ledger):

- internal helpers and plumbing with no outside consumer;
- generated code, vendored dependencies, build artifacts;
- test scaffolding and fixtures;
- dead or deprecated surface about to be removed (say so in the reason);
- anything whose documentation would merely paraphrase the code without
  helping a reader decide or act.

## Placement heuristics

Prefer, in order:

1. **The most appropriate EXISTING doc.** A new flag belongs in the doc that
   already lists the command's flags; a new package belongs in the existing
   module map (README or architecture page). Extending an existing section
   beats creating a parallel one — one topic, one home.
2. **A new section in an existing doc** when the doc covers the area but has
   no section for this capability.
3. **A new `.md` file** only when no existing doc plausibly owns the topic.
   Put it where this repo keeps docs (the run's `docs_dir`, typically
   `docs/`), follow the repo's naming convention (match the case and word
   separators of its sibling files), and LINK it from the nearest index
   (README or the docs index) — an orphan page is not documentation.

Mirror the target repo's conventions, not your own taste: heading style,
tone, code-fence language tags, link style. Read two or three neighbouring
docs before writing.

## Style rules

- **Concise and code-grounded.** Every claim must reflect what you read in
  the code — cite real names, real defaults, real paths. If you cannot
  verify a behaviour, omit it rather than guess.
- **Present-state only.** Describe what the code does now — no history
  ("previously…", "as of the refactor…"), no changelog prose, no marketing
  ("blazingly fast", "powerful"). The commit message carries the history.
- **Smallest useful unit.** One accurate paragraph beats a padded page. An
  `undocumented_area` fix is often a single line in a module map.
- **Commit in stride**: `docs(<area>): document <capability>`, body ending
  with the `Bot: docs-refresh` trailer, one commit per doc touched.

## Dismissal discipline

Dismissing is a first-class outcome, not a failure. When a candidate is not
worth documenting, append `{doc, kind, value, reason}` to the dismissals
ledger — the reason must state WHY it does not deserve documentation
("internal, not user-facing", "generated code", "deprecated, removal
planned"), not merely that you chose to skip it. The ledger is how the
adjudication persists: an unrecorded dismissal comes back next pass, and a
recorded one never re-surfaces. Undocumented candidates carry an empty
`doc` field — keep it empty in the ledger entry so the keys match.

## Obsolete prose vs unfulfilled promise

When a documented claim does not match the code, decide WHICH failure it is
before acting:

- **Obsolete** — the code moved on and the prose was left behind. Signals:
  past-tense reality (the flag was renamed, the file was deleted), the git
  history shows the capability being removed or replaced, neighbouring docs
  already describe the new state. → FIX the doc (repair half).
- **Unfulfilled promise** — the claim is a deliberate ambition the code has
  not caught up with. Signals: future or intent phrasing ("will",
  "planned", "roadmap", "TODO"), an accepted ADR or design doc not yet
  implemented, a capability that was never present in history (announced,
  not removed). → Neither delete it nor "align" it down to the current
  code. Record it in the promises ledger (`<scratch_dir>/promises.json`):
  `{doc, claim, code_gap, note}` with `code_gap` as the big-picture missing
  work — AND record the candidate in the dismissals ledger (reason
  `promise: <short ref>`) so the manifest stops re-surfacing it.
  Optionally — your judgment, case by case — add a clearly-flagged honest
  status note in the doc itself ("Implementation status: …; remaining
  work: …") and commit it: documenting the gap IS a legitimate
  doc↔reality alignment.

A promise is not a code bug (`is_code_bug` is for code that is broken
against a correct contract); here the code is simply behind the documented
ambition. Promises never block convergence — they are reported in the PR
body under "Unfulfilled documented promises".
