# ADR-010: whats-next findings auto-hygiene — destructive prune on a conservative git-log match, driven from the emit_action prompt

- **Status**: Accepted
- **Date**: 2026-05-31
- **Authors**: devthejo
- **Code context**: [`bots/whats-next/main.bot`](../../bots/whats-next/main.bot)
  (`emit_action` node, `emit_action_system` steps 10–13, `emit_output`
  schema), [`bots/whats-next/skills/session-continuity.md`](../../bots/whats-next/skills/session-continuity.md)
  (findings/ scope lifecycle), [`pkg/store/storedir.go`](../../pkg/store/storedir.go)
  (`EncodeWorkDirKey`, replicated in bash), [`pkg/memory/memory.go`](../../pkg/memory/memory.go)
  (the `findings` scope path), [`pkg/botreplay/testdata/bot-goldens/whats-next/nexie_turn_basic.json`](../../pkg/botreplay/testdata/bot-goldens/whats-next/nexie_turn_basic.json)

## Context

Dispatcher-spawned bots write free-form *finding files* into the iterion
memory tree under the `findings` scope
(`~/.iterion/projects/<key>/memory/findings/YYYY-MM-DD-<slug>.md`). Nothing
ever pruned them: once the underlying bug/drift was fixed in code, the
finding lingered indefinitely, so the inbox a future whats-next session (or
the operator) reads grows monotonically with stale, already-resolved noise.

The task was to make whats-next archive findings that are demonstrably
resolved, **after** `emit_action` records the roadmap. Several choices had
non-obvious trade-offs the code alone would not justify.

### Decisions and the alternatives rejected

**1. Destructively `rm` the finding file, not move it to `findings/archived/`
with a `status: archived-by-bot` frontmatter flag.** The brief said "delete
the finding file"; a non-destructive move would have kept a tombstone but
also kept the file in the same scope's auto-index, re-polluting exactly the
inbox we are trying to keep honest, and would have forced a new required
frontmatter field onto every *writer* bot. We delete and rely on **git
history + operator backups** as the recovery net. The accepted trade-off:
an over-eager match is data loss of a still-relevant finding's prose — which
is why decision #3 makes matching conservative to the point of usually
doing nothing. `archived-by-bot` therefore names a **lifecycle state
recorded in the audit log**, not a surviving on-disk flag.

**2. Implement as extra steps in the existing `emit_action` prompt, not a
new dedicated node or a new Go `memory_delete` runtime primitive.**
`emit_action` already runs `claude_code` with `[bash, read_file,
write_file, glob, grep]`, already fires exactly once after human approval
("after emit_action lands"), and already owns the run's audit markdown — so
the hygiene log lands in the same file with zero new wiring. A dedicated
node would have needed its own schema, edges, and `plan_path` threading. A
`memory_delete` tool was considered and rejected: the memory tool set
(`pkg/backend/model/memory_tools.go`) is deliberately read/write/list only,
findings is a *different* scope than whats-next's own (`whats-next`) so the
node has no memory binding to it anyway, and a general delete primitive is a
larger, riskier surface than a single prompt-scoped `rm`. The cost: the
logic lives in a prompt (LLM-executed bash), not in tested Go — mitigated by
the conservative contract and the `archived_findings` audit trail.

**3. Confident-match-only: filename/slug verbatim, exact stable finding ID,
explicit distinctive strong keyword, or a resolution verb plus the finding's
exact title/≥3-word phrase. A bare topic keyword is never sufficient.**
Finding files may carry a stable `id:`/`finding_id:`-style field or explicit
`strong_keywords`, and exact mentions of those labelled identifiers are as
high-precision as the filename slug. Loose keyword matching ("dispatcher",
"cancel") would match unrelated commits and delete open findings, so only
metadata-marked, distinctive strong keywords qualify. Ambiguous N-to-one
matches are left in place. Under-archiving is explicitly the safe failure
mode: doing nothing costs a little inbox noise; a false delete costs a lost
trail.

**4. Derive the findings path at runtime from `input.workspace_dir`, never
hardcode the operator's `~/.iterion/projects/-home-...` path.** whats-next is
a catalog bot and must run on any host/repo (CLAUDE.md "Catalog bots are
repo-agnostic"). The bash reproduces `EncodeWorkDirKey` (replace `/ : \` →
`-`, force leading `-`) against `${ITERION_HOME:-$HOME/.iterion}`, matching
the same workDir the memory subsystem uses, so the resolved scope is
identical to the one the writers used. Empty/missing/empty-dir → clean
no-op. The literal path in the task brief is treated as runtime state to
*derive*, not a constant to bake in.

**5. Add `archived_findings: json` to `emit_output` (structured audit) and
patch the frozen golden fixture, rather than logging only to markdown.**
`model.ValidateOutput` treats every schema field as required, so the
hand-authored `emit_action_basic.json` golden gets `"archived_findings":
[]` added (correct: that scenario's `workspace_dir` is empty → hygiene
no-ops). This keeps the bot golden-replay green without a live re-record and
gives the operator a machine-readable record alongside the markdown line.

## Consequences

- Enforcement is **conservative by design**: on a typical repo almost
  nothing matches, so `archived_findings` is usually `[]`. That is the
  intended behaviour, not a bug — the pass earns its keep only on findings
  whose commit explicitly names them.
- The delete is **irreversible on disk**; recovery is via git history and
  backups. The audit line is written and verified before deletion, and the
  `## Findings archived (auto-hygiene)` heading is created on the first
  archived (or dry-run) match, so a successful prune has a durable one-line
  trail. Operators who want zero deletion set
  `ITERION_WHATS_NEXT_FINDINGS_HYGIENE=off` (or `dry` to preview matches in
  the markdown without deleting).
- The matching logic is **prompt-resident**, so it is exercised by live runs
  and the golden's no-op path, but not by a dedicated Go unit test. The
  `archived_findings` array + the plan-markdown section are the audit trail
  that makes a wrong delete visible after the fact.
- **Existing findings handoff is untouched.** Writer bots keep their
  frontmatter contract (no new required field); the separate board-`inbox`
  findings mechanism (explore node, `emit_action` steps 7–9) is unchanged.
  The only behavioural delta is whats-next's own `emit_action` gaining a
  final, additive, non-blocking prune pass.
