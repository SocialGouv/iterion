# 🤖 Bots and examples

Iterion ships a maintained fleet of production bots — a co-CTO, a feature
builder, security auditors, a dependency upgrader, and more — each a
declarative `.bot` you can run, inspect, or fork. The repository keeps two
things deliberately separate:

- [`bots/`](../bots/) contains the maintained bot catalogue. Each bot is a folder with a `main.bot`, a `manifest.yaml`, and any skills or resources it needs.
- [`examples/`](../examples/) contains focused DSL and integration demos. They are teaching fixtures, not the product bot catalogue.

Iterion runs `.bot` sources and packaged `.botz` bundles. Other workflow extensions are rejected consistently at the CLI, server, dispatcher, and studio boundaries; [`pkg/dsl/workflowfile`](../pkg/dsl/workflowfile/workflowfile.go) is the source of truth.

## Embedded dispatcher catalogue

The release binary embeds these nine general-purpose bots for zero-config dispatcher use. The checked-in folders remain the editable sources; `task templates:dispatch-bots` refreshes the embedded copies.

| Persona | Bot | Purpose |
|---|---|---|
| 🧭 Nexie | [`whats-next`](../bots/whats-next/) | Conversational co-CTO: inspect the repo and board, recommend work, curate tickets, and dispatch the right bot. |
| 🛠️ Featurly | [`feature-dev`](../bots/feature-dev/) | Deliver one feature through an adaptive implementation campaign and deterministic verification gates. |
| 🌿 Billy | [`branch-improve-loop`](../bots/branch-improve-loop/) | Review and improve the branch diff, committing verified fixes in stride until convergence. |
| 🌍 Willy | [`whole-improve-loop`](../bots/whole-improve-loop/) | Apply one cross-cutting improvement axis across an existing codebase, site by site. |
| 📚 Doki | [`docs-refresh`](../bots/docs-refresh/) | Align documentation with code using one adaptive campaign, advisory drift hints, and a deterministic Markdown-only scope gate; never edits code. |
| 🔎 Revi | [`review-pr`](../bots/review-pr/) | Read-only pull/merge-request review with grounded findings and forge/board publication. |
| 🛡️ Seki | [`sec-audit-source`](../bots/sec-audit-source/) | Read-only source security audit using SAST, secret scanning, triage, and false-positive memory. |
| 📦 Depsy | [`sec-audit-deps`](../bots/sec-audit-deps/) | Read-only dependency malware/CVE audit with a cross-run package cache. |
| ⬆️ Renovacy | [`secured-renovacy`](../bots/secured-renovacy/) | Multi-stack dependency upgrades with security, compatibility, build, and review gates. |

`iterion dispatch` also exposes a `default` alias. Run `iterion bots list` to inspect the catalogue visible from the current workspace.

## Additional maintained bots

These bots are shipped in the repository and are discoverable by the CLI and studio when `bots/` is in the search path, but they are not copied into the zero-config dispatcher template:

