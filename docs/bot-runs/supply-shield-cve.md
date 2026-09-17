# supply-shield-cve (Vulny) — run bilans

Newest first. See [README](README.md) for the template.

## 2026-09-17 — profile 2: CVE gate on the same lockfile — three advisories validated, MEDIUM (run 01a0af95-6521)

- Status: **validated end to end** — `diff_scope` → `enumerate_deps` → scanners →
  `llm_review` → `sarif_gen` → `forge_report` (nothing to post: no remote, no PR, no token)
  → `update_cache` → done.
- Versions: bot supply-shield-cve 0.1.2 (`dsl: 2`, wave 3b of #1344) · iterion `5fa790d82` for the bots; the engine the branch binary `v3.157.0+c31559517` (built on the wave-3a branch from main at v3.157.0; main is at v3.159.0 as this is written, one PR of engine work ahead), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records.
- Method: the supply-shield fixture (a scratch greeter repository on a branch adding
  `package.json` + `package-lock.json` with lodash 4.17.21), `--var base_ref=main --var
  head_ref=HEAD` plus a one-line `scope_notes`, `--store-dir` the operator's workspace store, `--sandbox none`,
  `--merge-into none`, caps `--max-cost-usd 1.5 --max-duration 10m`, every LLM node on
  `claude_code`/`claude-opus-5` (the enumerator's default `claw`/gpt-5.5 route is closed until
  2026-09-20).
- Result: $1.02, 6 min 32 s. Coverage healthy (2/2 scanners: the trivy + osv floor and
  `npm audit`); the floor corroborated nothing, `npm audit` alone carried the three GHSAs
  (GHSA-r5fr-rjxr-66jc high 8.1, GHSA-f23m-r3pf-42rh and GHSA-xxjr-mmjv-4gpg moderate 6.5),
  all applicable to a production dependency at 4.17.21 — MEDIUM, one board issue
  (`native:2daada7d-7368-414b-bfe5-028b04c732f4`, on the operator's local board: close it by
  hand), the aggregation note written. SARIF written, one cache line appended.
- Value: the six prompts' paragraph breaks reach the models (proven in the run's `events.jsonl`
  for the three system prompts a node rendered; the user prompts' first break sits next to a
  `{"{"}…}` value and is not a static witness), and the reviewer names its sole source instead of
  claiming corroboration it did not get.
- Findings / misses: same board-emission remark as supply-shield; nothing new.
- Engine hardening: none needed.
- Lessons for next run: unchanged from supply-shield — the fixture is the branch.

## 2026-06-30 — first SANDBOXED run from the sec image (runs 019f17ab + dedup run)

- **Status:** validated (sandboxed, end-to-end)
- **Versions:** bot supply-shield-cve@0.1.0 · iterion 356fde8b6 · image
  `ghcr.io/socialgouv/iterion-sandbox-sec:edge` (locally rebuilt + CI
  `build-sandbox-sec` green — self-test passed: js-x-ray detected the eval,
  `osv-scanner 2.4.0` present)
- **Method:** local Docker driver, `iterion run` from inside
  `/tmp/ssfix/cve-target` (fixtures via
  `scripts/adhoc/supply-shield-fixtures.sh`), `--var base_ref=<.base>`,
  `--store-dir /tmp/ss-store`. No `--sandbox none`, no `--workdir`.
- **Proven for the first time on real infra:**
  - **Scanners come from the IMAGE** (no host shim): js-x-ray, trivy,
    npm-audit all ran inside the container; `coverage_gate` →
    **`degraded:false`** (`present:[generic.json, npm-audit.json]`, `missing:[]`).
  - **Skills mirrored INSIDE the container** (`skills mirrored=11` into the
    worktree `.claude/skills`, bind-mounted into the sandbox).
  - **CVEs detected.** `lodash@4.17.4` → CVE-2019-10744 (CRITICAL,
    prototype pollution in defaultsDeep, trivy), CVE-2018-16487 (HIGH),
    npm-advisory-1106900 (critical: Prototype Pollution + Command Injection)
    with GHSA aliases + fixed_versions; combined `heuristic_score:100`. trivy
    + npm-audit DBs queried from the image over the open sandbox network.
  - **Cross-run dedup hits sandboxed.** 2nd run on the same target →
    `already_scanned:[lodash]`, `pending:[]`.
