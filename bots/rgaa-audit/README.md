# rgaa-audit (Acci) — universal RGAA 4.1.2 accessibility auditor, read-only

Acci statically reviews a project's UI source (HTML, JSX/TSX, Vue, Twig, CSS)
against the **106 RGAA 4.1.2 criteria across 13 themes** (WCAG 2.1 AA basis),
guided by the bundled `rgaa-criteria-*` skills and — when the target uses the
Système de Design de l'État — the DSFR MCP tools as the reference markup. Each
applicable criterion is scored **C** (conforme) / **NC** (non conforme) / **NA**
(non applicable), every NC is classified 🔴 blocking / 🟠 major / 🟡 minor, and
the run produces a dated Markdown conformance report under `report_dir` plus
(optionally) one board issue per non-conformity, labelled by severity, theme and
criterion.

Static analysis only: it reads source code, it never launches a browser or a DOM
scanner. Criteria that need a rendered page are marked NA with the reason
"requires runtime verification" rather than guessed conformant. It reports; it
does not fix.

## When to use it

A read-only RGAA conformance pass on a web UI codebase — pre-release review or
recurring conformance tracking. For **fixing** accessibility issues, use Willy
(`whole-improve-loop`) with the `rgaa` preset.

Against its sibling Ally (`bots/ultra11y`), the split is **who finds the
non-conformity**: Ally's findings come from a static engine (78
machine-detectable checks tied to the WCAG 2.2 success criteria), reproducible
without a model in the loop, and it has a PR diff mode. Acci's findings come from
the **agent**, reading the UI source theme by theme against the RGAA criteria;
its deterministic gates check that the audit happened, they do not produce the
findings. Reach for Acci when the value is RGAA theme-by-theme reasoning over a
DSFR UI, or for a whole-repo campaign (Acci has no diff mode); reach for Ally
when a finding must survive without a model, or per-PR. Neither supersedes the
other.

## How it runs

Single pass — the deterministic gates are the report's integrity, so there is no
continuation loop.

```
inventory     TOOL, deterministic  bounded repo listing (top-level entries +
                                   manifest/config files; vendor/build/cache
                                   pruned, depth ≤ 4, capped at 400)
campaign      AGENT (claude_code)  phase 0 classifies the surface (has_ui /
                                   frameworks / uses_dsfr / ui_paths), then
                                   audits theme by theme → candidates[],
                                   examined_files, conformity_pct
scan_health   TOOL, GATE           hard-fails when the rgaa-criteria-* skills
                                   are absent, or when a UI surface exists and
                                   the campaign examined zero files
cap_findings  TOOL, deterministic  severity-sorted top-N overflow guard
report_card   AGENT (claude_code)  writes <report_dir>/rgaa-<YYYY-MM-DD>.md
                                   (incremental suffix on collision) and, when
                                   post_to_board, one board issue per NC
done
```

`scan_health` is the anti-façade guard: a broken setup exits non-zero instead of
reading as a clean bill of health. Both it and `cap_findings` count NC by the
candidate's `status` field and fail safe — a finding-shaped entry (severity or
problem present) with a missing status counts as NC, because surfacing beats
dropping.

## Configuration

| Var | Type | Default | Meaning |
|---|---|---|---|
| `workspace_dir` | string | `${PROJECT_DIR}` | the repo to audit |
| `scope_notes` | string | `""` | free-text scope hint from the operator or dispatched issue; empty = audit the whole UI |
| `scope_globs` | string | `""` | optional focusing globs, comma-separated and relative; empty = the whole workspace |
| `report_dir` | string | `${PROJECT_DIR}/audits` | where the dated conformance report lands |
| `post_to_board` | bool | `true` | `false` = write the report only, create no board issues |
| `findings_cap` | int | `80` | keep at most this many NC as board issues (severity-sorted); the rest stay summarised in the report |

Models and effort are env-tunable, set inline on the node `model:` fields:
`ITERION_RGAA_MODEL_REVIEW` / `ITERION_RGAA_EFFORT_REVIEW` (default
`claude-opus-5` / `high`) on `campaign`, and the `_REPORT` pair (`claude-opus-5`
/ `medium`) on `report_card`. `ITERION_RGAA_MAX_DURATION` (default `2h`) tunes
the budget, alongside `max_cost_usd: 20` and `max_tokens: 2000000`.

## Invocation

```bash
# Whole-repo audit, report + board issues:
iterion run bots/rgaa-audit/main.bot --var workspace_dir=$(pwd)

# Focused, report only (contains side-effects — useful for a dogfood run):
iterion run bots/rgaa-audit/main.bot \
  --var scope_globs='src/**' \
  --var scope_notes='audit the checkout form components' \
  --var post_to_board=false --var report_dir=/tmp/rgaa

# On-demand from the board / chat:  /acci <scope notes>   (alias /rgaa-audit)
```

Declared `invocations:` — the `acci` command (alias `rgaa-audit`, any scope,
minimum replier role `developer`, args into `scope_notes`), a board-mode schedule
suggesting `0 4 * * 1`, and plain board dispatch.

## Notable

- **Board** — `capabilities: [board.create, board.label, board.read]`, granted on
  `report_card`. Issues are created in state `ready` with labels
  `severity:<blocking|major|minor>`, `theme:<n>`, `criterion:<n.n>` and
  `source:rgaa-audit`.
- **No `sandbox:` and no `worktree:` block** — it takes the platform default
  sandbox and audits the workspace in place. `campaign` is `readonly: true`; the
  only file the bot writes into the target is the report under `report_dir` (a
  neutral repo-root directory the operator can gitignore).
- **Backend** — `default_backend: claude_code` on both agent nodes, required so
  the bundled skills are mirrored into `.claude/skills/` and the board MCP tools
  are reachable.
- **Skills** — `rgaa-audit` (workflow, C/NC/NA scoring, priority grid, report
  format), the five `rgaa-criteria-*` splitting the 13 themes, `rgaa-dsfr` (DSFR
  as the accessible-markup baseline) and `iterion-board`.
- Run history and validation status: [`docs/bot-runs/rgaa-audit.md`](../../docs/bot-runs/rgaa-audit.md).
