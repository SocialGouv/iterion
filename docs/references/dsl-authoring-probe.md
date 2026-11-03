# The authoring probe — measuring "right the first time"

The programme behind the DSL registry, the skill and the template gallery
(#1010) has one measurable goal: an agent given the skill writes a complex,
correct bot on the first draft, without reading the whole documentation
first. This is the protocol that measures it, so that a change to the skill,
the gallery or the diagnostics is judged by a number and not by an
impression. It is the F20 finding of the plan's adversarial review: a probe
whose sessions are not fresh, whose versions are not pinned, or whose reading
is not measured separately from its total tokens cannot attribute a gain to
anything.

## What is measured

One run of the probe is one agent, one spec, one fresh session. Record:

| Measure | How |
|---|---|
| tokens READ | the input tokens of the session's file reads only (the docs, the skills, the catalog bots the agent opened) — what the knowledge cost |
| tokens total | the session's total input + output |
| minutes to the first draft | wall clock from the first turn to the first complete `.bot` |
| errors at the first draft, by class | `iterion validate --json` on the first draft, each diagnostic classed L (lexical), K (keyword collision), S (graph semantics), D (doc drift) per the plan's table; R (runtime) is counted from a stub run when one is made |
| rounds to green | how many `iterion validate` rounds until 0 errors |
| functional conformance | the spec's checklist below, each item verified by reading the final `.bot` (never by the agent's own report) |

The reading is what the gallery and the skill are meant to shrink; the
errors are what the registry and the diagnostics are meant to shrink. They
are reported apart, because a probe that reads 250k tokens and writes a
perfect bot proves the docs suffice, not that the skill does.

## Protocol

1. **Fresh session, one agent, one model.** No prior context, no memory of an
   earlier probe. Name the model and the harness (Claude Code, pi, claw…).
2. **Pinned build.** The agent validates with the build under test and
   nothing else: `iterion version --commit` in the report. A stale binary
   turns a new keyword into a false C040 (measured on the first probe).
3. **The context is the skill and the repository.** The agent gets `SKILL.md`
   (or the whats-next quickref) and read access to the checkout. It is told
   it MAY run `iterion bots templates`, `iterion bots create --template` and
   `iterion validate`, and that the first draft must be written BEFORE the
   first `validate` (that is what "first time" means). Nothing else is
   forbidden: what the agent chooses to read is a measure, not a rule.
4. **Three specs**, one run each per model, and at least two models (a
   strong one and a mid-size one — the mid-size one is the real user of the
   whats-next builder). Three repetitions when the numbers are close.
5. **Archive** the first draft, the final `.bot`, the `validate --json`
   outputs and the session's token accounting next to the report, so a later
   probe can be compared to this one and not to a memory of it.

## The three specs

A spec that cites the tree makes a claim about it. Each spec that opens an
existing file carries a `file | lines | holds` table, read at test time by
`TestAuthoringProbeSpecsNameTargetsThatExist` in `internal/docsguard`: the
paths must exist, each count must be within a quarter of `wc -l` (with 100
lines of slack), and every structure a `holds` cell names must still be in
that file. The dated rows below are observations rather than guidance and
stay out of its reach.

### Spec 1 — creation (the plan's ten requirements)

Write `release_readiness`, a bot that decides whether a repository is ready
to tag a release. It must have, in this order of the graph:

1. a `tool` node that runs the repository's tests and reports the outcome as
   JSON on stdout;
2. a fan-out of TWO reviewers in parallel — one `claude_code` agent that is
   read-only, one `claw` agent on an OpenAI model — each reporting findings
   and a `blocking` flag;
3. a `compute` node that converges the two (`await: wait_all`) into a single
   `ready` verdict without any LLM;
4. a fix loop: when not ready, a fixer agent amends the code and the tests
   run again, bounded by a var `max_fix_passes` read from `vars`;
5. a typed `fail` node reached when the fix passes are exhausted, with its
   own failure code;
6. a `human` gate before the tag, showing the verdict and asking for approval;
7. a Verified Action tool that creates the git tag named by a var
   `release_tag`, with a `postcondition` proving the tag exists;
8. a guard: when the run has spent more than half of its `max_duration`
   before reaching the gate, it fails resumable instead of asking the human
   (`run.*` in a compute);
9. a `budget:` block, `worktree: auto`, and `sandbox: auto`;
10. every prompt as a declared `prompt` block, every node output typed by a
    `schema`.

Checklist: one line per requirement, verified by reading the `.bot`.

### Spec 2 — edition of a catalog bot

