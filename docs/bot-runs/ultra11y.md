# Ally — `ultra11y` run bilans

Engine-backed WCAG 2.2 AA / RGAA accessibility auditor (read-only), with a
pull-request mode. The ultra11y static engine produces the findings; one agent
step rules on the criteria a static pass cannot decide; the engine's own
fail-closed gates refuse the result if that ruling does not hold up. See
[bots/ultra11y/](../../bots/ultra11y/).

## 2026-08-13 — CI flake, empty-scope façade, then a full walk to fold (run 019ffa09)

- Status: **partial** — deterministic half validated on a live run; `adjudicate`
  finished (48/48); `fold` fail-closed on the agent's `normativeRef`s. The
  graph has not walked `deliver` / `publish`. Resumable.
- Versions: bot v0.1.1 · iterion `b5cfc171` · engine `ultra11y@2.32.0`
- Method: `iterion run bots/ultra11y/ --var scope_globs='studio/src/**'
  --var post_to_board=false --var report_dir=/tmp/iterion-ally-audit
  --sandbox none --max-cost-usd 15`, store in the operator's `.iterion`.
  Inherited the bot's 2h duration (the 2026-08-12 run had overridden it to 20m).
- Result: `prepare` → `static_audit` → `worklist` → `adjudicate` in **~18 min**,
  $8.42, 74 905 tokens. `fold` refused. `failed_resumable`.
- Value: two load-bearing bugs, one of them the exact empty-scope façade this
  bot exists to refuse.

### The CI "failure" was not the bot

`helm-lint` on the previous head died downloading `nats-2.14.4.tgz` with a
GitHub 503. Unrelated to Ally. Rerun went green; the follow-up push's
`helm-lint` passed in 11s.

### Finding 1 — `audit_args` as a joined string reports 0 files

A first launch (killed after ~90s) produced:

```
audit_args: studio/src/** --graph
scope: {inputs: ["studio/src/** --graph"], files: 0}
findings: 0   conformance: 100%   residual: 52
banner: The engine found no auditable file in scope.
```

`studio/src` held 959 files. `prepare` had `' '.join(args)`'d the engine argv
into a `string`; the tool-node interpolator single-quotes a string as **one**
argv; ultra11y treated `--graph` as part of the glob. Zero files is exactly
what a clean repo looks like.

Fix: `audit_args: string[]`, emit the list, not the join. Replay:

| | before | after (this run) |
|---|---|---|
| `audit_args` | `"studio/src/** --graph"` | `[studio/src/**, --graph]` |
| files | 0 | **494** |
| findings | 0 | **74** |
| residual | 52 | **48** |
| static pass rate | 100% | **20%** |

Same shape as the 2026-08-12 dogfood (496 / 74 / 48 / 20%). Guarded by
`bots/ultra11y_audit_args_test.go`.

### Finding 2 — the fold refuses the technique ids ADJUDICATE.md tells the agent to cite

`adjudicate` (claude_code / opus-5 / high) ruled all 48:

| verdict | n | notes |
|---|---|---|
| NC | 4 | 1.3.5 autocomplete, 2.1.4 character shortcuts, 3.1.2 lang of parts, 4.1.3 status messages |
| NA | 2 | 1.2.4 live captions, 2.5.4 motion actuation |
| C | 4 | 2.4.3, 2.4.4, 2.4.6, 3.3.2 — each with citations from its own evidence |
| manual | 38 | 15 `needs-rendered-dom`, 23 `undecidable` |

`fold` / `verify --apply` refused all four NCs:

```
normativeRef "H98" does not resolve to a test of wcag (fabricated?)
normativeRef "F99" does not resolve to a test of wcag (fabricated?)
normativeRef "H58" does not resolve to a test of wcag (fabricated?)
normativeRef "ARIA19" does not resolve to a test of wcag (fabricated?)
cited snippet not found in CloudReloginModal.tsx:89  (and 3 siblings)
```

