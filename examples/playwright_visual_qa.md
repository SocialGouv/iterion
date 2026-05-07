# playwright_visual_qa — companion notes

Generic visual + functional QA workflow against any public web URL.
Demonstrates four iterion primitives in a single file, with no
runtime config beyond an OpenAI key:

- **Playwright MCP** integration via `mcp_server playwright`
- **LLM planning + observation + verdict** via `claw` backend
- **Mid-tool-loop `ask_user`** escalation on the judge
  (`interaction: human`)
- **Human sign-off gate** with Approve / Reject quick-action buttons
  in the run console (the schema's `approved: bool` triggers them)

The companion file is [`playwright_visual_qa.iter`](playwright_visual_qa.iter).

## Pre-requisites

Run-once on the host:

```bash
npx playwright install chromium                 # downloads the browser (~150 MB)
npx -y @playwright/mcp@latest --help            # warms the npm cache
echo "$OPENAI_API_KEY" | grep -q . && echo OK   # confirm the key is exported
```

The workflow defaults to OpenAI (`openai/gpt-5.4` for the heavy
nodes, `openai/gpt-5.4-mini` for plan/report). Swap the model
literals if you have a different provider — claw supports any
`provider/model-id` shape its registry knows about.

## How to run

```bash
set -a; source .env; set +a   # exports OPENAI_API_KEY
./iterion run examples/playwright_visual_qa.iter \
  --var target_url=https://playwright.dev \
  --var objective='Verify the homepage loads, the docs link in the navbar opens the docs, and the search box is reachable from the homepage'
```

Or via the editor's Launch modal: open
`examples/playwright_visual_qa.iter`, fill `target_url` + `objective`,
click Launch. The run pauses on `human_signoff` for your final
verdict; the run console's bottom drawer renders the verdict + the
collected observations with `Approve` / `Reject` buttons.

## What you should see

| Phase | Where | What |
|---|---|---|
| `plan_qa` | events log, brief | Numbered test steps + success criteria |
| `browse` | events log, ~1-2 min | Several `mcp.playwright.*` tool calls; screenshots saved by the MCP server |
| `judge_qa` | events log | Verdict; **may pause on `human_input_requested`** if it calls `ask_user` |
| `human_signoff` | run console panel | Drawer at the bottom: review the observations, click Approve or Reject |
| `report` | events log + `.iterion/qa-report.md`-ish | Final markdown summary in the artifact |

The judge can call `ask_user` mid-tool-loop when it can't decide
visually (contrast, polish, brand consistency). When it does, the
panel renders the question typed by the LLM as the value to review,
and your reply gets fed back into the judge's conversation —
session continuity preserved.

## Anti-façade rules baked in

The judge's system prompt enforces:

1. `screenshot_paths` must be non-empty (no screenshots ⇒ browse
   failed ⇒ rejected unconditionally).
2. `console_errors` non-empty ⇒ rejected unless every error is
   off-feature noise.
3. `network_failures` with status ≥ 500 ⇒ rejected.
4. Visually undecidable cases ⇒ MUST `ask_user`, MUST NOT invent.

Without these, the LLM tends to emit "looks good" verdicts on the
basis of the test plan text alone — see
[`docs/workflow_authoring_pitfalls.md`](../docs/workflow_authoring_pitfalls.md)
for the longer story.

## Loop semantics

The judge can request a retry by setting
`retry_recommended=true` and writing `additional_instructions`. The
edge `judge_qa -> browse when "!approved && retry_recommended"` is
loop-bound to `qa_loop(3)`; on exhaustion the run terminates
`failed` via the `judge_qa -> fail` catch-all.

The retry path replays the original `plan_summary` PLUS the
judge's instructions. The browse system prompt explicitly tells the
LLM to prioritize the additional instructions before re-running the
plan.

## Limitations

- The judge LLM doesn't actually *see* the screenshot pixels; it
  reads the file paths plus the DOM/console/network capture.
  Visually polish-sensitive verdicts are best escalated via
  `ask_user`. A future iteration can pass the screenshots as
  vision attachments (see `examples/vision_attachments.iter` if it
  exists in your tree).
- `npx -y @playwright/mcp@latest` is fetched on every run unless
  the npm cache is warm. The first-ever invocation can stretch
  `browse` past a minute.
- Sandbox is implicit `none` (no opt-in here). The MCP subprocess
  runs in the iterion process tree; it inherits its FS access.
  Restrict accordingly when running on shared infrastructure.

## When something doesn't work

- `claw backend: model: invalid spec ""` → check that
  `OPENAI_API_KEY` is exported in the shell where the editor /
  CLI was started, and that the model literal in the `.iter` is a
  valid `provider/model-id`.
- The browse node hangs without taking any screenshot → check
  that `npx playwright install chromium` was run; the MCP server
  blocks the first navigate while it downloads the browser
  otherwise.
- The judge always escalates via `ask_user` → tighten the
  `success_criteria` in your objective so the LLM has measurable
  signals; or override `interaction: human` to `none` if you want
  unattended runs (you lose the escalation safety net).
