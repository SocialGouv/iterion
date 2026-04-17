# feature_dev_review_fix_dual — session handoff

Companion doc for `examples/feature_dev_review_fix_dual.iter`. Records the current state of this workflow as exercised against carto to add the Anthropic Claude Agent SDK as an alternative (and new default) LLM provider.

## The workflow

`examples/feature_dev_review_fix_dual.iter` is an adaptation of `pr_review_fix_dual.iter` that adds an upstream feature-development phase before the existing review/fix loop.

**Role split (important):**
- **Claude Code = main worker** — planning, implementation, primary review, primary verdict, primary fix.
- **Codex = support only** — runs *after* Claude approves. Two roles: (1) second-opinion verdict, (2) fix the blockers it itself found. Codex never touches Claude's blockers.

**Pipeline (18 nodes, 20 edges):**

```
[main] planner (claude, readonly)
  → [main] developer (inherit planner)
  → commit_namer_post_dev → commit_post_dev        ← WIP commit
  → build_check_post_dev                           ← pnpm typecheck+lint+test
  → [main] context_builder → [main] reviewer (claude)
  → build_check → [main] verdict (claude)
      │
      ├─ approved → [support] codex_reviewer → [support] codex_verdict
      │                                              │
      │                               approved → done ✅
      │                                              │
      │                     not approved → [support] codex_fixer ─┐
      │                                                           │
      └─ not approved → [main] claude_fixer ───────────────────── │
                                                                  ▼
                                              commit_namer_post_fix
                                                    → commit_post_fix   ← WIP commit
                                                    → build_check_post_fix
                                                    → back to reviewer (as fix_loop(20))
```

Commits are WIP-style (before the build check), so work is never lost even if typecheck/lint/test fails after. Pattern borrowed from `rust_to_go_port.iter`.

## Project state

- **iterion repo:** [/workspaces/iterion/](/workspaces/iterion/)
- **carto repo:** [/workspaces/iterion/external/carto/](/workspaces/iterion/external/carto/)
- **Base commit (carto):** `32f7803` (`--var base_ref=32f7803`)
- **iterion binary:** `/workspaces/iterion/iterion` (rebuild via `devbox run -- go build -o iterion ./cmd/iterion`)
- **Backends on PATH:** `claude`, `codex`, `devbox` (all verified working in this environment)
- **Store dir:** `/workspaces/iterion/.iterion-carto/` (`--store-dir .iterion-carto` relative to iterion cwd)

## Current run status

The original run `run_1776432208455` **reached `done`** on 2026-04-17 — both Claude `verdict` and Codex `codex_verdict` approved. Two commits landed in carto on top of `32f7803`:

- `d3dab08 feat(llm): add claude-agent provider via Claude Agent SDK` (dev commit — 13 files, +1567/−191)
- `4b1466c docs(cli): document claude-agent as default provider in --help` (codex_fixer commit — from a blocker Codex caught that Claude missed)

Verify:
```bash
cd /workspaces/iterion/external/carto && git log --oneline 32f7803..HEAD
devbox run -- pnpm typecheck && devbox run -- pnpm lint && devbox run -- pnpm test
```

## Iterion-side improvements shipped this session

Kept in the repo, uncommitted as of last save. Touch [delegate/codex.go](delegate/codex.go) and add [delegate/codex_output_discipline.txt](delegate/codex_output_discipline.txt):

