# State of the art — how proven is each surface

<!-- Three headings below carry no emoji on purpose: they are linked by anchor from
     backends.md and board-epics.md, and a leading emoji slugs differently on
     GitHub (leading hyphen) than in the VitePress site. -->

_Last measured: **2026-09-18**._

[`current-state.md`](current-state.md) answers *what ships*. This page answers
a different and harder question: **how proven is it?** A capability can be
implemented, documented, and reachable from the CLI while never having been
exercised against anything but its own unit test. Those two states look
identical in a feature table, and telling them apart is what this page is for.

It is the repo-side twin of the **[GitHub board's
epics](https://github.com/orgs/SocialGouv/projects/203)**: one epic per
chantier, each carrying its own state of play and its open sub-issues. This
page links out to them; they link back here. Keep both ends when you add a
row — a one-way link rots in one direction without anyone noticing.

## The maturity vocabulary

The same five marks are used on this page and in every epic body. They grade
**evidence**, not ambition:

| mark | means | what earns it |
|---|---|---|
| 🟢 **humming** | runs unattended in production | a measured run, check or schedule you can point at |
| 🟡 **running, watch it** | in production, actively changing | same, plus open work on its surface |
| 🟠 **gap identified** | works, with a named hole | the hole is written down and ticketed |
| 🔴 **blocked** | cannot progress without a decision | the decision is named, and whose it is |
| ⚪ **design stage** | not exercised yet | a design doc, not a run |

**An unknown is written `unknown`, never left blank.** A blank cell reads as
"fine" to every future reader, which is the one thing it never means.

## Backends — only two are battle-tested

The rest of the detail (resolution chain, detection rules, fallback routes)
lives in [backends.md](backends.md); this is the maturity read.

| backend | mark | proven by | not proven |
|---|---|---|---|
| `claude_code` | 🟢 | the default implementation harness — every catalog bot has run on it in production | — |
| `claw` | 🟢 | in-process twin; claw + GPT-5.6-sol proven end to end with positive **and** negative controls (#1203); anthropic + openai validated | bedrock / vertex / foundry ship but are **untested** |
| `codex` | 🟠 | used in real reviews | never run against the full feature matrix |
| `pi` | 🟠 | rpc mode, permission gate + `ask_user` + board caps via an embedded extension | the extension loads on the **rpc transport only**; `print` mode refuses gated nodes. No matrix proof |
| `kimi` | 🟠 | generic CLI-agent protocol | permission gate is **`deny` only** (C176 refuses `ask`); needs `sandbox: none`; session resume/fork **not wired**. No matrix proof |
| `grok` | 🟠 | same generic protocol | same `deny`-only gate, same unwired resume/fork. No matrix proof |
| `opencode` | ⚪ | — | not implemented — planned as a `CLIAgentProtocol` value |

**The honest statement: `claude_code` and `claw` are battle-tested; the other
four are *supported*.** They compile, they run, they have detection rules and
documentation — but none has been exercised against the whole iterion feature
set. **Structured output** (`schema:` + `output:`) is the first thing to
prove, because most of the catalogue depends on it, and it is not settled even
on the proven path.

The parity doctrine is settled and pre-arbitrated for `claw` ↔ `claude_code`
([philosophy.md](philosophy.md#backend-parity--claw--claude_code-pre-arbitrated)):
a capability wired for one is wired — or **typed-refused** — for the other. It
says nothing yet about the four CLI-agent backends, and closing that silence
is the work tracked in the Backends epic.

> **Adding a backend is a class, not a file.** The seam is
> `delegate.CLIAgentBackend` + `CLIAgentProtocol`
> ([ADR-065](adr/065-dedicated-cli-agent-backend.md)), one file per protocol
> value. But a backend is a *role on a shared entity*: it must be honoured at
> every site that selects, accepts or describes one — measured on `kimi`, that
> is **~30 non-test files** across the seam, validation, execution,
> accounting, runtime, transport, CLI, studio and docs. A backend honoured on
> 20 of those 30 is worse than none: the ten silent sites contradict the
> twenty, and nothing goes red.

**Epic:** [backends & execution parity](https://github.com/SocialGouv/iterion/issues/1404)

## 🤖 Bots — the fleet, and what it has actually done

37 bundles under [`bots/`](../bots). The catalogue and its options live in
[examples.md](examples.md); this is the maturity read, and it covers only the
bots with production evidence. **Everything not listed here is ⚪ or 🟠 by
default** — implemented and documented, not proven in production.

| bot | persona | mark | evidence, 2026-09-18 |
|---|---|---|---|
| `review-pr` | **Revi** | 🟢 | `revi/review` is a required check on `iterion` **and** `buildkit-operator`. The last 25 merged iterion PRs (#1342→#1392) are **25/25 SUCCESS**. On the `mesure-impact` binding: **10 runs / 30 days, 0 failures**, p50 552 s, p95 884 s, **$13.68** total (~$1.37 a review) |
| `feed-watch` | **Vigie** | 🟢 | **10 schedules** armed on the Ministères-Sociaux tenant (`Europe/Paris`): a twice-daily pair, a daily, and two weekly quarter-hour ladders. Fired today at 05:00Z and 06:00Z |
| `vuln-watch` | **Senti** | 🟢 | hourly (`25 * * * *`), last fire 2026-09-18T10:25Z |
| `docs-refresh` | **Doki** | 🟡 | weekly against this repo (Monday 04:00Z), opens its own MR; last fire 2026-09-14 |
| `sec-audit-source` | **Seki** | 🟡 | reaching the cloud; **5 open findings sit untriaged in Inbox** (#1322 #1323 #1324 #1328 #1333) |
| `branch-improve-loop` | **Billy** | 🟠 | proven in the gate loop, then **paused on iterion** since 2026-09-15 (`auto_fix_on_gate_failure` off — cost). `/billy` still answers on demand |
| `dep-update-guard` | **Vetty** | 🟠 | third member of the gate trio; **one ticket in 298** on this board — effectively unmeasured |

Two silences are findings in their own right, and are written up in their
epics rather than passed over: **Vigie carries one ticket out of 298** after
months on ten schedules, and the **Assistant** and **Automation** epics have
**zero open tickets**. Either those surfaces are genuinely settled, or nobody
is exercising them hard enough to file anything. Until a deliberate look says
which, treat them as unmeasured, not solved.

**Epics:** [Revi](https://github.com/SocialGouv/iterion/issues/1396) ·
[Vigie](https://github.com/SocialGouv/iterion/issues/1397) ·
[Seki](https://github.com/SocialGouv/iterion/issues/1398) ·
[the merge gate](https://github.com/SocialGouv/iterion/issues/1395) ·
[the bot catalogue](https://github.com/SocialGouv/iterion/issues/1399)

## 🚦 The merge gate — the one 🔴

`revi/review` is required on two repos. It **produces** on iterion (25/25) and
**does not produce** on `buildkit-operator`: the single open PR there,
[#17](https://github.com/SocialGouv/buildkit-operator/pull/17), comes from a
**fork**, and Revi refuses forks by design. The check is required, absent, and
unsatisfiable — so that PR can only land through an admin bypass.

**The decision is the operator's**: assume the bypass on fork PRs, or stop
requiring the check on that repo. #874 (an opt-in read-only review lane for
fork PRs) is the third path, and it is designed but not built.

**Epic:** [the merge gate](https://github.com/SocialGouv/iterion/issues/1395)

## Tests — the free layer and the paid layer

Two layers answer different questions, and confusing them is what makes
"run the e2e at every change" sound unaffordable.

| layer | what it proves | cost | when it runs |
|---|---|---|---|
| **deterministic** | the real seams, credential-free, stub executor | free | **every push and PR** — `go test ./e2e/...` (900 s) + a `-race` pass, plus `mongo-conformance`, `nats-conformance` and `cloud-e2e` on a kind cluster |
| **live** (`-tags live`) | what a real harness actually does | real LLM spend | **never automatically, by design** — 59 `task test:live:*` targets, opt-in |

[`e2e-coverage-matrix.md`](e2e-coverage-matrix.md) holds **387 rows**:

| status | rows | meaning |
|---|---|---|
| `covered-deterministic` | **361** | free, runs every time |
| `excluded` | 15 | needs a third-party tenant / cloud control plane |
| `unit-only` | 8 | an e2e would only re-test the harness |
| `covered-live` | **7** | only reachable with a real model |
| `uncovered` | 3 | real gap |

**So the free layer carries 93% of the inventory.** The paid layer is the
remaining sliver — which is what makes a per-change discipline affordable at
all: run the free layer always, and reach for a live target only when the
change touches what only a live target can prove.

The matrix is not a wish list: `TestE2ECoverageMatrixGate`
(`bots/e2e_coverage_matrix_gate_test.go`) turns an **orphan claim** — a
`covered-*` row whose cited tests resolve nowhere — into `matrix_ok=false`,
a red pass regardless of a green suite.

### 🟠 The paid layer has no memory

Measured 2026-09-18: the suite **compiles** (`go vet -tags live ./e2e/...
./bots/...` exits 0) and was last touched 2026-09-08. But the committed
quality history (`e2e/testdata/live/quality/`) holds **6 snapshots, all dated
2026-08-07**, across **2 targets out of 59**.

Read that honestly: the snapshot is written by the judge panel, which
`ITERION_LIVE_QUALITY=off` disables, so an absent snapshot does not prove an
absent run. What it proves is that **the repo keeps no durable record of when
a live target last passed** — so the question can only be answered by spending
money. That is the gap [#1422](https://github.com/SocialGouv/iterion/issues/1422)
closes.

**Epic:** [end-to-end proof](https://github.com/SocialGouv/iterion/issues/1421)

## The chantier map

All 22 epics, their mark, and where the detail lives. Counts are board items
(closed + open) as of 2026-09-18.

| epic | mark | items | what it is |
|---|---|---|---|
| 🚦 [Merge gate](https://github.com/SocialGouv/iterion/issues/1395) | 🔴 | 15 | revi × billy × vetty, and the repos they guard |
| 🔍 [Revi](https://github.com/SocialGouv/iterion/issues/1396) | 🟢 | 24 | PR review in production |
| 📡 [Vigie](https://github.com/SocialGouv/iterion/issues/1397) | 🟢 | 2 | the watch feed in production |
| 🛡️ [Seki](https://github.com/SocialGouv/iterion/issues/1398) | 🟡 | 9 | security audit on the cloud |
| 🤖 [Bot catalogue](https://github.com/SocialGouv/iterion/issues/1399) | 🟡 | 15 | 37 bundles, shapes, `dsl: 2`, dogfood |
| 💬 [Assistant](https://github.com/SocialGouv/iterion/issues/1400) | 🟢 | 13 | Copi & Nexie |
| ✍️ [DSL authoring](https://github.com/SocialGouv/iterion/issues/1401) | 🟡 | 46 | the authoring-first DSL — the largest chantier |
| 📐 [Public contracts](https://github.com/SocialGouv/iterion/issues/1402) | ⚪ | 9 | contracts & execution by ports |
| ⚙️ [Runtime](https://github.com/SocialGouv/iterion/issues/1403) | 🟡 | 24 | fan-out, checkpoints, resume, rewind |
| 🔌 [Backends](https://github.com/SocialGouv/iterion/issues/1404) | 🟠 | 18 | execution backends and their parity |
| 📦 [Sandbox](https://github.com/SocialGouv/iterion/issues/1405) | 🟢 | 18 | isolation and the permission gate |
| 🧩 [Connectors](https://github.com/SocialGouv/iterion/issues/1406) | 🟠 | 12 | connectors, MCP, plugins, skills |
| ☁️ [Cloud plane](https://github.com/SocialGouv/iterion/issues/1407) | 🟡 | 12 | tenancy, replicas, persistence |
| 🔑 [Credentials](https://github.com/SocialGouv/iterion/issues/1408) | 🟡 | 31 | BYOK, quotas, metering, cost |
| 🔗 [Forge](https://github.com/SocialGouv/iterion/issues/1409) | 🟡 | 25 | GitHub App, GitLab, Forgejo, webhooks |
| 🩺 [CI health](https://github.com/SocialGouv/iterion/issues/1410) | 🟠 | 16 | flakes, races, the merge queue |
| 🚀 [Prod ops](https://github.com/SocialGouv/iterion/issues/1411) | 🟡 | 7 | deploy, rollout, incidents |
| 🎨 [Studio](https://github.com/SocialGouv/iterion/issues/1412) | 🟠 | 13 | editor, board, run console, desktop |
| 🔄 [Automation](https://github.com/SocialGouv/iterion/issues/1413) | 🟢 | 14 | board, dispatcher, triggers, schedules |
| 📚 [Docs](https://github.com/SocialGouv/iterion/issues/1414) | 🟠 | 4 | the doc site, ADRs, this page |
| 👁️ [Observability](https://github.com/SocialGouv/iterion/issues/1415) | ⚪ | 1 | events, logs, error tracking |
| 🧪 [E2E proof](https://github.com/SocialGouv/iterion/issues/1421) | 🟠 | 4 | the free layer always, the paid layer deliberately |

## ✅ Keeping this page honest

- **A mark needs a date and a source.** "🟢 humming" with nothing to point at
  is a wish. If the evidence is older than the last release, say so rather
  than refresh the date.
- **Measure, don't infer.** A required check is not a produced check —
  `buildkit-operator` is the whole lesson in one line. A closed ticket does
  not mean a surface is exercised; a green rerun does not settle an
  intermittent failure.
- **Both ends of every link.** A row here names its epic; that epic names this
  page. Add them in the same change.
- **A discovery lands here.** When a session burns real time finding out how
  proven something actually is, the answer belongs in this table, not in the
  transcript. Operating knowledge goes to a runbook instead —
  [agents/runbooks.md](agents/runbooks.md) is the index, and
  [board-epics.md](board-epics.md) is the runbook for this board.
