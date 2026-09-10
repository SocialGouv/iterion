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

Open `bots/review-pr/main.bot` (about 1 600 lines). Add a second reviewer
lens (`dependencies`: reads the manifest and lockfile changes and flags a
major bump) to the existing fan-out WITHOUT changing the verdict's
semantics, keep the merge gate's count deterministic, and keep every other
node untouched. Checklist: the new agent and its two edges; the convergence
node reads the new output; `iterion validate` clean; `git diff --stat`
touches only `main.bot`; the diff is under 80 lines.

### Spec 3 — repair from diagnostics

Take `examples/review-merge-gate.bot`, break it in five ways an author
would (a property renamed to a plausible one, a `when` on a non-boolean
field, an undeclared loop, a quoted ref in a `command:`, a prompt name
misspelt), give the agent ONLY the `iterion validate --json` output, and
ask for the fixed file. Checklist: the five diagnostics gone, no other
change in the file, one `validate` round.

## Baseline and targets

| Probe | Model | Lines | Errors at first draft | Tokens read | Minutes | Rounds |
|---|---|---|---|---|---|---|
| 2026-09-09, spec 1, no gallery, no template section in the skill | claude-opus-4-8 (Claude Code) | 331 | 0 | ~213k | 20 | 0 |
| 2026-09-09, spec 1, same | claude-sonnet-4-6 (Claude Code) | 215 | 0 | ~278k | 37 | 0 |

Both drafts were correct; both cost a whole session of reading before the
first line, and both agents had to guess the same unwritten rules (the
loop-exhaustion exit, the tool's stdout-JSON contract, `outputs.*` without
`with`). The targets after the registry (#1092, #1103) and the gallery
(#1110): **0 L/K/D errors, at most 2 S errors, at most 30k tokens read,
green in at most 2 rounds** — and the same on the mid-size model.

## Reporting

Attach to the ticket that changed the skill, the gallery or the diagnostics:
the table above with the new rows, the archived artifacts, and one paragraph
per spec on what the agent still had to guess. A rule an agent had to guess
is a line missing from the skill; that line is the next change.