1. **Embedded output-discipline preamble** prepended to every codex agent's system prompt. Teaches frugal tool usage (Grep-first, bounded reads, denylist for `node_modules` / `dist` / lockfiles / generated `*.d.ts`). Content lives in `delegate/codex_output_discipline.txt`, loaded via `//go:embed`. User-supplied system prompts follow and can override.
2. **`inspectCodexRollout` diagnostic** — when the codex SDK's Query iterator closes without emitting a `ResultMessage` (codex's silent-death mode), iterion reads the session rollout at `~/.codex/sessions/YYYY/MM/DD/rollout-*-<thread_id>.jsonl` and surfaces a real reason in the error, e.g. `codex likely hit context window: total_tokens=298839 > model_context_window=258400`. Captures `thread_id` from `thread.started` SystemMessages.
3. **Tool-error log fix** (`contentBlocksText`) — `tool_result` blocks used to log a raw Go pointer (`[0x6cceab29c200]`); now flattens the `ContentBlock` slice to readable text (`rg: unrecognized flag --help|…`).
4. **AllowedTools → codex sandbox** (`codexSandboxForAllowedTools`) — codex's only native tool is `shell_tool` (one `exec_command`), so iterion's per-name `tools:` allowlist can't be enforced directly. Iterion now translates the intent into codex's OS-level sandbox mode: empty or contains `Bash`/`Edit`/`Write`/`NotebookEdit` → `danger-full-access`; otherwise → `read-only` (shell still works, writes are OS-blocked). Replaces the previous no-op `WithAllowedTools` call.

`devbox run -- go test ./delegate/` covers the sandbox mapping; broader `./model/ ./runtime/` suites also pass.

## Workflow file edits this session

[examples/feature_dev_review_fix_dual.iter](examples/feature_dev_review_fix_dual.iter):

- `codex_reviewer` `tools: [Bash, Read, Glob, Grep]` → `[Read, Glob, Grep]`
- `codex_reviewer` `tool_max_steps: 25` → `15`
- `codex_reviewer_system` prompt now has a **BUDGET / tool-output discipline** section (Grep-first, bounded reads ≤80 lines, denylist for generated paths). With the embedded preamble shipped above this is somewhat belt-and-suspenders; leaving both in place for explicitness.

## Feature definition (the `--var` values)

**`feature_title`:**
```
Add Anthropic Claude Agent SDK as alternative LLM provider, default to it
```

**`feature_description`:**
```
carto currently uses the Vercel AI SDK via @ai-sdk/openai and @ai-sdk/anthropic
(see src/llm.ts getModel factory and src/config.ts Provider type). We want to
add the official Anthropic Claude Agent SDK (npm package
@anthropic-ai/claude-agent-sdk) as a THIRD provider option, default to it, and
keep the two existing Vercel-based providers (openai, anthropic) working
unchanged.

Key touchpoints: src/llm.ts getModel factory, src/config.ts Provider type,
src/inspect/repoInspector.ts (currently uses generateObject and generateText
from the ai package — the Claude Agent SDK is agentic and does NOT expose the
same Vercel LanguageModel interface, so an abstraction layer is likely
required so RepoInspector can work with either backend), src/tools.ts for the
tool schemas, and CLI --provider flag handling in src/commands/*.ts. Env var
fallbacks CARTO_LLM_PROVIDER and LLM_PROVIDER should accept the new provider
value. No stderr/stdout channel regressions.
```

**`acceptance_criteria`:**
```
1) @anthropic-ai/claude-agent-sdk is added to package.json dependencies.
2) Provider type in src/config.ts includes the new option (e.g. claude-agent).
3) src/llm.ts supports the new provider alongside existing openai and anthropic.
4) RepoInspector in src/inspect/repoInspector.ts runs against the new provider
   and produces a valid RepoInspectionResultSchema output.
5) The new provider is the DEFAULT when no --provider flag and no env var is set.
6) Existing openai and anthropic (Vercel) providers remain functional, no
   regressions on existing commands.
7) CLI --provider flag and env vars CARTO_LLM_PROVIDER and LLM_PROVIDER
   accept the new provider value.
8) At least one test covers the new provider path in test/**/*.test.ts.
9) devbox run -- pnpm typecheck passes with zero errors.
10) devbox run -- pnpm lint passes.
11) devbox run -- pnpm test passes.
12) README.md and carto --help mention the new default provider and how to
    switch back to the Vercel providers.
```

## Launch command (background — for fresh runs or re-runs)

