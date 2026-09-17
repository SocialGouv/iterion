# Themis — `arbitrate` run bilans

Doctrine-bound judge for the divergence cases a modernisation programme
leaves blocked. See [bots/arbitrate/](../../bots/arbitrate/).

## 2026-09-17 — profile 2: the judge prompts are untouched by a programme with nothing to arbitrate (run 01a0af4f-2920)

- Status: **the preflight only** — on a programme with no blocked case and no doctrine, `case_read`
  refused and the judge never ran.
- Versions: bot arbitrate 0.1.1 (`dsl: 2`, wave 3a of #1344) · iterion `db8dbb8eb` (branch build v3.154.1 + the wave-1 runtime fix), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records.
- Method: CLI `iterion run <bundle>/main.bot` launched FROM the scratch copy Campy used (a one-lot
  `.modernize/plan.yaml`, no `.modernize/ARBITRAGE.md`), `--store-dir` the operator's workspace
  store, `--sandbox none`, caps `--max-cost-usd 2 --max-duration 10m`.
- Result: `case_read` (a Python tool node) exited 1 twice and the run **failed**; $0.
- Value: what wave 3a proves for this bot is the dry run — `validate --exec --strict` identical
  before and after the migration but for C144 — and the migrator's verification that only the
  two judge prompts' six blank lines change; the judge's live rendering needs a programme with a
  blocked lot, its divergence report and the repository's arbitration doctrine, which no scratch
  fixture of this wave carries.
- Findings / misses: the refusal is a plain script failure, not a typed `fail` node — the bot's
  own choice, older than this wave.
- Engine hardening: none needed.
- Lessons for next run: build the fixture with modernize itself (a lot that blocks) before
  launching the judge.