The engine's own `ADJUDICATE.md` lists those W3C technique codes under
"you may cite". The gate does not resolve them. Probed: the same
adjudication, with each `normativeRef` rewritten to the criterion id
(`"1.3.5"` …) and `snippet` dropped, folds cleanly — `ok: true`,
`applied: 10`, `stillManual: 38`, zero issues.

The skill + the adjudicator's system prompt now say: cite the criterion
id, never a technique code; omit `snippet` unless it is an exact
substring of that `file:line`. Next run should walk `deliver`.

### Lessons for next run

- Do not override `max_duration` down to 20m — 18 min was enough for
  this 48-criterion SPA once the empty-scope bug was gone.
- Re-run with the `normativeRef` guidance and confirm `fold` → `deliver`
  → `publish` (still `post_to_board=false` unless exercising the board).
- The technique-vs-criterion-id mismatch is also an ultra11y engine
  contract bug (ADJUDICATE.md vs `verify --apply`); worth a report
  upstream so the worklist stops pointing at ids the gate rejects.

## 2026-08-12 — first cut, dogfooded on iterion's own studio SPA (run 019ff5ef)

- Status: **partial** — the deterministic nodes are proven on real code. The one
  agent node ran genuinely and was still working when the run hit its wall
  clock: `BUDGET_EXCEEDED: duration (1200060863250/1200000000000)`,
  `resumable: true`. The graph has not yet been walked to `done`. Not
  validated, and not a defect either — under-budgeted. The next run raises it.
- Versions: bot v0.1.0 · iterion `feat/ultra11y-bot` @ 80780a5 · engine
  `ultra11y@2.31.2` (pinned; the shipped default is 2.32.0 — see *Dependency*).
- Method: `iterion run bots/ultra11y/ --var scope_globs='studio/src/**'
  --var post_to_board=false --var report_dir=<out-of-tree> --sandbox none
  --max-cost-usd 10 --max-duration 20m`, store in the operator's `.iterion`.
  Deliberately the same scope Acci was dogfooded on (2026-07-07, run
  019f3d3b-7aea) so the two are directly comparable.

### Result — the deterministic half, measured

`prepare`, `static_audit` and `worklist` all finished in the live run; `fold`,
`deliver` and the integrity gate were additionally executed node-by-node under
`sh` against the same workspace, with the engine build that carries the
2.32.0 changes.

| | Acci, 2026-07-07 | Ally, this run |
|---|---|---|
| files examined | 22–29 | **496** |
| non-conformities | 5 | **74** |
| conformance reported | 72% | 20% (automatic static pass rate) |
| criteria left undecided | "à vérifier visuellement" | **48**, named, in `residualRisks` |
| who produced the findings | the model | the engine |

The 74 findings collapse into **5 criterion-keyed issues** — 40 occurrences
under one 4.1.2 issue, not 40 tickets:

```
[bloquant] WCAG 4.1.2 — Name, Role, Value      (40 occurrences)
[majeur]   WCAG 2.1.1 — Keyboard               (15)
[majeur]   WCAG 1.3.1 — Info and Relationships (14)
[majeur]   WCAG 1.2.2 — Captions (recommendation, non-normative)  (4)
[majeur]   WCAG 1.1.1 — Non-text Content       (1)
```

Real anchors, spot-checked against the source:
`studio/src/views/Bots/BotBuilder/index.tsx:242` `<fieldset>` with no
`<legend>`; `studio/src/views/Bots/index.tsx:170` `<input>` with no label;
`studio/src/views/Dispatcher/index.tsx:455` `<dt>` outside any `<dl>`.

This is not "Ally beats Acci". They do different work: Acci reasons RGAA
theme-by-theme with the DSFR MCP; Ally produces findings reproducible without
a model in the loop. The number that matters is the third row — 496 files
examined against 22–29 — because the engine's coverage does not depend on how
much of the codebase an agent had budget to read.

### Value

