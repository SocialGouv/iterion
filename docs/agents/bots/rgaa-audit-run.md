# Run the RGAA audit yourself — the demo path

`rgaa-audit` (Acci) audits a product against the RGAA 4.1.2 standard and
produces a dated French report: conformance rate, findings per criterion,
a page grid, a coverage annex and the verdict ledger. The engine
([ultra11y](https://github.com/SocialGouv/ultra11y), pinned and
hash-verified) owns the judgement; iterion orchestrates the LLM
adjudication around it.

The fastest full tour audits **this repository's own studio** — the
dogfood target the sample and the committed ledger were built for.

```sh
task demo:rgaa-audit
```

## Prerequisites

- The repo toolchain: `devbox install` (node ≥ 22.18, go, pnpm — the
  Taskfile drives everything else).
- An authenticated `claude` CLI: the run's agents delegate through the
  `claude_code` backend, so they spend YOUR Claude budget. A full first
  run is ~$20-30 (`RGAA_DEMO_BUDGET` overrides the ceiling); the run
  replays the committed ledger (`.ultra11y/verdicts/rgaa.json`), so
  criteria that did not change are not re-adjudicated.
- A free port 4899 (the task fails fast when something already listens
  there — a leftover studio e2e serve, for instance).
- ~10-20 minutes of wall clock, most of it in the LLM rounds.

## What the task does

1. Builds the studio SPA and the iterion binary with the bot catalogue
   embedded.
2. Resolves the pinned ultra11y engine (`scripts/ultra11y-engine.sh` —
   it prefers a matching global npm install; otherwise the first use
   installs the engine under `~/.cache/iterion/ultra11y` after
   verifying the pin's size and sha256).
3. Installs the chromium build the engine's rendered scan navigates with
   (a browser already on the machine is named instead with the bot's
   `browser_path` var or `ULTRA11Y_BROWSER_PATH`, which the engine takes
   as `--browser-path`; the sandbox browser image names its
   `/usr/bin/chromium` that way).
4. Runs `iterion run bots/rgaa-audit` with `rendered=required`: the
   studio is served locally (`studio/e2e/serve.mjs`, seeded workspace),
   the 21-page sample (`.ultra11yrc.json`) is captured with Chromium,
   and every layer — static audit, rendered capture, ledger replay,
   LLM adjudication, counter-reading — runs to the final report.

The report, page grid, coverage annex and ledger land under
`.rgaa-demo/<UTC-timestamp>/`; the run's own state lives in
`.rgaa-demo/store`, `home` and `run`. The rendered captures are the
exception: the engine writes them under the audited checkout's
`.ultra11y/pages/` (gitignored, like the e2e workspace `serve.mjs`
seeds under `studio/e2e/.tmp/`) — so don't run the demo while a studio
e2e session is live. A previous demo's captures are not inherited:
`static_audit` purges that directory before the audit, so a second run
on the same checkout never ingests the first's stale probes. The audit
reads the checkout AS IT IS: uncommitted
edits to `studio/src` are part of what gets judged.

## Options and variants

- `RGAA_DEMO_BUDGET=50 task demo:rgaa-audit` — raise the spend ceiling.
- Post findings to the run's native board instead of stopping at the
  report: add `--var post_to_board=true` to the `iterion run` line (the
  board lives in the `--store-dir` store, `.rgaa-demo/store` — the
  GitHub 203 bridge is a separate, operator-owned push).
- Audit a DIFFERENT repo: copy the task's `iterion run` line, point
  `scope_globs` at the target, set `rendered=off` if it has no page
  server, and write your own `scope_notes`. The committed ledger only
  covers this studio; another repo starts from an empty ledger —
  expect ~$60 for ~100 criteria, so raise the ceiling accordingly
  (`RGAA_DEMO_BUDGET=80` or `--max-cost-usd 80`).
- `iterion resume --run-id <id> --store-dir .rgaa-demo/store …` resumes
  an interrupted run (add `--loop-budget-guard off` if the guard trips
  on the resumed budget). The measured ceiling of a full run is the
  bot's TOKEN budget, not dollars: the default 3M ran out at 91% before
  the counter-reading — the task ships `--max-tokens` 5M
  (`RGAA_DEMO_TOKENS` overrides), and a resume that worked for real:

  ```sh
  .rgaa-demo/bin/iterion resume --run-id <id> --store-dir .rgaa-demo/store \
    --max-tokens 4500000 --max-cost-usd 40 --loop-budget-guard off
  ```

## Limits

- The task is local-only by design: it pins `rendered=required`, and a
  missing browser under `required` FAILS the run (typed, not degraded).
  On a machine without chromium, drop to the static layer explicitly
  with `--var rendered=off` — the report then says so in its annex.
- `--sandbox none` ships in the task because the rendered layer needs a
  local browser and a local server; the bot's own nodes are read-only
  (no shell, no writes), so the exposure is the page capture, not your
  tree.
- The sample covers the studio's seeded pages; cloud-only screens
  (login, admin, teams) are out of sample.
- The weekly CI advisory (`ci/a11y-advisory`) runs the deterministic
  layers only (no LLM); a red advisory names the criteria the next run
  will re-adjudicate.

## The `rgaa` sandbox image class

A sandboxed run of this bot needs two things no stock default image
carries: a browser (Chromium) and the ultra11y engine. Both now exist as
a published image class (#2632, decision (a)): the `rgaa` variant —
`sandbox/rgaa/Dockerfile`, built FROM the browser base (Chromium +
Playwright MCP + `ULTRA11Y_BROWSER_PATH=/usr/bin/chromium`) with the
pinned ultra11y engine installed globally (`npm install -g
ultra11y@<pin>`, the pin held equal to `bots/rgaa-audit/ultra11y.pin` by
`bots/rgaa_audit_sandbox_rgaa_image_test.go`).

A bot opts in by declaring the class on its sandbox block:

```iter fragment:workflow
sandbox:
  image: rgaa    ## resolves at launch to ghcr.io/socialgouv/iterion-sandbox-rgaa:<version>
```

A bare class name (`slim`, `full`, `sec`, `browser`, `rgaa`) resolves to
the published variant pinned to the running iterion; anything else keeps
its meaning (a full `registry/repo:tag` reference is your own image; an
unknown bare word is verbatim, with the compiler's C325 saying so).

**Until the bot itself declares the class**, its `prepare` node keeps its
engine resolution ladder: `ULTRA11Y_BIN` → a matching global npm install
→ `ULTRA11Y_HOME` → a per-run `npm install ultra11y@<pin>` (fingerprint
verified either way). On the rgaa image the second arm answers, so the
per-run install disappears the day the bot declares `image: rgaa` — the
fallback stays as the honest answer on every other image.

## Running on iterion.cloud — what a sandboxed cloud run still does NOT get

A cloud run executes inside the run's sandbox pod, not in the runner
image. Decision (a) of
[#2632](https://github.com/SocialGouv/iterion/issues/2632) is delivered
(the `rgaa` image class, above); what follows is the honest interim
state until the bot declares it, plus the two decisions still pending.

- **The engine pre-install does not reach a run on the default image.**
  The runner image pre-installs the pinned ultra11y
  (`Dockerfile.runner-devbox`: `ARG ULTRA11Y_VERSION`, installed under
  `/opt/iterion/ultra11y` and exported as `ULTRA11Y_HOME`), but the
  sandbox pod defaults to `ghcr.io/socialgouv/iterion-sandbox-slim`
  (`pkg/runtime/sandbox_default_image.go`) — node + devbox + Nix, no
  ultra11y (`sandbox/slim/Dockerfile`). `prepare`'s candidate walk
  (`ULTRA11Y_BIN` → global npm → `ULTRA11Y_HOME`,
  `bots/rgaa-audit/main.bot`) therefore misses, and every cloud run on
  the default image pays one `npm install ultra11y@<pin>` into its run
  directory (`engine_source: "npm-install"`) — a per-run network fetch
  from the registry, fingerprinted like any other engine source. The
  `rgaa` class carries the engine pre-installed.
- **No browser on the default image, so `rendered=off` there.** The
  slim image ships no Chromium, so the rendered layer cannot run on
  iterion.cloud while the bot rides the default: launch with
  `rendered=off` (or let `auto` find nothing) and the coverage annex
  says the layer did not run. The `browser` class and the `rgaa` class
  both ship Chromium at `/usr/bin/chromium`, named to the bot through
  `ULTRA11Y_BROWSER_PATH`.
- **Board filing is off in the sandbox.** `publish` /
  `publish_verify` write through the board MCP transport; in a
  sandboxed run it is wired only when the runtime configured
  `BoardHTTPEndpoint` + `BoardRunToken`
  (`pkg/backend/delegate/claude_code_hooks.go`, `wireBoardMCP` — else
  the warning "board capabilities granted but workflow is sandboxed
  … board MCP disabled for this node"), and the server mounts those
  routes only when the NATIVE board store exists
  (`pkg/server/server.go`, `boardMCPServiceOption`). On iterion.cloud
  the board is the tenant-scoped Mongo board (`CloudBoardFor`), not
  the native store, so the endpoint is never configured and `publish`
  cannot file: launch with `post_to_board=false` — the issue plan
  stays a run artefact (`issues-<date>.json`). Making board writes
  deterministic server-side is the second decision pending in #2632.
- **Credentials.** A cloud launch spends the credentials of the
  account or team that launched it (BYOK); no platform key is
  provisioned for this bot.
