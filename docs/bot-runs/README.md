[← Docs](../README.md)

# Bot runs — validation & knowledge base

This directory is the **committed knowledge base** for iterion's catalog bots.
Every time a bot is dogfooded with a real run, the operator distils the outcome
into a dated **bilan** in `docs/bot-runs/<bot>.md` (named by bot **directory**,
not persona). The next person to launch that bot reads its file first — what it
caught, what it missed, what to change, and which engine bugs the run surfaced.

A bilan is durable, reviewable in a PR, and shows up in `git log`. It is one of
**three complementary knowledge channels** — do not confuse them:

| Channel | Where | Scope | Lifetime |
|---|---|---|---|
| **Workspace memory** | `~/.iterion/projects/<key>/memory/` (gitignored) — see [memory-and-knowledge.md](../memory-and-knowledge.md) | per-operator scratch, "what did we learn this session" | local to one machine/operator |
| **Board issues** | native kanban (`.iterion/`, gitignored) | open tasks / findings to act on | until closed |
| **Bilans (this dir)** | `docs/bot-runs/<bot>.md` (committed) | durable lessons the next operator must read before launching the bot | forever, in git history |

Cross-bot lessons (Goodhart, façade patterns, asymptote rules) live in
[workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md), not here —
this directory is **per-bot**.

## Bilan template

Append one dated section per run to `docs/bot-runs/<bot>.md` (newest first):

```markdown
## YYYY-MM-DD — <short label> (run <id-prefix>)
- Status: validated | partial | failed
- Versions: bot <manifest version> · iterion <git sha>
- Method: backend(s)/model(s), budget, key --vars, flags (--merge-into, post_to_board, sandbox image)
- Result: converged? iterations, cost $, duration, where commits landed (branch/sha)
- Value: the high-value thing it actually produced (or: low value + why)
- Findings / misses: what the bot caught or missed
- Engine hardening: iterion bugs found → commits/ADRs
- Lessons for next run: what to change (vars, prompt, scanner, skill)
```

The run artifacts (`.iterion/runs/<id>/`) are gitignored, so the bilan is the
only committed trace. Regenerate the full chronological run report any time with:

```sh
iterion report --run-id <id> --output /tmp/<bot>-<id>.md
```

and cite the run-id in the bilan so it can be reconstructed.

## Index

Persona → bot directory, with current validation status. Add the link when the
first bilan for a bot lands.

| Persona | Bot | Kind | Bilan |
|---|---|---|---|
| Nexie | `whats-next` | orchestrator / board triage | [whats-next.md](whats-next.md) |
| Triagy | `issue-triage` | auto-triage of fresh board cards (stamps the handler bot) | [issue-triage.md](issue-triage.md) |
| Appy | `app-dev` | greenfield app-from-prompt (interview / free first draft) | [app-dev.md](app-dev.md) |
| Evoly | `evolve` | strategic vision + evolution proposals (per-bot memory) | [evolve.md](evolve.md) |
| Willy | `whole-improve-loop` | whole-repo review-fix loop | [whole-improve-loop.md](whole-improve-loop.md) |
| Billy | `branch-improve-loop` | branch-scoped review-fix loop (commits) | [branch-improve-loop.md](branch-improve-loop.md) |
| Featurly | `feature-dev` | one-shot feature dev + review loop | [feature-dev.md](feature-dev.md) |
| Testy | `test-coverage` | test-coverage augmentation + anti-façade review loop | [test-coverage.md](test-coverage.md) |
| Endy | `e2e-coverage` | matrix-anchored e2e coverage completion (claims-verified gate) | [e2e-coverage.md](e2e-coverage.md) |
| Doki | `docs-refresh` | docs↔code convergence loop | [docs-refresh.md](docs-refresh.md) |
| Wikky | `wiki-gen` | navigable OKF wiki generator/maintainer | [wiki-gen.md](wiki-gen.md) |
| Revi | `review-pr` | read-only reviewer (mono default; cross-family dual opt-in) | [review-pr.md](review-pr.md) |
| Revi (converse) | `revi-converse` | conversational PR follow-up | _not yet_ |
| Seki | `sec-audit-source` | source SAST audit | [sec-audit-source.md](sec-audit-source.md) |
| Depsy | `sec-audit-deps` | supply-chain SCA audit (real Trivy CVE floor; other malware/ecosystem signals remain partial) | [sec-audit-deps.md](sec-audit-deps.md) |
| Shieldy | `supply-shield` | global supply-chain MALWARE shield (diff-scoped, PR/push-driven) | [supply-shield.md](supply-shield.md) |
| Vulny | `supply-shield-cve` | global supply-chain CVE shield (diff-scoped, PR/push-driven) | [supply-shield-cve.md](supply-shield-cve.md) |
| Renovacy | `secured-renovacy` | dependency upgrade pipeline | [secured-renovacy.md](secured-renovacy.md) |
| Bmady | `bmady` | BMAD multi-persona human-gated delivery | [bmady.md](bmady.md) |
| Devy | `devbox-setup` | devbox.json bootstrap | [devbox-setup.md](devbox-setup.md) |
| Obsy | `instrument` | observability instrumentation (Sentry/GlitchTip errors + JSON logs; opt-in tracing) | [instrument.md](instrument.md) |
| Adry | `adr-cartograph` | ADR cartographer + completeness audit (idempotent) | [adr-cartograph.md](adr-cartograph.md) |
| Vetty | `dep-update-guard` | Dependabot/Renovate PR guard (audit + align + deterministic verify) | [dep-update-guard.md](dep-update-guard.md) |
| Acci | `rgaa-audit` | RGAA 4.1.2 accessibility audit (read-only) | [rgaa-audit.md](rgaa-audit.md) |
| Ally | `ultra11y` | engine-backed WCAG 2.2 AA / RGAA audit + PR diff mode (read-only) | [ultra11y.md](ultra11y.md) |
| Vigie | `feed-watch` | feed watch + LLM digest to chat (Huginn-style veille) | [feed-watch.md](feed-watch.md) |
| ReArchi | `adr-rechallenge` | human-gated ADR re-challenge | [adr-rechallenge.md](adr-rechallenge.md) |
| Fini | `feature-gap-fill` | gap-driven feature completion loop | [feature-gap-fill.md](feature-gap-fill.md) |
| Goldy | `golden-master` | behavioural non-regression net, falsifiable both ways | [golden-master.md](golden-master.md) |
| Morphy | `modernize` | gate-to-gate modernisation lots, oracle-protected | [modernize.md](modernize.md) |
| — | `examples/keepalive` | always-on (`overlap: keepalive`) demo + feature dogfood | [keepalive.md](keepalive.md) |