- **Engine/infra findings:**
  1. **Host-wide cache + sandbox = store-dir matters.** With the default
     store under `~/.iterion`, the worktree lives inside `~/.iterion`, so
     the `~/.iterion` host_state mount is SKIPPED (overlap rule,
     `sandbox.go:collectHostStateMounts`) → a `cache_path` under
     `~/.iterion` is unreachable in-container and `update_cache` dies with
     `PermissionError`. Fix: put the run store OUTSIDE `~/.iterion`
     (`--store-dir /tmp/ss-store`) so `~/.iterion` mounts RW and the cache
     persists + is writable. Worth documenting for any host-wide-cache
     sandboxed run.
  2. **enumerate_deps shape non-determinism (FIXED, 356fde8b6).** The LLM
     enumerator emits the new dep as `{pkg:[ver]}`, `{eco:[{name,…}]}` OR
     `{pkg:{version,path,change,…}}` across runs. The last shape (dict keyed
     by package name, value = details object WITHOUT a `name`) made
     `normalize_deps` drop it → 0 packages → no scan/dedup. Fixed by
     injecting the key as `name`. Copy-shared to supply-shield-cve.
- **Reported (decisions for the operator):**
  - **(b) board emit:** under sandbox the `__mcp-board` MCP server is
    unreachable (C082) — `mcp__iterion_board__create_issue` → "No such tool
    available". The bot handled it gracefully (recorded the error, still
    produced the verdict + local report). Needs the HTTP board MCP transport
    (`BoardHTTPEndpoint`/`BoardRunToken`) → run via studio/server, not a bare
    `iterion run`.
  - **(c) forge_report:** no forge token present → degraded to
    `mode=local-only` (report stays at `report_path`). Expected; posting for
    real needs a forge PR + token.

## 2026-06-30 — first dogfood: CVE PR-gate end-to-end

- **Status:** validated
- **Versions:** bot supply-shield-cve@0.1.0 · iterion (worktree, dedup fixes)
- **Method:** **unsandboxed** (`--sandbox none`; no docker / sec image on
  this host), run **from inside the target** so skills mirror + the
  `${PROJECT_DIR}` workspace resolves correctly. Host `trivy` 0.70.0 +
  `osv-scanner` 2.4.0 on PATH supplied the CVE floor (these ship in the
  `iterion-sandbox-sec` image; installed locally for the dogfood).
  claude_code **opus 4.8** for `llm_review` + `forge_report`; `claw`
  (`openai/gpt-5.5` via codex forfait) for `enumerate_deps`. Target: a
  crafted npm repo whose PR HEAD **adds** `lodash@4.17.4` (a release with
  many published advisories). No forge token (so `forge_report` exercises
  its degrade path).
- **Result:** converged in 1 pass. `diff_scope` scoped to the newly-added
  `lodash`; `coverage_gate` reported **`degraded: false`** (the trivy + osv
  floor both produced output). `llm_review` (opus) returned **risk 100/100
  HIGH**, matching **10 published advisories** against the pinned version —
  worst **CVE-2019-10744** (CRITICAL prototype pollution in `defaultsDeep`),
  plus CVE-2021-23337 (command injection), CVE-2020-8203, CVE-2018-16487,
  … — **de-duplicated the cross-scanner overlap** (npm-advisory GHSA
  aliases folded into the trivy CVE ids), determined a fix is available,
  and recommended upgrading to **4.18.0**. Filed board issue
  `native:312bb1d4` (`severity:high`, `type:cve`, `source:supply-shield-cve`),
  emitted valid SARIF, `forge_report` correctly degraded to local-only,
  `update_cache` appended one **`kind:cve`** line carrying
  `advisory_db_date: 2026-06-30` + a 7-day TTL.
- **Value:** the CVE axis is validated end-to-end — the universal trivy +
  osv floor surfaces real advisories from a **bare lockfile** (no
  `npm install`), the reviewer correctly anchors severity to the worst
  applicable advisory and names the fix version, and the result is reported
  back to the board. The `kind:cve` cache line (short TTL +
  `advisory_db_date`) coexists with malware lines in the shared store.
- **Findings / misses:** none blocking. Same shared `normalize_deps` /
  identity-cache-key fix as the malware sibling — a second run on the same
  target **hit the cache** (`cache_hits:1, pending:0`), so a clean version
  is not re-scanned within its TTL. The per-ecosystem `npm audit` /
  `pip-audit` heuristics still need an installed tree (the floor does not),
  so a node_modules-less repo relies on trivy + osv (which is the design).
- **Engine hardening:** none needed — the dedup + load_cache fixes landed
  with the malware bot; see [supply-shield.md](supply-shield.md).
- **Lessons for next run:** build/publish `iterion-sandbox-sec` (Layer 5)
  and run **sandboxed** so trivy/osv/the per-eco SCA come from the image
  rather than a host install; validate a multi-ecosystem target (npm + go
  + py) to exercise govulncheck / pip-audit alongside the floor.
