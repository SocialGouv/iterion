---
name: repo-survey
description: Read-only survey method for Nexie — a tight single-pass survey for scoped questions, and a 3-audit parallel fan-out (docs/ADRs, code gaps, operational state) with structured briefs for roadmap-scale questions.
---

# Repo Survey — single pass or audit fan-out

You survey to GROUND recommendations, never to dump. Everything here is
read-only: `read_file`, `glob`, `grep`. Bash is runtime-denied so the action
policy cannot be bypassed with curl or a direct store edit. Never
write, commit, push, or mutate the board from a survey.

**Anchor every read/glob/grep path on the workspace, never on your cwd.** The
run may start in a different tree. A relative-path read in the wrong tree
produces verdicts that are right for the wrong tree, which is worse than no
verdict.

Two regimes. Pick by the size of the question, not by keywords:

- **Single pass** — the operator asks a scoped question ("what does this
  repo need?", "is the board stale?") or the board is empty and one
  direction is obvious. One surveyor (you), ≤25 tool calls.
- **Audit fan-out** — the operator asks a roadmap-scale question ("quels
  sont les prochains chantiers ?", "où va-t-on ce trimestre ?", "étudie
  le projet"). Three read-only sub-agents in parallel (the Task tool),
  one per audit axis; you stay in your own context and synthesise.

A small question gets a small survey. The fan-out triggers on scale.

## Single pass (≤25 tool calls)

Go deep on what matters, skip the rest:

1. **Top level** — glob the workspace's first-level entries;
   classify roles (code / tests / docs / tooling / examples / infra /
   vendored / runtime-data).
2. **Convention files** — `README.md`, `CLAUDE.md`, `CONTRIBUTING.md`:
   product framing, build/test entry point.
3. **Stack** — build files (`Taskfile.yml`, `devbox.json`, `go.mod`,
   `package.json`, `Cargo.toml`, `pyproject.toml`…): languages +
   package managers.
4. **Bots** — `.bot`/`.botz` files outside vendor dirs (paths, not just
   names). Empty is a valid signal on a non-iterion repo.
5. **ADRs / architecture docs** — never recommend work that contradicts
   an accepted ADR; surface tensions instead.
6. **TODO/FIXME hotspots** — first-party only
   (`--exclude-dir=vendor --exclude-dir=node_modules`), report
   hotspots, not every marker.

Report conclusions in your own reply, cited compactly (`file:line`,
file reference). There is no output schema — you are speaking to the operator.

## Audit fan-out (roadmap-scale)

Spawn THREE parallel read-only sub-agents with the Task tool — one per
axis. Keep your own context clean for the synthesis. Do not ask
permission to spawn them; the fan-out is the method.

**Await the audits within the SAME turn** (blocking task wait):
background sub-agents do NOT survive the turn boundary — ending the
turn with audits still in flight forfeits them. If you inherit a
session where that happened, say so and relaunch the missing audits
rather than stalling or inventing their conclusions.

The three canonical axes:

- **docs-adr** — docs/ADR follow-ons: accepted ADRs whose
  implementation drifted, follow-ups explicitly deferred ("later",
  "phase 2", "out of scope for this ADR"), contradictions between
  ADRs, reference docs that describe code that no longer exists.
- **code-gaps** — first-party TODO/FIXME/XXX hotspots, deprecated
  markers, half-wired seams (interfaces with one lonely impl, feature
  flags never flipped), dead or unread config, skipped tests.
- **operational-state** — the board state-by-state (via the board
  tools BEFORE spawning, or handed to the sub-agent as text), bot
  manifests vs what the catalog says,
  schedules/dispatcher config present in the repo, open PRs if a forge
  remote is visible.

Adapt an axis when the workspace demands it (a docs-only repo has no
code-gaps axis; a monorepo may need a fourth axis per subsystem) — but
say in your reply which axes ran.

### The audit brief (per sub-agent)

Compose each Task prompt from this template — the envelope is the
contract that lets you synthesise without re-reading their transcripts:

```
You are a read-only audit sub-agent for a roadmap study.

Area: <docs-adr | code-gaps | operational-state | …>
Workspace: <absolute path>
Baseline (read if present): <CONTEXT_BRIEF.md path> · <findings/ dir path>

Tools: read_file, glob, grep. Bash is unavailable by design.
NEVER write, commit, push, or mutate anything.
Budget: ≤20 tool calls, then reply.

Return EXACTLY this markdown envelope, nothing before or after:

## Summary
5-8 lines. Key observations, in the operator's language.

## Findings
5-12 bullets: "<one-line finding> — evidence: <file:line>"

## Candidate chantiers
2-4 bullets: "<short name>: <one-sentence why> — evidence: <ref>"

## Gaps in scope
0-3 bullets: what you did NOT get to survey (out of scope or budget).

Rules: every claim carries concrete evidence — no vibes. DESCRIBE,
don't recommend; the synthesis is not your job.
```

### Composing the three reports

- A candidate chantier cited by **≥2 axes** is high signal — merge the
  bullets into ONE named chantier.
- Never paste the raw reports into your reply. Cite the top findings
  inline (`file:line`, sha) and keep the rest in your context.
- Honour `## Gaps in scope` — they feed the blind-spots list, and your
  reply must be honest about what was NOT surveyed.
- Cross-reference workspace memory (see `session-continuity`): open
  threads in `CONTEXT_BRIEF.md` that a finding resolves get surfaced
  ("le fil X est clos par <sha>"); findings that reopen a thread too.

### Blind-spots checklist

After the synthesis, walk this list. Any axis NO audit touched is named
explicitly as an angle mort in your reply — the operator decides to
punt, ticket it, or dispatch a targeted audit:

adoption/users · public docs & onboarding · security posture ·
GDPR/privacy · backups/DR · cost trend · release/versioning ·
ecosystem/dependency risk.

The tiering, quick-wins and top-3 that follow the survey are specified
in `roadmap-synthesis`.

## Findings inbox (cross-bot handoff)

Other bots post **findings** — things they noticed but did not treat —
as board issues in state `inbox` (sentinel label `findings`), and as
markdown files in the workspace-memory `findings/` scope (see
`session-continuity`). During any survey:

- `list_issues {"state": "inbox"}` — fold relevant entries into your
  conclusions as `[finding:<source>:<kind>] <issue_id> — <title>`,
  keeping the full id (a later promotion needs it).
- Read `findings/*.md` when present; cite, don't copy.
- Empty inbox → no bullets. Do NOT manufacture findings, and do NOT
  mutate inbox issues during a survey — promote-or-close decisions
  happen later, with the operator.

## What a survey never does

- Recommend a bot or dispatch anything (that's synthesis + arbitrage).
- Mutate files, git state, or the board.
- Pad conclusions beyond the evidence — a thin repo yields a thin
  survey, and saying so is the correct output.
