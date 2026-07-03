# whole_improve_loop (Willy) — companion notes

Companion to the `whole_improve_loop` workflow ([`main.bot`](main.bot)).
This is a **design journal**: what the mechanism is, why it is shaped this
way, and what is still open.

## What it does (ADR-057)

`whole_improve_loop` runs an **axis-driven work-list sweep**:
`improvement_prompt` is **THE AXIS** — one determined improvement applied
across the whole codebase, **site by site, verified and committed**. It is
the operator's own proven manual Claude Code loop (write a todo work-list,
then apply + commit each item incrementally), amplified with a deterministic
per-item build/test gate and cross-family review of every change.

Examples of an axis:

- `split every source file over 600 lines into cohesive smaller files`
- `converge every hand-rolled retry onto a shared backoff helper`
- `make every public function validate its inputs the same way`
- `extract a store-agnostic streaming package`

Each is **one axis applied to every matching site**, committed site-by-site —
not "review each chunk for whatever is wrong". That open-ended review is what
the **retired** chunked loop (ADR-011 + ADR-055) did; on a whole repo it
never converged (there is always another local issue in the next chunk) and
structurally could not produce a **global, cross-cutting** change, because no
reviewer handed a slice ever held the whole system. See
[docs/adr/057-axis-driven-work-list-sweep.md](../../docs/adr/057-axis-driven-work-list-sweep.md)
for the full rationale.

## The graph

```
next_item ─(needs_enumerate)─▶ enumerate ─┐   (writes the ordered work-list)
    ▲                                       │
    │  (has an item)                        ▼
    ├───────────────────────────────▶ transform  (apply the axis at THIS site)
    │                                       │
    │                                       ▼
    │                                verify_build ⇄ verify_run   (deterministic
    │                                       │        build/test gate; red →
    │                                       │        bounded fix retry → skip)
    │                              (green)  ▼
    │                                     alt ─▶ reviewer_claude / reviewer_gpt
    │                                       │     (ONE cross-family reviewer,
    │                                       ▼      ADR-052 mono/dual)
    │                                  review_gate
    │                        (approve)  │      │  (reject w/ blocker → transform)
    │                                   ▼      ▼
    │                             commit_item  transform (bounded re-try)
    │                                   │
    │  (advance +1)              advance│  (cursor+1 for the next item)
    └───────────────────────────────────┘
    │
    └─(exhausted)─▶ re_enumerate ─(found more → append)─▶ next_item
                          │
                          └─(nothing left)─▶ mr_gate ─▶ (finalize_mr) ─▶ done
```

- **`enumerate`** (adaptive, claude_code, whole-repo, full tools) reads the
  codebase *by its real structure* — grep/glob/read, never chunks — and
  **writes** `.whole_improve_loop.worklist.json`: an ordered list of
  `{id, title, targets, change_spec}`. An item with no concrete, nameable
  target is **dropped, not guessed** (belt-and-suspenders: `next_item`
  deterministically drops target-less items too).
