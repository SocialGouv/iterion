# supply-shield-cve (Vulny) — run bilans

Newest first. See [README](README.md) for the template.

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