```bash
cd /workspaces/iterion && ./iterion run examples/feature_dev_review_fix_dual.iter \
  --var workspace_dir=/workspaces/iterion/external/carto \
  --var feature_title='Add Anthropic Claude Agent SDK as alternative LLM provider, default to it' \
  --var feature_description='carto currently uses the Vercel AI SDK via @ai-sdk/openai and @ai-sdk/anthropic (see src/llm.ts getModel factory and src/config.ts Provider type). We want to add the official Anthropic Claude Agent SDK (npm package @anthropic-ai/claude-agent-sdk) as a THIRD provider option, default to it, and keep the two existing Vercel-based providers (openai, anthropic) working unchanged. Key touchpoints: src/llm.ts getModel factory, src/config.ts Provider type, src/inspect/repoInspector.ts (currently uses generateObject and generateText from the ai package — the Claude Agent SDK is agentic and does NOT expose the same Vercel LanguageModel interface, so an abstraction layer is likely required so RepoInspector can work with either backend), src/tools.ts for the tool schemas, and CLI --provider flag handling in src/commands/*.ts. Env var fallbacks CARTO_LLM_PROVIDER and LLM_PROVIDER should accept the new provider value. No stderr/stdout channel regressions.' \
  --var acceptance_criteria='1) @anthropic-ai/claude-agent-sdk is added to package.json dependencies. 2) Provider type in src/config.ts includes the new option (e.g. claude-agent). 3) src/llm.ts supports the new provider alongside existing openai and anthropic. 4) RepoInspector in src/inspect/repoInspector.ts runs against the new provider and produces a valid RepoInspectionResultSchema output. 5) The new provider is the DEFAULT when no --provider flag and no env var is set. 6) Existing openai and anthropic (Vercel) providers remain functional, no regressions on existing commands. 7) CLI --provider flag and env vars CARTO_LLM_PROVIDER and LLM_PROVIDER accept the new provider value. 8) At least one test covers the new provider path in test/**/*.test.ts. 9) devbox run -- pnpm typecheck passes with zero errors. 10) devbox run -- pnpm lint passes. 11) devbox run -- pnpm test passes. 12) README.md and carto --help mention the new default provider and how to switch back to the Vercel providers.' \
  --var base_ref=32f7803 \
  --store-dir .iterion-carto \
  --log-level info 2>&1 &
```

> `base_ref=32f7803` locks the review diff to everything added since this commit. `head_ref` defaults to `HEAD` (which moves forward with each commit the workflow makes).
> `--store-dir .iterion-carto` keeps this run's state isolated under `iterion/.iterion-carto/`.

## Monitoring commands

```bash
# Follow the live log (Claude Code background tasks write to /tmp/claude-.../tasks/<id>.output)
tail -f <path-from-background-task-result>

# Latest run id
ls -t /workspaces/iterion/.iterion-carto/runs/ | head -1

# Status + checkpoint
python3 -c "
import json
run = '<run_id>'
with open(f'/workspaces/iterion/.iterion-carto/runs/{run}/run.json') as f:
    d = json.load(f)
print('Status:', d['status'])
cp = d.get('checkpoint', {})
print('Checkpoint node:', cp.get('node_id', '?'))
print('Loop counters:', cp.get('loop_counters', {}))
"

# Latest events
tail -n 30 /workspaces/iterion/.iterion-carto/runs/<run_id>/events.jsonl

# Inspect the latest verdict / review
ls /workspaces/iterion/.iterion-carto/runs/<run_id>/artifacts/
ls -t /workspaces/iterion/.iterion-carto/runs/<run_id>/artifacts/codex_verdict/ | head -1

# Commits the workflow made in carto
cd /workspaces/iterion/external/carto && git log --oneline 32f7803..HEAD
```

## Resume after interruption

```bash
cd /workspaces/iterion && ./iterion resume \
  --run-id <run_id> \
  --file examples/feature_dev_review_fix_dual.iter \
  --store-dir .iterion-carto \
  --force \
  --log-level info
```

