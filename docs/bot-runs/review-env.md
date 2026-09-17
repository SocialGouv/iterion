# Envy — `review-env` run bilans

Deploys the current commit's already-published image to the attached platform
and hands back a live https URL. See [bots/review-env/](../../bots/review-env/).

## 2026-09-17 — profile 2: the deploy prompt reaches the model with its paragraphs, no platform to deploy to (run 01a0af4f-34ce)

- Status: **partial** — the deploy agent ran and refused honestly (no image to name), the verdict
  routed the retry the bot allows, and the run's 10-minute wall ended it as BUDGET_EXCEEDED.
- Versions: bot review-env 0.1.1 (`dsl: 2`, wave 3a of #1344) · iterion `db8dbb8eb` (branch build v3.154.1 + the wave-1 runtime fix), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records · claude_code +
  claude-opus-5.
- Method: CLI `iterion run <bundle>/main.bot --var slug=greet-scratch` launched FROM a scratch copy
  of the greet project (no CI, no published image, no platform attached), `--store-dir` the
  operator's workspace store, `--sandbox none`, caps `--max-cost-usd 2 --max-duration 10m`.
- Result: preflight ok; `deploy` (89 327 tokens, **$0.41**) reported `deployed: false` — "cannot
  name an image, deploy aborted per protocol (never build or invent a reference)"; `url_gate` and
  `verdict` sent it round `deploy_loop` (max 2 retries) and the duration wall closed the run
  (`failed_resumable`, BUDGET_EXCEEDED at `deploy`).
- Value: the deploy prompt live on profile 2 — the run's `events.jsonl` carries it with
  `honestly.\n\nYOU DO NOT KNOW THE TARGET PLATFORM`, the paragraph break the profile-1 lexer
  used to fold — and the bot's refusal discipline confirmed on a workspace with nothing to deploy.
- Findings / misses: a real deployment needs an attached platform and a published image; none is
  available from this host, so the URL and health paths stay unexercised. Retrying a
  `deployed: false` whose cause is "no image at all" twice more is the bot's design
  (`max_deploy_retries`); on a hopeless workspace it only spends the wall.
- Engine hardening: none needed.
- Lessons for next run: on a workspace without an image, set `--var max_deploy_retries=0` and a
  short wall; the honest refusal is the whole result.
