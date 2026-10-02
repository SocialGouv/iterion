# Repo-agnostic and stack-agnostic — the universality rules

Read it when touching a catalog bot's vars, prompts or scanners; enforced by the catalog tests.

## Catalog bots are repo-agnostic

Every bot shipped in `bots/` (the catalog `iterion bots list`
discovers — docs-refresh, feature_dev, whole_improve_loop,
branch_improve_loop, secured-renovacy, whats-next, sec-audit-*, …) is
a **general-purpose tool that must run on ANY target repository**, in
any language, with no knowledge of iterion's own layout baked in.
docs-refresh aligns *a* repo's docs with *its* code; feature_dev ships
*a* feature in *whatever* repo it's pointed at. iterion is just one
possible target, never the assumed one.

**The rule:** a catalog bot's `vars:` defaults, prompts, and scanners
must not hardcode iterion-specific *target-repo* facts. Concretely,
the following are violations when they appear as **defaults**:

- Code/doc globs pinned to iterion's tree — `cmd/iterion/*.go`,
  `pkg/dsl/ir/*.go`, `pkg/**/*.go`, `examples/*/skills/*.md`. Default
  to language/layout-agnostic globs (or empty = "scan the workspace");
  a specific layout is a per-run `--var` override.
- Output/cache paths under iterion's store — `.iterion/...`,
  `~/.iterion/...` written **into the target repo**. Use a neutral
  repo-root dotfile (e.g. `.docs-refresh-cache.json`) the operator can
  gitignore; never scatter `.iterion/` into someone else's tree.
- Scanners that only produce meaningful output on iterion's shape
  (e.g. gre`p`ing for cobra `Use:` literals, `Cxxx` diagnostic codes,
  or the literal `iterion <subcmd>`). Gate these **off by default**
  (empty scope glob) and document them as an opt-in specialization;
  generalising their patterns to other stacks is the bar for making
  them a default.
- Prose framing the bot AS an iterion tool ("docs-refresh's primary
  target is iterion's own documentation"). The bot's target is
  whatever repo it's run against; iterion is at most the *reference
  self-host case*.

**Not violations** (these are the *runtime*, not the target repo):
references to iterion the engine running the bot — `mcp__iterion_board__*`
capability tools, "iterion's expr / template substitution", `iterion
report` for surfacing output, `.bot` DSL syntax. The bot is
*written for* iterion; it must not be *scoped to* iterion.

**Enforcement:** `bots/catalog_universality_test.go` greps every
catalog bot's var-default block for the violation patterns above and
fails CI on a regression. When a default legitimately needs an
iterion path (rare), add it to the test's allowlist with a comment
explaining why it's universal-safe. When you touch a catalog bot,
re-read this section — the iterion repo is the easiest target to
accidentally overfit to, because it's the one you're staring at.

## Universal code bots — stack knowledge lives in skills

Catalog bots are not only repo-agnostic (layout) — they are
**stack-agnostic** (language/ecosystem). A bot is universal when adding
a new language or package manager requires **zero DSL edits**: the
stack-specific knowledge lives in the bot's **skills**, the (now
adaptive) agent reads the relevant skill and adapts to whatever repo it
is pointed at — exactly how native Claude Code works — and
**deterministic gates verify the right work happened**. This is the
companion dimension to "Catalog bots are repo-agnostic" above; a catalog
bot must clear both bars.

**The rule:** a catalog bot's DSL (`vars:`, `prompt:`, `schema:`,
`tool ... command:`) must not enumerate languages or package managers.
Violations:
- Per-ecosystem shell branches in a tool node — `case "$PKG_MGR" in
  yarn) …; npm) …; go) …`. The skill is the catalogue; the agent
  dispatches.
- Per-language tool nodes wired in fixed position — `tool
  run_go_scanners:` / `run_js_heuristics:` plus a closed router fan-out.
  One adaptive agent step, guided by the skills, replaces them.
- Closed enum booleans in a schema — `has_js: bool`, `has_go: bool`,
  `has_npm: bool`. Emit an open `langs: []` / `ecosystems: []` list.
- Hardcoded language extension globs (`*.go`, `*.py`, `*.rs`) in `vars:`
  defaults or `command:` bodies.

**The canonical pattern (skill-guided + deterministic gate):**
1. A `skills/<topic>.md` (or `skills/lang-<id>.md`) holds the
   stack-specific knowledge — how to detect the stack, which
   scanners/commands to run, how to read the results.
2. An adaptive agent node (claude_code or claw, agentic base restored —
   see "System-prompt composition" in
   [fallbacks-and-parity.md](../backends/fallbacks-and-parity.md)) reads the matching skill and
   runs the right commands for the repo in front of it.
3. A **deterministic gate** (a `tool`/`compute` node, no LLM) verifies
   coverage: the always-on floor must have produced output, and every
   detected stack must have produced its expected artifact, else the run
   degrades/fails with a visible banner. The gate is the determinism —
   not an LLM judgment, and not a closed DSL enum. (sec-audit-source's
   `scan_health` is the reference: hard-fail when the generic floor is
   missing, banner partial per-language coverage.)

This keeps the asymptote/quality guarantees intact while removing every
language/ecosystem assumption from the workflow graph. Adding Rust to a
security bot = drop `skills/lang-rust.md`; no `main.bot` or schema edit.

**Not violations** (universal infrastructure, not stack-specific tooling):
- The always-on generic floor — `gitleaks` / `trivy` / `semgrep
  --config=p/default` in sec-audit-source's `run_generic_scanners`
  (`p/default` is Semgrep's universal cross-language pack — the metrics-off
  floor, since `--config=auto --metrics=off` is rejected by semgrep; only
  per-**language** packs like `p/golang` / `p/python` are violations, which
  is exactly what `catalog_universality_test.go` matches).
- `npm install -g @anthropic-ai/claude-code` in a sandbox `post_create`
  (bootstrapping the runtime, not the target's stack).
- Prose in a `prompt:` block that *mentions* `go test` / `npm install` as
  an illustrative example — the agent picks its commands from the repo +
  skill; the example is just guidance.

**Enforcement:** `bots/catalog_universality_test.go` greps every catalog
bot's `command:` bodies and `schema:` blocks (not only `vars:` defaults)
for the stack-specific patterns above. When you touch a catalog bot,
re-read this section and "Catalog bots are repo-agnostic" — iterion (Go)
is the easiest stack to overfit to, because it's the one you're staring at.

