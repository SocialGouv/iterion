# bmady (Bmady) — dogfood bilan

Index + template: [README.md](README.md). Newest first.

## 2026-09-17 — profile 2: the five personas and five gates walked on a scratch CLI, the commit swept the skills mirror again (run 01a0afa6-aaf4)

- Status: **validated end to end, one known sweep** — `analyst` → `elicit_brief` → `pm` →
  `review_prd` (approve) → `architect` → `approve_arch` (approved) → `select_stories`
  (S1–S3, now, WIP 3) → `dev` → `qa` (no blocker, confidence high) → `final_review` (ship) →
  `commit_changes` → done. Neither new exhaustion exit was reached (both loops approved on
  their first turn); their witness is the dry run below.
- Versions: bot bmady 0.1.1 (`dsl: 2` + the two C145 exits, wave 3b of #1344) · iterion `5fa790d82` for the bots; the engine the branch binary `v3.157.0+c31559517` (built on the wave-3a branch from main at v3.157.0; main is at v3.159.0 as this is written, one PR of engine work ahead), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records.
- Method: launched FROM a scratch greeter repository, `--var brief="Add a --lang option (en or
  fr) to the greet CLI: fr prints 'Bonjour, NAME'. Keep the standard-library-only rule of
  ADR-0001 and the existing tests green."`, `--store-dir` the operator's workspace store,
  `--sandbox none`, `--merge-into none`, caps `--max-cost-usd 1.5 --max-duration 10m`, every
  LLM node on `claude_code`/`claude-opus-5`. The five gates answered with `iterion resume
  --answer …`; the operator pauses count against the wall, so the 90 % duration guard fired
  as `dev` started and the run was resumed with `--max-duration 40m --max-cost-usd 4`.
- Result: $1.83, 23 min 18 s from launch to done including the five pauses. The analyst asked
  the right clarifications (language set, casing, exit code), the PM wrote acceptance criteria
  with the exit-code map, the architect chose a `ValueError` subclass carried through the
  existing except-ladder and named the ordering risk, the dev implemented S1–S3 with a
  `GREETINGS` table as the single source of truth plus tests asserting exit 3 and the exact
  stderr line, QA found no blocker; `commit_changes` produced `d5a39fd` on the run worktree —
  43 files, of which 16 under `.claude/skills/` are iterion's own mirror, swept in by the
  deterministic `git add -A` (the bot's comment calls it safe on the assumption that the
  repository ignores the mirror; this scratch does not). Storage branch
  `iterion/run/01a0afa6-aaf4-7549-960f-70206b791fd0` → `d5a39fd`.
- Value: the fifteen prompts' paragraph breaks reach the models — proven in the run's
  `events.jsonl` for every system prompt and all five gate instruction blocks, whose `#`
  titles and paragraphs now arrive as the authors laid them out — and the two exits hold in
  `validate --exec --strict`: the false pass (every condition false, so both loops exhaust)
  now ends `finished` through `derive_prd -> architect` and `approve_arch -> select_stories`
  where main's copy ended `failed_resumable` (LOOP_EXHAUSTED).
- Findings / misses: the mirror sweep is the commit-side twin of #1364 (the gate-side refusal
  of `?? .claude/`) — same cause, same class rule to apply (iterion's scaffold is never run
  output); recorded on #1364, not fixed in this wave.
- Engine hardening: none in this wave.
- Lessons for next run: budget the wall for the operator's own latency at five gates
  (`--max-duration 30m`); pass `--merge-into none` again on resume.

## 2026-06-14 — full BMAD flow validated end-to-end via API-driven gates (run 019ec66b)

- Status: **validated.** All five BMAD personas + all five human gates ran to a
  shipped, tested, committed feature. I drove the gates programmatically (resume
  API), not Playwright.
- Method: dedicated worktree studio :4899, `worktree: auto` (no sandbox), all
  nodes `claude_code`/opus (no claw/gpt → no forfait shape flakiness).
  `--var brief="Add a --short flag to iterion version that prints only the
  semantic version, for shell scripts"`, `merge_into=none`.
- Flow: analyst (Mary) → **elicit_brief** → pm (John) → **review_prd** →
  architect (Winston) → **approve_arch** → **select_stories** → dev (James) →
  qa → **final_review** → commit → done. Each gate paused as
  `paused_waiting_human`; answered via `POST /api/runs/{id}/resume` with the
  node's output-schema fields (`clarifications` / `action` / `approved` /
  `selected_story_ids`+`priority`+`wip_limit` / `action:ship`).
- Quality of each persona (high):
  - **Analyst** surfaced 5 genuine open questions (build-date misconception,
    `dev` un-stamped handling, leading-`v`, `--json` composition, flag spelling)
    — real ambiguities, no solutioning.
  - **PM** produced precise acceptance criteria incl. "no regression to
    RawVersion / desktop auto-updater" and "`--json` wins deterministically".
  - **Architect** correctly judged this "exposure work, not new machinery" and
    reused the existing `FullVersion`/`Version` seam (3-layer minimal design).
  - **Dev** implemented exactly to spec AND wrote tests unprompted
    (`cmd/iterion/version_test.go` + `pkg/cli/version_test.go`).
  - **qa** judge: blockers=[], confidence high.
- Result: `final_commit f452cccf` on storage branch
  `iterion/run/dawn-thrash-midnightkazoo-9da0` (not merged). The feature is
  correct: `ShortVersion()` reads only `appinfo.Version` (commit suffix
  structurally impossible), strips one leading `v`; `--short` prints it;
  `--json` takes precedence; `version_test.go` covers both; docs in
  `docs/cli-reference.md` + `CLAUDE.md`.
- Finding (confirms Doki): the commit also swept in **`.claude/skills/bmady-*.md`**
  (6 files) — the runtime skill mirror. The clone carries main's `.gitignore`
  (which lacks the `.claude/skills/` entry added on the `c082-board-emit` branch,
  commit `a9b6d671`), so Bmady's `commit_changes` included the mirror as if it
  were source. This is exactly the data-loss/noise Doki flagged and the gitignore
  fix prevents — a code bot DID sweep the mirror into its commit. Re-running with
  the gitignore fix in place would produce a clean feature-only commit.
- Lessons for next run: Bmady is reliable + high-value for a scoped feature when
  the gates are driven attentively; the all-claude_code/opus pipeline avoided the
  forfait shape flakiness that bit Seki. Land the `.claude/skills/` gitignore fix
  (already on this branch) before using Bmady on a repo where the commit matters.