Open the `bots/review-pr` program. Since lot 3's `import` it is four files,
and this spec's edit crosses all four:

| file | lines | holds |
|---|---|---|
| `bots/review-pr/main.bot` | 720 | `workflow review_pr:` — the vars, the budget and every edge — and `prompt pacer_policy:`; no node and no schema |
| `bots/review-pr/lib/nodes.bot` | 1 329 | `router fan:` with `mode: fan_out_all`, the four reviewer judges (`judge reviewer_claude:` … `judge reviewer_gpt_glance:`), `compute merge_reviews:`, `compute pr_gate:` |
| `bots/review-pr/lib/prompts.bot` | 539 | `prompt review_system:`, one user prompt per family, `prompt emit_system:` |
| `bots/review-pr/lib/schemas.bot` | 275 | `schema review_output:`, what a reviewer returns, and `schema family_reviews:`, the merged blocks |

Add a reviewer lens — `dependencies`: reads the manifest and lockfile
changes and flags a major bump — to the existing fan-out, WITHOUT changing
the verdict's semantics and keeping the merge gate's count deterministic.
The fan-out already carries two FAMILIES × two tiers, so the difficulty is
no longer writing the node: it is the three slots the new lens has to join,
which the spec names because the agent has to honour all three.

1. The branch: an edge off `router fan:`, which `router topology:` reaches
   only on `review_mode == 'dual' || review_tier == 'audit'`.
2. The merge slot: `compute merge_reviews:` normalises per FAMILY
   (`claude_*` and `gpt_*`, each guarded by its own `_ran` flag) and
   `schema review_output:` constrains `family` to those two, so a third
   lens has nowhere to land until both are widened.
3. The absence: every merged key carries an `if()` fallback, because a run
   that did not schedule the lens renders a reference to an absent node as
   a literal placeholder — which reads to the merging agent as a broken
   template rather than as "that lens did not run".

Checklist: the new node and its two edges; `schema family_reviews:` declares
the lens's keys and `compute merge_reviews:` fills them, fallback included;
`compute pr_gate:` and the verdict's own counts untouched; validating the
unit adds no error and no warning (as shipped it has 0 errors and 32 C315
warnings, one per route read from the environment, plus C251 on a build
with no orderable version); the diff touches those four files and nothing
else, and adds under 120 lines — a judge of the glance tier's size is ~25,
its user prompt ~30, the schema and merge keys ~17, the edges ~6, and
rendering the new block ~10.

### Spec 3 — repair from diagnostics

Take `examples/review-merge-gate.bot` and break it in five ways an author
would. Each break has a site in that file and raises exactly one error:

| file | lines | holds |
|---|---|---|
| `examples/review-merge-gate.bot` | 123 | `workflow review_merge_gate:` and `human ship_review:`, the node four of the five breaks edit |

| break | the edit | code |
|---|---|---|
| a property renamed to a plausible one | `merge_strategy: squash` becomes `merge_mode: squash` | E012 |
| an unquoted value holding a `/` | `model: "anthropic/claude-sonnet-4-6"` loses its quotes | E001 |
| a `when` on a non-boolean field | `when "decision == 'approved'"` becomes `when decision` | C013 |
| an undeclared loop | `as fix_loop(5)` drops off the back edge | C019 |
| a prompt name misspelt | `system: companion_system` gains a typo | C003 |

Give the agent ONLY the `iterion validate --json` output and ask for the
fixed file. Checklist: the five errors gone, no other change in the file,
one `validate` round. The file is clean as shipped and the five breaks
together raise exactly those five errors and nothing else, so a sixth
diagnostic is the agent's own. The quoted-ref break (C137) has no site
here: this file declares no `tool` node, and C137 reads a tool body.

## Baseline and targets