| Persona | Bot | Purpose |
|---|---|---|
| 🗺️ Adry | [`adr-cartograph`](../bots/adr-cartograph/) | Reconcile implemented architectural decisions with `docs/adr/` and surface decision gaps. |
| ⚖️ ReArchi | [`adr-rechallenge`](../bots/adr-rechallenge/) | Human-guided re-evaluation of an existing ADR: keep, change, or append an addendum. |
| 🏗️ Appy | [`app-dev`](../bots/app-dev/) | Build a new application end to end, optionally beginning with a conversational specification. |
| ⚖️ Themis | [`arbitrate`](../bots/arbitrate/) | Judge the divergences a modernisation lot is blocked on, applying the target repository's own written arbitration doctrine and nothing else; every decision is consigned to the doctrine's journal, and an uncovered case escalates to a human. |
| 🎭 Bmady | [`bmady`](../bots/bmady/) | Human-steered analyst → PM → architect → developer → QA delivery pipeline. |
| 🧭 Campy | [`campaign`](../bots/campaign/) | Deterministic supervisor for a whole modernisation programme: runs `modernize` as a subbot lot after lot and judges progress by git, never by what a run says of itself. Carries no LLM node of its own. |
| 💂 Vetty | [`dep-update-guard`](../bots/dep-update-guard/) | Audit and align automated dependency-update PRs without merging them. |
| 🧰 Devy | [`devbox-setup`](../bots/devbox-setup/) | Create and validate a pinned `devbox.json`/lock for a reproducible project toolchain. |
| 🕸️ Endy | [`e2e-coverage`](../bots/e2e-coverage/) | Inventory an application's features from the outside, keep a committed feature×coverage matrix, and close each gap with a real e2e test in the repo's own harness. |
| 🧬 Evoly | [`evolve`](../bots/evolve/) | Long-horizon product and architecture partner backed by persistent per-bot memory. |
| 🧩 Fini | [`feature-gap-fill`](../bots/feature-gap-fill/) | Finish a structured partial-implementation gap while preserving what already works. |
| 🔭 Vigie | [`feed-watch`](../bots/feed-watch/) | Collect RSS/Atom feeds at zero LLM cost, then produce and deliver grounded digests. |
| 📡 Obsy | [`instrument`](../bots/instrument/) | Wire a repository for observability — error tracking on the Sentry DSN protocol, one standardized structured-logging seam, opt-in tracing — one verified commit at a time. |
| 🏷️ Triagy | [`issue-triage`](../bots/issue-triage/) | Single-shot card triage: read a fresh board card, classify it, stamp the handler bot + labels, and leave a routing comment. Routes work to other bots; never dispatched to. |
| 🧭 Prody | [`product-docs`](../bots/product-docs/) | Write and maintain a product's business-audience documentation in a dedicated docs repository, grounded in the source of the N repositories a product catalog names. |
| 💬 Revi (converse) | [`revi-converse`](../bots/revi-converse/) | Answer a focused `/revi` follow-up in the same forge discussion; never edits code. |
| 🌐 Envy | [`review-env`](../bots/review-env/) | Deploy the workspace's already-CI-published image to the attached platform and hand back a live https URL; the verdict is a deterministic outside probe, not the agent's word. |
| ♿ Acci | [`rgaa-audit`](../bots/rgaa-audit/) | Read-only RGAA 4.1.2 source audit with a deterministic coverage gate. |
| 🦮 Ally | [`ultra11y`](../bots/ultra11y/) | Read-only WCAG 2.2 AA / RGAA audit where a static ENGINE finds the non-conformities and the agent only rules on what it cannot decide; PR diff mode. |
| ⛓️ Shieldy | [`supply-shield`](../bots/supply-shield/) | Diff-scoped dependency-malware gate for forge events. |
| 🚨 Vulny | [`supply-shield-cve`](../bots/supply-shield-cve/) | Diff-scoped known-CVE gate for changed dependency versions. |
| 🧪 Testy | [`test-coverage`](../bots/test-coverage/) | Add meaningful regression-catching tests and verify both the suite and the new test diff. |
| 🛡️ Senti | [`vuln-watch`](../bots/vuln-watch/) | Inventory-scoped vulnerability sentinel with no LLM node at all: polls Dependabot alerts, advisory feeds, CISA KEV and EPSS, and alerts to chat only on an exploitation signal. |
| 📖 Wikky | [`wiki-gen`](../bots/wiki-gen/) | Generate and incrementally maintain a validated Open Knowledge Format wiki. |
| 🪞 Goldy | [`golden-master`](../bots/golden-master/) | Build a behavioural non-regression net (golden master) for an existing app and prove it is neither blind nor hysterical with a deterministic mutation counter-test; the safety net under a migration or refactor. |
| 🧱 Morphy | [`modernize`](../bots/modernize/) | Carry a repo through modernisation lots (toolchain/runtime/framework/datastore) one deterministic gate-to-gate step at a time, each proven by a behavioural oracle — refuses to start without a golden-master net. |

The manifests are authoritative for inputs, invocation modes, required capabilities, forge events, and suggested schedules. Nexie’s generated [bot decision catalogue](../bots/whats-next/skills/iterion-bot-catalog.md) is the routing-oriented view.

## Focused DSL demos

| Example | Shows |
|---|---|
| [`human-in-the-loop.bot`](../examples/human-in-the-loop.bot) | A `human` entry node and interaction form. |
| [`human-file-upload.bot`](../examples/human-file-upload.bot) | A `file`-typed gate field: the operator drops a file in the run console (or `--answer track=@./x.mp3`) and a tool node opens the real bytes. No LLM. |
| [`async-questions/`](../examples/async-questions/) | `interaction: async` — non-blocking `ask_user_async` questions with an `await_answers` sync point (ADR-081). |
| [`clarify/`](../examples/clarify/) | Conversational read-only facilitation with LLM interaction. |
| [`composition/`](../examples/composition/) | `group`/`use`, nested bots, and parallel sub-bot execution. |
| [`cursors/`](../examples/cursors/) | Cursor declarations and per-node calibration. |
| [`events/`](../examples/events/) | In-run `emit`/`wait` coordination. |
| [`turing/`](../examples/turing/) | Fuelled expression/loop semantics. |
| [`supervisor/`](../examples/supervisor/) | Concurrent supervisor declarations and steering. |
| [`ultracode/`](../examples/ultracode/) | Ultracode compression mode. |
| [`web-search/`](../examples/web-search/) | Tiered web-search capability use. |
| [`keepalive/`](../examples/keepalive/) | Sub-minute always-on schedule shape and overlap policy. |
| [`nested-subbots-demo/`](../examples/nested-subbots-demo/) | Multi-level child-run nesting. |
| [`pipeline-board-demo/`](../examples/pipeline-board-demo/) | Pipeline-board episode projection. |
| [`devcontainer-devbox/`](../examples/devcontainer-devbox/) | Devcontainer sandbox plus repository `devbox.json`, including non-interactive `PATH` provisioning and composition with bot-local tools. |
| [`github-actions/`](../examples/github-actions/) | Human-in-the-loop workflow from GitHub Actions. |

Other root-level fixtures exercise explicit `else`, deploy E2E, and the review/merge gate. Validate any example with `iterion validate <path>` before adapting it.
