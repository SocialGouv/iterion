# iterion — agent instructions

iterion: build, run and orchestrate agentic AI workflows, written in a DSL —
`.bot` files ([`pkg/dsl/workflowfile`](pkg/dsl/workflowfile/workflowfile.go)
owns the accepted extension) and `.botz` bundles. Go module
`github.com/SocialGouv/iterion`, MIT. This is the one instruction file every
session loads — Codex and pi natively, Claude Code through `CLAUDE.md`.
Everything else is read on demand from [the tree](docs/agents/README.md): open
the page your task names before you start. Automated bot runs follow their
`.bot` mission and never claim or open board tickets (**Work tracking** is for
interactive sessions).

## Find before you read

- **Exact** — where is X, who implements Y, what breaks if Z changes, A→B:
  `iterion map find|neighbours|impact|path`, or the
  `local_map_*` tools of the iterion MCP server (`engine` in this repo's
  `.mcp.json`); committed indexes in [`docs/references/`](docs/references/map-packages.md).
- **Semantic** — how does X work, which docs explain this code: graphify when
  `graphify-out/` exists (MCP `query_graph`, CLI `graphify query "<q>"`) —
  operator-local, so check its commit against HEAD ([graph](docs/agents/orientation/graphify.md)).

## Setup and the gate you owe

- Tooling comes from devbox, never the host: `devbox run -- task <name>`
  (direnv works); `task` lists every command ([development](docs/development.md)).
  CLI map: [cli-reference](docs/cli-reference.md), [cloud-cli](docs/cloud-cli.md).
- `task check` — lint and the free deterministic layer (tests, goldens, studio,
  pi extension, brand, DSL and map checks) — is owed by every session; CI also
  runs `race`, `vendor-check` and `mongo-conformance`. The `live` layer costs
  real LLM money and no CI job runs it: only when a real model alone can prove
  the change — and say which target and what it cost ([proof discipline](docs/agents/testing/testing.md#the-proof-discipline)).

## Non-negotiables

- **Review loop.** Every change lands through a PR whose `revi/review` gate is
  green. Run a local adversarial round on the diff before every push: the
  findings decide the next round, the diff size caps the budget, you fix the
  findings (by hand or through another round), and commits that ship reviewed
  work carry `Adversarial-Rounds:` / `Adversarial-Model:` trailers ([the loop](docs/agents/workflow/adversarial-review-loop.md),
  [merge and gate](docs/agents/workflow/review-and-merge.md)).
- **Do not comment `/billy`** — the fixer is paused here
  ([why, and the re-arm](docs/agents/workflow/billy.md#billy-is-paused)).
- **Never hand-edit `CHANGELOG.md`.** Bump `claw-code-go` only with
  `scripts/bump-claw.sh`; first-party — improve it in its `.works/` worktree
  when a seam crosses it, then bump.
- **`.bot` traps.** A tool node's `command:` runs under `bash -c`; a `script:`
  with `language: sh` runs `sh`, so keep it POSIX ([pitfalls](docs/workflow_authoring_pitfalls.md#shell-portability-for-tool-nodes)).
  A literal `{{` in a template is `{{"{{"}}` ([delimiters](docs/dsl.md#literal-template-delimiters)).
- **Conventions.** Tests: the standard `testing` package. CLI: Cobra (one file
  per command, `cmd/iterion/`), output through `Printer`. Logs: `pkg/log`.
  Errors and tracing: extend `pkg/errtrack`, never a second tracker. Run state
  lives in `.iterion/`.
- **Pay discoveries back.** A discovery lands in the tree: content in its
  page, one line in its index. A paragraph that would grow this file belongs
  there.

## Philosophy — [long form](docs/philosophy.md), read it before arguing with a rule

1. **Maximum power, no artificial limit.** A bound with no override is a
   defect; a load-bearing limit keeps a greppable escape hatch; warn (C1xx)
   rather than reject; never silently replace an operator's explicit choice.
2. **Modularity — the Nth-variant test.** A new capability implements an
   existing seam (`NodeExecutor`, `delegate.Backend`, `tracker.Tracker`,
   `pkg/forge`, `eventbus.Bus`, …); if the next variant would need an engine
   `if`, build the seam.
3. **Cloud-native.** The same code runs single-process and multi-replica:
   elected ownership, restart is normal, idempotent reconciliation; new
   durable state ships its cloud twin in the same change.
4. **Git-native first** — the `.bot` as text, `worktree: auto`, the review
   scope, the forge gate: never degraded; where git cannot serve, add a
   parallel mechanism.
5. **Views are additive** — read models over execution and git, never a
   second source of truth.

**Backend parity:** `claw` ↔ `claude_code` are interchangeable; a capability
wired for one is wired, or typed-refused, for the other.

## Work tracking — interactive sessions

The [project board](https://github.com/orgs/SocialGouv/projects/203) is the
truth for ongoing work: every non-trivial task is an issue under an `epic:`,
as a sub-issue **and** through the `Epic` field ([mechanics](docs/board-epics.md)).

- **A — plan.** Start from the 🎯 Epics view, triage the Inbox, make statuses
  true, pick the session's ticket and **claim** it (In progress + a timestamped
  "claimed" comment naming the session). Never touch another session's claim
  without the operator's arbitration. Mid-session work becomes an issue under
  an epic.
- **B — dev.** First ask whether a catalog bot can do it and propose that
  ([dogfood](docs/agents/workflow/dogfood.md)), never impose it; otherwise
  code directly — in a dedicated worktree, never the shared primary checkout
  ([worktrees](docs/agents/workflow/worktrees.md); bot runs are out of
  scope: the engine owns their workspace).
- **C — close.** Link the evidence (PR, commit, bilan), update the status and
  release the claim: Done, or Planned with a state-of-work comment. An In
  progress ticket nobody holds is a board bug — fix it.

## The tree

[docs/agents/](docs/agents/README.md) — seven domains (workflow, engine,
backends, bots, testing, ops, orientation), one index line per page.