| Probe | Model | Lines | Errors at first draft | Reading (as recorded) | Minutes | Rounds |
|---|---|---|---|---|---|---|
| 2026-09-09, spec 1, no gallery, no template section in the skill | claude-opus-4-8 (Claude Code) | 331 | 0 | ~213k total, ~2 300 lines | 20 | 0 |
| 2026-09-09, spec 1, same | claude-sonnet-4-6 (Claude Code) | 215 | 0 | ~278k total, ~2 500 lines | 37 | 0 |
| 2026-09-10, spec 1, the gallery + the "Start from a template" section (#1114, before the rules below were written) | the harness's `opus` label (self-reported "Claude Opus 4.5"), Claude Agent SDK subagent | 229 | 0 (and 0 warnings) | 189k total, **1 509 lines** | 12 | 1 (green first draft) |
| 2026-09-10, spec 1, same | the harness's `sonnet` label (self-reported "Claude Sonnet 5"), same | 199 | 0 (and 0 warnings) | 245k total, 2 497 lines | 16 | 1 (green first draft) |
| 2026-09-16, spec 1, the lot 4 build (`e652e78ed`: the dry run, `fmt`/`fix`, C145–C147, the skill's "Rules the grammar does not show"), standard protocol | `claude-haiku-4-5-20251001`, Claude Agent SDK subagent | 217 | 4 (C010; C040 ×2, C033) and 2 warnings (C145, C137) | not captured, 1 113 lines | 3.5 | 2 |
| 2026-09-16, spec 1, same build, reading capped to the skill and three gallery shapes (no docs, no catalogue, no source) | same | 239 | 4 (C012; C029 ×2, C014) and 2 warnings (C145, C137) | not captured, ~1 200 lines | ~4 | 2 |
| 2026-09-22, spec 1, **the YAML twin** (lot 5 PR B, the prototype: the author document read by `pkg/dsl/author`, validated by the `probecheck` harness — converter, parser and compiler, as `validate` will), reading capped to the skill, a one-page explainer of the twin and the three gallery shapes written as YAML (232 lines); three runs, the explainer's second version | `claude-haiku-4-5-20251001`, Claude Agent SDK subagent | 189 / 176 / 138 | 11 (E012; E002 ×3, E001 ×2 on two edge strings; C012, C016 ×2, C040 ×2) and 3 warnings / 1 (E012: `worktree:` on an agent) / 0 and 1 warning (C137) | 821 lines each | 2–3 | 4 / 2 / 2 |
| 2026-09-22, spec 1, **the `.bot` control of the same day** — same model, same build, reading capped to the skill and the same three shapes scaffolded as `.bot` (431 lines); three runs | same | 267 / 219 / 248 | 6 (E002 on a bare `when a && b`; C040, C016 ×4) and 2 warnings / 9 (C034 ×2, C031, C040 ×3, C012, C033, C014) and 2 warnings / 1 (C033) | 912 lines each | 2–5 | 5 / 3 / 2 |
| 2026-09-22, spec 1, the YAML twin with the explainer's FIRST version, whose own example wrote a `command:` holding `: ` unquoted — not YAML — and which two of three runs copied; kept as the measure of that trap | same | 215 / 151 / 149 | 7 (E001 ×2 on a bare `when a && b`; C012, C016 ×2, C036, C040) and 4 warnings / 1 (E050: not YAML) / 1 (E050) | 812–817 lines | 2–3.3 | 3 / 4 / 6 |

Rounds count the agent's `validate` runs up to and including the green one; a
0 means the protocol forbade the agent to validate, and the draft was green
when validated afterwards. On 2026-09-22 the first draft's errors are the
harness's on the archived draft (the compile runs whatever the parse said, as
`validate` does), never the agent's own count.

The 2026-09-09 drafts were correct; both cost a whole session of reading
before the first line, and both agents had to guess the same unwritten
rules (the loop-exhaustion exit, the tool's stdout-JSON contract,
`outputs.*` without `with`). On 2026-09-10, with the gallery, both agents
scaffolded three or four shapes as REFERENCE and composed the final file
from them (no single shape covers the ten requirements — by design, the
spec is a composite); the strong model read a third less and finished in
half the time, the mid-size model still read `docs/dsl.md`, the grammar
and `docs/routers.md` whole (2 497 lines — the reading did not shrink,
the errors did not appear either). The strong model reported fourteen
rules it had to guess — the `run.*` members, `max_* == 0` meaning
unbounded, which endpoints C244 judges, which edge carries `as name(N)`,
the one-predecessor condition on `resumable`, the reserved failure codes,
`jq` in the image, both reviewers read-only — and two of them by reading
COMPILER SOURCE (`validate_edges.go`, `lifecycle.go`); every one is now a
bullet of the skill's "Rules the grammar does not show". The targets after
the registry (#1092, #1103) and the gallery (#1110): **0 L/K/D errors, at
most 2 S errors, at most 30k tokens read, green in at most 2 rounds** —
and the same on the mid-size model. Reached on errors and rounds by both.
The reading target is still unmeasured: no row above records tokens READ —
the 2026-09-09 and 2026-09-10 rows give the session's TOTAL tokens and a
line count, the later rows a line count alone — and a line is not a token.
Artifacts: the first drafts and the reading ledgers of both 2026-09-10 runs
are in the secret gist the ticket links (four files); the `validate` outputs
of those two runs were not archived.

On 2026-09-16, with the rules section in place and the lot 4 diagnostics, the
mid-size model read 1 113 lines — the shortest reading to that date, and
still not the measure the target is set in — and made no L or K error in
either run, the one capped to the skill and three shapes included.
Each draft had four errors: the standard one a second unconditional edge (C010)
and three templates written where an expression goes (`{{outputs.x}}` inside
an `expr:`, C040 ×2, and inside a loop cap, C033 — D, the plan's own example);
the capped one exhaustiveness (C012), a condition's field (C014) and
`{{outputs.loop.iteration}}` for `{{loop.<name>.iteration}}` (C029 ×2 — the one
rule the skill did not state; its line landed with #1336). Every one was fixed
in one `validate` round with no further reading. Against the targets, by the
classing of the rows above, the two runs miss on OPPOSITE axes: the standard
run holds S (C010, one) and misses D (C040 ×2 and C033 — three against a
target of zero); the capped run holds D (none) and misses S (C012, C029 ×2
and C014 — four against at most two). Errors only: the two warnings each run
carries (C145, an S; C137, an L) are counted apart, as their rows keep them.
The standard draft's `--strict` dry run then refused deliberately at its entry
gate on `vars.release_tag`, the gallery's idiom, with no way to hand it a
value — which is why `validate --var` exists (#1332). The artifacts of both
runs sit in the operator's probe dossier and are not attached to #1110: the
comment of the day offers them on request.

### 2026-09-22 — the YAML twin against the `.bot`, on the prototype (lot 5, PR B)

The bar, set before the measure (lot 5's plan, § 7): the twin is worth a
surface if a mid-size model writing it does **not worse than the `.bot`** on
errors, rounds and lines read — same day, build, model, spec, checklist and
capped context — with three repetitions when the numbers are close. Six runs
of each were made; the three that count on the YAML side are those of the
explainer's second version (the first version's own example wrote a
`command:` holding `: ` unquoted, which is not YAML, and two runs copied it —
the rows above keep them as the measure of that trap).

| measure, mean of three | YAML twin (runs 4–6) | `.bot` control (runs 1–3) |
|---|---|---|
| errors at the first draft | 4.0 (11, 1, 0) | 5.3 (6, 9, 1) |
| of which parse-level (E-codes) | 2.3 (6, 1, 0) | 0.3 (1, 0, 0) |
| rounds to green | 2.7 (4, 2, 2) | 3.3 (5, 3, 2) |
| lines read | 821 | 912 |
| minutes to the first draft | 2–3 | 2–5 |
| requirements met, read on the final (grep, quoted or bare values) | 30/30 | 30/30 |

**Verdict: GO for the surface (PR C), with what the measure taught written
into it.** Not worse on the three measures; the reading is shorter because
the YAML shapes carry no comments (232 lines against 431), not because the
grammar is smaller. What the twin does NOT do is remove the lexical class the
plan hoped would vanish "by construction": it moves it. Two of six runs fell
into YAML's own trap — a plain value holding `: ` (a shell command with a jq
filter) reads as a nested mapping and the whole document is refused, and the
scanner's message ("mapping values are not allowed in this context") sent the
model round in circles for four and six rounds — and one run nested quotes
badly inside an edge string's `with` map. Both surfaces share the other
parse-level mistake: an operator in a bare condition or a bare loop cap
(`when ready && not time_ok`, `as fix(vars.max_fix_passes - 1)`), which the
`.bot` refuses as "unexpected character" — the runs of the 16th had not made
it. Three things ship because of this measure: the converter's YAML syntax
errors carry the spelling that avoids them ("quote the whole value"), the
lexer's E001 on an operator character says "an operator belongs in a quoted
expression", and the twin's documentation (PR C) states the two rules a YAML
author has to know that a `.bot` author does not — quote a value holding
`: ` or ` #`, and an edge string's inner quotes are the `.bot`'s. The runs'
other guesses — the loop cap written as an output field, `expr:` as an
expression, exhaustiveness of `when` siblings — are the same the `.bot` runs
made, and the diagnostics fixed them in one round. Artifacts: the drafts,
finals, validate outputs and ledgers of the nine runs, the two explainers and
the YAML shapes are assembled as an archive of 106 files, scanned for
credentials; its upload awaits an authorization on the operator's side, so
none of the nine runs is attached to #1110 yet.

## Reporting

Attach to the ticket that changed the skill, the gallery or the diagnostics:
the table above with the new rows, the archived artifacts, and one paragraph
per spec on what the agent still had to guess. A rule an agent had to guess
is a line missing from the skill; that line is the next change.