- **`next_item`** (deterministic tool, the sweep's **entry**) reads the
  work-list + the cursor and emits THIS item + the routing flags
  (`needs_enumerate` / `capped` / `exhausted` / `has_item`).
- **`transform`** (adaptive fixer, whole-repo context) applies the axis to
  the current item's targets, and only those.
- **`verify_build` → `verify_run`** is the **deterministic, stack-agnostic**
  build/test gate (unchanged from the retired design): an adaptive agent
  reads the `verify-build` skill and writes `.whole_improve_loop.verify.sh`
  from the repo's own tooling; a tool node re-runs it and gates on the REAL
  exit code. Red → bounded `verify_loop(3)` fix retry; still red → **skip the
  item uncommitted** (never land broken code) and advance.
- **`review`** is **ONE cross-family reviewer** (the ADR-052 mono/dual
  `condition` router + `review_mode`/`mono_family`) confirming the transform
  **correctly + safely applies the axis at this site** (correctness /
  consistency / no regression) — *not* an open-ended re-audit. Approve →
  commit; reject with a **concrete blocker** → back to `transform` (bounded).
- **`commit_item`** lands **one incremental commit per item** (`git add -A`
  incl. untracked, minus the bot's scratch files, empty-guarded); message
  `refactor(improve): <item title>`.
- **`re_enumerate`** is the **done-oracle**: when the work-list is exhausted
  it re-scans for remaining sites, appends any it finds, and the sweep
  continues; when a fresh scan finds nothing the axis is fully applied → done.

## Convergence & bounding

- **Done-oracle:** the axis is fully applied **iff** `re_enumerate` finds no
  remaining sites — a finite, monotone condition (as opposed to an
  unreachable clean-sweep streak over a whole repo).
- **`max_items` cap:** `next_item` stops the run and ships when the cursor
  reaches `max_items` (default 120), so a pathological / unbounded axis can't
  run forever.
- Every loop is **declared and bounded** (`sweep_loop`, `transform_loop`,
  `verify_loop(3)`) — `iterion validate` reports **no undeclared cycle**.

## Crash-safe / resumable

- The **work-list** is persisted to `.whole_improve_loop.worklist.json` (the
  enumerate/re_enumerate agents own it) and the **cursor** to
  `.whole_improve_loop.state` (`next_item` owns it — **add both to your
  `.gitignore`**; `commit_item` never commits them).
- `next_item` persists the cursor it is **USING** (not an eager +1); the
  advance rides the `advance` compute (`cursor+1`) on the commit/skip return
  paths. So a run that dies mid-item leaves the cursor **at** that item and a
  re-dispatch **re-processes** it — a half-applied item is never credited.
- `next_item` is the entry, so a re-dispatch **resumes mid-sweep**: it finds
  the persisted work-list + cursor and routes straight to `transform`; only a
  fresh run (no work-list) routes to `enumerate`.

(Implementation note: the advance goes through the `advance` compute rather
than reading `outputs.next_item.next_cursor` on the loop-back edge — reading a
loop-head node's own output on its back-edge returns a loop-entry-frozen
value, so the return edge reads `outputs.advance.carry_next` instead, the same
discipline the old bot used with `streak_check.carry_*`.)

## Right artifact (anti-Goodhart)

`transform`'s work lives in the **uncommitted working tree**; the commit
happens only *after* review passes. So the reviewers `git add -N .`
(intent-to-add) then diff `git diff HEAD` — **never** `git diff HEAD^..HEAD`
(the base commit, which would make them conclude "nothing was done" and loop
forever), and `commit_item` stages untracked files. See
[docs/workflow_authoring_pitfalls.md](../../docs/workflow_authoring_pitfalls.md).

## Stack- & repo-agnostic

The **axis + the repo** define the sites. No language / package-manager
literal and no iterion-specific target path appears in any var default,
command body, or schema — `enumerate` / `transform` / `verify_build` are
adaptive agents that read whatever repo they are pointed at (CLAUDE.md
"Catalog bots are repo-agnostic" + "Universal code bots"). The
`verify_build`/`verify_run` gate stays deterministic while remaining
universal: the agent writes the repo's own build/test into
`.whole_improve_loop.verify.sh`, the tool node runs it and gates on the exit
code.

## Vars

| Var | Meaning |
|---|---|
| `improvement_prompt` | **THE AXIS**. Empty = enumerate picks the single highest-value cross-cutting improvement it can name for the repo. |
| `scope_globs` | Path scope (the WHERE): comma/space-separated fnmatch globs; empty = whole workspace. |
| `scope_notes` | Free-form extra context for the enumerate/transform agents. |
| `max_items` | Hard cap on items processed per run (default 120) — the convergence backstop; also sizes the declared loop caps. |
| `review_mode` / `mono_family` | ADR-052 mono/dual topology, resolved at launch by `pkg/reviewtopology`. |
| `open_mr` / `mr_branch` / `mr_base` / `source_issue_ref` | Opt-in MR/PR path shipping the series of per-item commits. |
| `workspace_dir` | Target repo (defaults to `${PROJECT_DIR}` → the run's worktree). |

## Presets

The `presets/` frame the axis: `code-quality`, `improve-quality` (SRE),
`production-ready`, `rgaa` (accessibility), `rgpd` (GDPR). Each sets
`improvement_prompt` (and skills) so the sweep enumerates + transforms the
sites that preset targets. They compose with the sweep unchanged — a preset
is just a pre-filled axis.

## Large axes span passes (and sometimes runs)

A big axis on a large repo may exceed one run's `max_items` / budget. Such a
run makes **bounded progress**, **commits every item it lands**, and either
hits the `max_items` cap (ships what is banked) or exhausts a declared loop.
Because the work-list + cursor are persisted, a re-dispatch **resumes
mid-sweep** and keeps banking committed items. Raise `max_items` (with the
budget) to finish a larger axis in one run.
