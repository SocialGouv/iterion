# dogfood_editor_ui_loop — companion notes

Companion to the [`dogfood_editor_ui_loop.iter`](dogfood_editor_ui_loop.iter)
workflow. Operator-oriented: pre-requisites, how to run, what to expect,
and the open questions this pattern is designed to surface.

## Intent

This is iterion's **first UI feedback loop**. It exists to:

1. Validate that **Playwright MCP** integrates cleanly into iterion via
   the existing `mcp_server` declaration — no core code changes needed.
2. Demonstrate a **plan → act → observe-running-app → judge → fix**
   pattern that converges on the *behaviour* of the running UI, not just
   on whether the code compiles.
3. **Dogfood** iterion's own editor (`editor/`, React + Vite). The
   workflow is meant to land features on the run console, the canvas,
   the various panels — and the run console is exactly where the
   operator watches it execute.
4. Exercise the **human-in-the-loop** primitives:
   - `interaction: human` on the judge → `ask_user` escalation when the
     visual verdict is ambiguous.
   - `human` final gate node → mandatory pause for operator approval
     before the run lands its commit.

## Operator pre-requisites

These run **once per host**:

```bash
# 1. Verify the playwright MCP bridge is reachable via npx (no global
#    install needed — npx -y in the workflow auto-installs on first run).
npx -y @playwright/mcp@latest --help

# 2. Pull a chromium binary for the local user (~150MB, cached at
#    ~/.cache/ms-playwright). The MCP server uses this; without it the
#    first navigate call hangs for ~60-120s while it downloads.
npx playwright install chromium

# 3. Confirm the editor dev server starts (sanity-check pnpm + corepack).
devbox run -- task editor:dev   # Ctrl+C once you see "Local: http://..."
```

Per-run: ensure port `5173` is free. The workflow's `playwright_run`
node kills any leftover `vite --port 5173` process on entry, but a
non-vite squatter (e.g. another React project) breaks the run.

## How to run

```bash
# Smoke test — trivial, single-file feature:
devbox run -- ./iterion run examples/dogfood_editor_ui_loop.iter \
  --var feature_prompt='Add a small "Hello dogfood" badge in the top-right corner of the run console, fixed-positioned, blue background, only visible when ?dogfood=1 is in the URL' \
  --merge-into none

# Realistic feature — touches a component + the API client:
devbox run -- ./iterion run examples/dogfood_editor_ui_loop.iter \
  --var feature_prompt='Add a "Pause run" button under the run header that POSTs /api/runs/{id}/pause and disables itself while the request is in-flight' \
  --merge-into none
```

`--merge-into none` is recommended for this workflow: the diff is the
artifact, and you want to inspect the branch before fast-forwarding
your main checkout onto it. The run produces an
`iterion/run/<friendly-name>` branch you can `git diff main..` against.

## What to expect

A typical successful run looks like this on the events log:

1. `run_started`
2. `node_started: plan` — Claude Code reads `editor/src/`, emits
   `plan_output` with `files_to_modify` listed
3. `node_started: act` — same Claude Code session, now with edit tools.
   Edits the listed files, runs `tsc --noEmit` until green
4. `node_started: playwright_run` — fresh session. Bash starts vite,
   waits for `Local:`, then drives the browser through Playwright MCP
   tools. Screenshot saved under `.iterion/dogfood-screenshot.png`
5. `node_started: judge` — runs `git diff --name-only`, validates the
   diff against `plan_output.files_to_modify`, inspects screenshot
   evidence. May `human_input_requested` if confidence is low —
   answer in the run console panel, CLI, or Telegram bridge
6. `human_input_requested: human_gate` — final operator approval
7. `run_finished` — branch `iterion/run/<friendly-name>` left for review

If the judge rejects with concrete blockers, you'll see a
`fixer` node and another `playwright_run`. Bound is `ui_loop(3)`; on
exhaustion the run terminates with `failed`.

## Anti-façade design (load-bearing)

This workflow inherits the lessons captured in
[../docs/workflow_authoring_pitfalls.md](../docs/workflow_authoring_pitfalls.md)
after the goai → claw-code-go incident. Key invariants:

| Invariant | Where it lives |
|---|---|
| Plan declares `files_to_modify` upfront | `plan_output.files_to_modify` schema |
| Judge runs `git diff --name-only` and rejects scope drift | `judge_system` rule 2 + `verdict_output.diff_touches_correct_files` |
| Console errors are an automatic reject | `judge_system` rule 3 |
| Feature must be visible in the rendered DOM | `judge_system` rule 4 + `ui_observation.feature_visible` |
| Ambiguous visual verdict MUST escalate, not invent | `judge_system` rule 6 (calls `ask_user`) |

These cannot be softened by prompt: `diff_touches_correct_files=false`
is **always** an immediate `approved=false`. A convincing-looking
screenshot is not enough to ship code that doesn't touch the planned
files.

## Sandbox status

V1 forces `sandbox: none`. The dev server binds to `127.0.0.1:5173`
and the iterion sandbox CONNECT proxy doesn't currently route
loopback traffic. Phase 4 (proxy bypass for localhost) will allow
flipping this to `sandbox: auto`. Until then, run on a machine where
you trust the workflow's `act` and `fixer` nodes to edit `editor/`
files freely.

## Known limitations

1. **First-run latency** — Playwright MCP downloads chromium on first
   invocation if `npx playwright install` was skipped. Allow ~120s
   slack on the `playwright_run` budget the first time.
2. **Vite hot-reload race** — if `act` finishes a write while vite is
   mid-rebuild, the next `browser_snapshot` may catch a transient
   error overlay. The workflow handles this via the
   `confidence == 'low' && length(blockers) == 0` re-run path: a
   low-confidence rejection without concrete blockers re-triggers
   `playwright_run` (same loop counter) on the assumption it was a
   flake.
3. **Multi-modal judge** — `claude-opus-4-7` is vision-capable, but
   the screenshot is currently passed via path reference, not as an
   inline attachment. The judge reads the screenshot via its bash
   tool (e.g. `file` for metadata) but cannot truly *see* it. A
   future iteration will use an attachment-based handoff (see
   `examples/vision_attachments.iter` once written).
4. **Single MCP server** — only `playwright` is wired. Adding e.g.
   `mcp_server github` for issue context would be additive (no DSL
   changes), but each new server multiplies the agent's tool-list
   surface and demands a tighter allowlist.

## Open questions / future work

- **Vite headless mode** — for CI we'd want `PLAYWRIGHT_HEADLESS=true`
  and to skip `human_gate`. A `vars.ci_mode: bool` could swap
  `interaction: human` to `interaction: llm` and bypass the gate.
- **Anti-flake retry classification** — distinguishing a real
  rejection from a hot-reload race. Today the `confidence: low + no
  blockers` heuristic catches it; better signals (e.g. inspect the
  vite log for a rebuild marker) would reduce false-positive retries.
- **Coverage accumulation** — `vibe_feature_dev` accumulates
  `cumulative_scanned_areas` across iterations to encourage breadth.
  Equivalent here would be `cumulative_dom_assertions`: ensure each
  iteration probes a different selector set.

## Reference patterns reused

- **Plan → act session inheritance**:
  [`vibe_feature_dev.iter`](vibe_feature_dev.iter) lines 759-764
- **MCP server declaration + tool allowlist**:
  [`claw_mcp.iter`](claw_mcp.iter)
- **Human node final gate**:
  [`skill/human_gate.iter`](skill/human_gate.iter)
- **`interaction: human` for ask_user escalation**:
  [`vibe_feature_dev.iter`](vibe_feature_dev.iter) lines 642-649