The comparison the bot was built for. Acci's own bilan records four real
findings erased because the model emitted them without the `status` field the
gates counted by. Here the finding set never passes through a model at all:
`static_audit` reads `audit-latest.json` and counts `findings[]`. That class of
loss is structurally gone.

### Findings / misses

- **The adjudication gate was characterised, not assumed.** Each case run
  against a pristine audit: a null verdict, a C/NA with no justification, an NC
  with no finding, an NC citing a nonexistent file, a `manual` with an invalid
  reason, and **criteria dropped from the adjudication** (`coverage gap`) are
  all refused, exit 1, named individually.
- **The one thing it does not catch: a `C` with a plausible but unverified
  justification.** It passes, exit 0. This is the honest ceiling of the design
  and it is written as such in `skills/ultra11y-adjudication.md` — *the gate
  cannot tell an honest C from a lazy one; you are the only check on that*.
  The `citations` field being added upstream (a C must cite an evidence anchor
  it was shown) is what closes it; re-test this line when the bot moves to an
  engine that carries it.
- **48 residual criteria are declared, never silently conforming.** No browser
  runs, so contrast, focus, zoom and reflow stay undecided by construction.
- Adjudication is slow on a 496-file surface: 48 criteria at
  `reasoning_effort: high` did not converge in the 20 minutes allowed, and the
  run ended on the duration budget (resumable). The agent was doing real work
  throughout — refuting 2.5.3 false positives on landmark regions ("2.5.3 only
  applies to controls, not regions"), grepping for captcha/paste-blocking to
  rule on 3.3.8, reading error-suggestion strings for 3.3.3, enumerating
  `label=""` + `aria-label` pairs across the component tree. It is not stuck;
  it is under-budgeted. Next run: 45m+, or narrow `scope_globs`.

### Bot hardening — three traps the run found that `validate` could not

1. **A backtick inside a `##` comment within a `command:` block closes the
   command literal.** `iterion validate` passed; the run failed at parse time
   with `E012: unknown tool property 'set'`. Same class as the double-quote
   trap recorded in `bots/review-pr/manifest.yaml` 0.5.2 — worth a catalog-wide
   guard alongside the existing `python_command_shell_safe_test.go`.
2. **A multi-word command passed through `{{input.*}}` arrives as ONE shell
   token.** `engine_cmd = "npx -y ultra11y@2.31.2"` produced
   `commande introuvable`. The bot now passes the *version* and composes the
   invocation literally in each node. Same family as the array space-join
   documented in review-pr 0.5.1.
3. **Shell globbing silently narrows a recursive scope.** Without `set -f`, the
   shell expands `studio/src/**` before the engine sees it, and POSIX `sh`
   expands the recursive form **one level deep** — the audit would have covered
   the top directory and reported a full audit. Verified directly:
   `sh -c 'for a in $ARGS'` yields 8 top-level entries without `set -f`, and
   the intact pattern with it. Every engine call now runs under `set -f`.

None of these are engine bugs; all three are bot-authoring traps that only a
real run surfaces.

### Dependency

Requires **ultra11y ≥ 2.32.0** for `prd --issues-json` (maxgfr/ultra11y#15).
This run pinned 2.31.2 explicitly to prove the graph on the published engine;
the two capabilities the older engine lacks — the issue export and the
`contract` block inside the worklist — are handled as follows: the bot's
default pin is 2.32.0, and the bundled adjudication skill carries the verdict
vocabulary itself so the agent is never dependent on `contract` being present.

### Lessons for next run

- Raise `max_duration` to 45m+ for a whole-SPA scope, or split by
  `scope_globs`. Consider `reasoning_effort: medium` for the adjudicator and
  measure whether verdict quality actually drops.
- Run once with `post_to_board=true` to exercise `publish` against a real board
  and confirm the de-dupe grain (issue titles) survives a second run.
- Exercise PR mode (`pr_url` + `base_ref`) on a real pull request, and confirm
  whether the `produces: review` hand-off publishes generically or needs
  review-pr's commit-status path.
- Re-test the "plausible C" hole once the engine carries `citations`.
