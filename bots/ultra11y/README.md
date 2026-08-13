# Ally — `ultra11y`

Engine-backed WCAG 2.2 AA / RGAA accessibility auditor, read-only, with a
pull-request mode.

```sh
iterion run bots/ultra11y/                                   # audit the whole UI surface
iterion run bots/ultra11y/ --var standard=rgaa               # report against the French référentiel
iterion run bots/ultra11y/ --var scope_globs='src/**'        # focus the audit
iterion run bots/ultra11y/ --var post_to_board=false         # report only, no board issues
iterion run bots/ultra11y/ --var pr_url=<url> --var base_ref=main   # audit only what the branch introduced
```

## Why a second accessibility bot

Acci (`bots/rgaa-audit`) audits by LLM judgment inside deterministic gates.
Its own bilan records what that costs. On run `019f3d3b-7aea` the review found
four real defects, emitted them without the `status` field the gates count by,
and the report published *« 0 non conformes »* — four true findings erased
between the agent and the deliverable.

The gates were right. The **detector** was a language model, so a dropped field
was indistinguishable from a clean repo. The fix that day made the gate
fail-safe (a finding-shaped entry with no status counts as NC), which is damage
control around a structural fact: nothing between *a defect exists in the code*
and *a defect is counted* was deterministic.

Ally moves the detection out of the model's hands:

|  | Acci (`rgaa-audit`) | Ally (`ultra11y`) |
|---|---|---|
| finds the non-conformities | the model, reading source | the engine — 78 static checks tied to WCAG success criteria, measured against the W3C ACT corpus |
| a finding is | prose the model emitted | a record with a stable id, criterion, severity and `file:line` |
| the model's job | find **and** judge **and** report | judge only, on criteria a static pass cannot decide |
| the model's output is gated by | the bot's own counters | the engine's fail-closed `verify --apply` |
| the report is written by | a model | the engine, deterministically |
| PR diff mode | no | yes |
| RGAA theme-by-theme + DSFR MCP | **yes** — its strength | criteria via a pack |

Neither supersedes the other. Reach for Acci on a Système de Design de l'État
UI, where the DSFR MCP tools carry the reference markup and RGAA
theme-by-theme reasoning is the point. Reach for Ally when the findings must
be reproducible without a model in the loop, or when you want a per-PR check.

## The pipeline

One agent step. Everything that decides, counts or gates is a `tool` node.

```
prepare        DETERMINISTIC  resolve the PINNED engine — hard-fail if absent or
                              mismatched; pick full/PR mode; bounded inventory
static_audit   DETERMINISTIC  the engine runs. THE finding set. No model.
worklist       DETERMINISTIC  draft report + the adjudication worklist
adjudicate     AGENT          the ~38 judgment criteria: C / NC / NA / manual
fold           DETERMINISTIC  verify --apply — fail-closed gate ON THE AGENT
deliver        DETERMINISTIC  dated report + `check` integrity gate + issue set
publish        AGENT          board transcription (decides nothing)
```

`prepare` is deliberately the loudest failure in the bot. An engine that does
not resolve **fails the run**, because a step that let it fall through would
report zero findings — and zero findings is exactly what a clean repo looks
like.

## What it does not do

- **No browser.** The 14 criteria that need a rendered page (computed contrast,
  visible focus, zoom, reflow, content on hover) are reported as **residual
  risks**, never as conforming. Running them needs ultra11y's `scan` tier and a
  browser in the sandbox; that is a follow-up, not a silent omission.
- **No fixes, no commits.** Read-only. For fixing, use Willy
  (`whole-improve-loop`) with the `rgaa` preset.
- **Nothing written into the audited repo** except the dated report under
  `report_dir`. Every intermediate lives in `${PROJECT_SCRATCH_DIR}/ultra11y`.

## Vars

| var | default | |
|---|---|---|
| `engine_version` | `2.32.0` | the engine, **pinned**. `npx` would otherwise resolve `latest` and what runs could change with no commit anywhere |
| `standard` | `""` | `""` = WCAG 2.2 AA core; `rgaa` = the French référentiel. A new country is an engine pack, not a DSL edit |
| `scope_globs` | `""` | empty = the whole workspace |
| `pr_url` / `base_ref` | `""` | set by iterion for any bot launched on a PR; both non-empty ⇒ diff mode |
| `force_jsx` | `false` | force JSX parsing for inputs of any extension |
| `report_dir` | `${PROJECT_DIR}/audits` | where the dated report lands |
| `post_to_board` | `true` | `false` = report only |
| `findings_cap` | `80` | overflow guard on board issues, severity-sorted; the rest stay in the report |
| `report_lang` | `auto` | `auto` follows the audited repo |

Models and effort are env-tunable: `ITERION_ULTRA11Y_MODEL_ADJUDICATE`,
`ITERION_ULTRA11Y_EFFORT_ADJUDICATE`, and the `_PUBLISH` pair.

## Requirements

Node ≥ 22.18 — already in the sandbox base image, whose contract is
*"provides a working /bin/sh + git + node"*. No `devbox.json`, no custom
image, no API key. The engine is fetched by pinned version at run start;
sandbox network mode is `open` by default.

## Hand-off

Declares `produces: kind: review` (from `deliver`) so a fixer picks up an
accessibility review by KIND, and `consumes: kind: review_ledger` so a fixer's
pushback on a PR reaches the next audit. Neither manifest names the other bot.

## Skills

- `ultra11y-engine` — driving the engine, the run directory, exit codes, gates
- `ultra11y-adjudication` — the per-criterion decision protocol
- `iterion-board` — the capability-gated board MCP tools