`--force` is required when the `.iter` file has been edited since the run started (including if an agent edited it accidentally mid-run — see failure mode #6 below).

## Actual failure modes seen this session

Each item below is something this run hit, and the fix/mitigation that ended up shipping.

1. **Binary arch mismatch** — the pre-built `iterion` in the repo was a Mach-O (macOS) from another host. Linux exec produced `cannot execute binary file: Exec format error`. Fix: rebuild via `devbox run -- task build` (or `go build -o iterion ./cmd/iterion`).

2. **Stale OpenAI key in codex auth** — `codex` was "logged in" with a revoked `sk-proj-...` key; every codex node died with `401 Unauthorized`. Fix: `codex login --with-api-key` with a fresh key (or `codex login --device-auth`). iterion's codex delegate now captures stderr, so the 401 is visible in the live log.

3. **Codex context-window overflow** — `codex_reviewer` read large portions of the carto repo (plus `node_modules/.d.ts`), accumulated tool outputs across turns, and hit `total_tokens > model_context_window` (gpt-5.4 is 258k). Codex exited **without** sending `turn.completed` / `turn.failed`; iterion's old error was the unhelpful `no result message received after 3 attempts`. Fix shipped:
   - Embedded output-discipline preamble for every codex agent (see "Iterion-side improvements" above).
   - `inspectCodexRollout` now surfaces the real reason in the delegate error.
   - Workflow tightened: `codex_reviewer` tools dropped `Bash`, `tool_max_steps: 25 → 15`, and a BUDGET block added to its system prompt.

4. **iterion's tools allowlist was advisory for codex** — `WithAllowedTools` in the codex SDK is emulated via a `can_use_tool` callback that iterion never registered, so `tools: [Read, Glob, Grep]` on a codex node didn't actually block shell access. Fix: iterion now maps intent to codex's OS-level sandbox mode (`codexSandboxForAllowedTools`). Read-only reviewer intent → `read-only` sandbox → writes blocked at the kernel level.

5. **Broken tool-error log line** — `tool_result` blocks were printed as `❌ tool error: [0x6cceab29c200]` (raw `ContentBlock` pointer). Fixed with `contentBlocksText` helper.

6. **Developer agent edited the `.iter` file mid-run** — observed mtime change at 13:34:17 on the workflow file while the `developer` node was working. Caused `--force` to be required on every subsequent resume (workflow hash mismatch). Structural contents of the file stayed intact, so `--force` was safe, but this is a reminder to scope agent workspaces when possible.

7. **`codex_reviewer` caught a blocker Claude missed** — AC #12 (top-level `carto --help` must mention the new default). Claude's primary reviewer didn't actually run `pnpm carto --help`; codex did and flagged the gap. `codex_fixer` committed the docs fix (`4b1466c`). Validates the cross-model second-opinion design.

## Still open (for a future session)

- Commit the iterion-side changes in [delegate/codex.go](delegate/codex.go) + [delegate/codex_output_discipline.txt](delegate/codex_output_discipline.txt) + the test additions in [delegate/delegate_test.go](delegate/delegate_test.go).
- Consider pruning the now-redundant BUDGET block in `codex_reviewer_system` (the embedded preamble covers it generically).
- Investigate the duplicate-log-line artifact seen in the live log (every codex tool call prints twice).
- Decide whether iterion should defensively scope the developer agent's tool workspace to `workspace_dir` (would prevent failure mode #6).

## What "done" looks like (already achieved on the original run)

Both `verdict` (Claude) AND `codex_verdict` (Codex) approved. Expected final state in carto:

- `src/llm.ts` + `src/config.ts` + `src/inspect/repoInspector.ts` modified to support three providers.
- New abstraction layer at `src/inspect/backends/` (`RepoInspectionBackend` interface + `VercelBackend` + `ClaudeAgentBackend`).
- `package.json` has `@anthropic-ai/claude-agent-sdk` added.
- Test file added under `test/claudeAgentProvider.test.ts`.
- README + `carto --help` updated.
- Clean commits: `d3dab08` (feat) and `4b1466c` (docs fix).
- `devbox run -- pnpm typecheck && pnpm lint && pnpm test` passes end-to-end.
