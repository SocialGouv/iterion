# supply-shield (Shieldy) — run bilans

Newest first. See [README](README.md) for the template.

## 2026-06-30 — first SANDBOXED run from the sec image (runs 019f1783 / 019f1792 / 019f17.. dedup)

- **Status:** validated (sandboxed, end-to-end)
- **Versions:** bot supply-shield@0.1.0 · iterion 356fde8b6 · image
  `ghcr.io/socialgouv/iterion-sandbox-sec:edge` (locally rebuilt + CI
  `build-sandbox-sec` green — self-test passed: js-x-ray detected the eval,
  `osv-scanner 2.4.0` present)
- **Method:** local Docker driver, `iterion run` from inside
  `/tmp/ssfix/malware-target` (fixtures via
  `scripts/adhoc/supply-shield-fixtures.sh`), `--var base_ref=<.base>`,
  `--store-dir /tmp/ss-store`. No `--sandbox none`, no `--workdir`.
- **Proven for the first time on real infra:**
  - **Scanners come from the IMAGE** (no host shim): js-x-ray, trivy,
    npm-audit all ran inside the container; `coverage_gate` →
    **`degraded:false`** (`present:[generic.json, jsxray.json,
    npm-audit.json]`, `missing:[]`).
  - **Skills mirrored INSIDE the container** (`skills mirrored=11` into the
    worktree `.claude/skills`, bind-mounted into the sandbox).
  - **Malware detected.** `telemetry-helper@2.4.1`'s `postinstall.js` —
    `eval(atob(...))` → `https://exfil.example` C2 + `AWS_SECRET_ACCESS_KEY`/
    `NPM_TOKEN` theft. Notably js-x-ray hit a `parsing-error` on the install
    hook; the LLM reviewer's **deep-read fallback** caught it directly
    (anti-façade working).
  - **Cross-run dedup hits sandboxed.** 2nd run on the same target →
    `already_scanned:[telemetry-helper]`, `pending:[]`.
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

## 2026-06-30 — dedup + load_cache fixes, re-validated end-to-end

- **Status:** validated (all three findings from the first run fixed and
  re-dogfooded)
- **Versions:** bot supply-shield@0.1.0 · iterion (worktree, dedup fixes)
- **Method:** same unsandboxed setup as below, but run **from inside the
  target dir** (so `workDir` = target → skills mirror correctly) + host
  `iterion-jsxray` shim. Fresh target each time for a clean cache.
- **What was fixed (from the first run's findings):**
  1. **Cross-run dedup now reliable.** Added a deterministic
     `normalize_deps` node that coerces the LLM enumerator's output into
     canonical `[{ecosystem,name,version,checksum}]` regardless of the
     shape it emits (object array, `{name:[versions]}` map,
     `{ecosystem:[name@ver]}` map, or `name@version` strings) **and strips
     artifact-dir prefixes** (`node_modules/`, `site-packages/`, `vendor/`)
     so the name is the bare package id. The cache key changed from
     `ecosystem:name:version:checksum` to **`ecosystem:name:version`**
     (the LLM-fragile checksum left the key; it is now metadata compared
     on a hit to catch republish attacks → `_checksum_changed`). Proven:
     run 1 inspected `telemetry-helper` (pending 1, hit 0); **run 2 on the
     same target → `cache_hits:1, pending:0`** (no re-inspection). Applied
     to all three family bots (supply-shield, supply-shield-cve,
     sec-audit-deps).
  2. **Skill-mirroring was NOT an engine bug** — `mirrorBundleSkills` is
     unconditional and correct. The first run's empty
     `<target>/.claude/skills` was a dogfood-invocation error: passing
     `--var workspace_dir=<target>` does NOT move the engine's `workDir`
     (there is no `--workdir` flag; `workDir` defaults to cwd). Running
     **from inside the target** mirrors skills correctly — the reviewer
     read them immediately this time, **no runaway `grep`**. Documented as
     a usage note, no code change.
  3. **`load_cache` rewritten in python** (reads the path from env, not an
     inline `printf`) — emits clean JSON on a cold cache (`line_count: 0`,
     unquoted `cache_path`) instead of the old `"0 0"` / quoted-path glitch.
- **Re-validation result:** **risk 100 HIGH** on `telemetry-helper@2.4.1`
  again (5 signals, js-x-ray-confirmed `eval`, `is-minified` FP discarded,
  board issue filed), name now clean (`telemetry-helper`, prefix stripped),
  cache holds exactly one `kind:malware` line, dedup hit on re-run. The CVE
  sibling (Vulny) was validated in the same pass — see
  [supply-shield-cve.md](supply-shield-cve.md).

## 2026-06-30 — first dogfood: malware PR-gate end-to-end (run 019f1738)

- **Status:** validated (core) / partial (dedup + scanner floor)
- **Versions:** bot supply-shield@0.1.0 · iterion `2e357e40e` (main)
- **Method:** **unsandboxed** (`--sandbox none` — no docker daemon / sec image
  available on this host), so the scanner floor was partial. claude_code
  **opus 4.8** for `llm_review` + `forge_report`; `claw` (`openai/gpt-5.5` via
  codex ChatGPT forfait) for `enumerate_deps`. js-x-ray supplied by a host
  `iterion-jsxray` shim (real `@nodesecure/js-x-ray` 15.1.0 + the Dockerfile
  walker). Target: a crafted npm repo where the PR HEAD **adds** a malicious
  `telemetry-helper@2.4.1` whose `postinstall.js` reads
  `AWS_SECRET_ACCESS_KEY`/`NPM_TOKEN`, `eval(atob())`-decodes an exfil URL, and
  `cp.exec("curl …")`-exfiltrates via child_process. `base_ref` = the clean
  base commit. No forge token (so `forge_report` exercises its degrade path).
- **Result:** converged in **1 pass**, ~5m43s, ~$1.16 (forfait → effectively
  free). `diff_scope` correctly scoped to **only the newly-added dep**
  (`telemetry-helper`; the unchanged `left-pad` was excluded). `coverage_gate`
  passed (floor `generic.json` + `jsxray.json` present) and bannered the one gap
  (`npm-audit.json` — needs an install; CVE axis only). `llm_review` (opus) read
  the js-x-ray output + the package source and returned **risk 100/100 HIGH**,
  validating 5 signals (install-hook, eval-on-startup, base64-blob,
  child-process-on-import, network-exfil-shape) and **correctly discarding the
  `is-minified` false positive** on the 1-line stub `index.js` — exactly the
  FP discipline `js-xray.md` documents. Filed board issue `native:09b9af90`
  (`severity:high`), wrote `supply-shield-findings.md` (coverage table cleanly
  separates the covered malware axis from the degraded CVE axis), `sarif_gen`
  emitted valid SARIF 2.1.0 (5 error-level results), `forge_report` **correctly
  degraded to `mode=local-only` / `posted=false`** (no PR/token), `update_cache`
  appended one `kind:malware` line. Commits: none (read-only bot).
- **Value:** the headline capability — **js-x-ray AST malware detection driven
  by a real LLM reviewer** — is validated end-to-end. The reviewer's verdict was
  precise and well-evidenced (cited file:line for every signal, recommended
  blocking + secret rotation), and the anti-façade coverage banner did its job.
- **Findings / misses:**
  1. **Cross-run cache dedup is unreliable (the headline "no re-check"
     guarantee).** A second identical run did **not** hit the cache
     (`pending:[telemetry-helper]`, `cache_hits:0`) even though `load_cache`
     read the cached line (`line_count:1`) and the checksum was stable. Root
     cause: `enumerate_deps` (an LLM node) emitted a **different output shape**
     between runs — run 1 produced the schema-intended
     `deps:[{ecosystem,name,version,checksum,…}]`; run 2 collapsed it to a
     `{name:[versions]}` map with **no `ecosystem`/`checksum`**, so
     `filter_cached` built a malformed cache key and missed. The deterministic
     cache key depends on non-deterministic LLM structured output (the
     `enumerate_output.deps` schema is untyped `json`, so both shapes validate).
     **This affects sec-audit-deps (Depsy) too** — the enumerate+cache logic was
     copied from it. Fix direction: normalize `enumerate_deps` output in a
     deterministic post-node (coerce to the canonical per-dep array, compute the
     checksum by hashing the resolved artifact / lockfile integrity) so the key
     no longer rides on LLM consistency.
  2. **Skills were not mirrored to the target workspace** on this unsandboxed
     loose-`.bot` run (`<target>/.claude/skills/` was empty). The reviewer
     improvised a `grep -rl` across `$HOME` to find them and **timed out**
     (Bash exit 143, ~2 min wasted) before recovering. The verdict still
     succeeded (opus's native judgement + the js-x-ray signal were sufficient
     without the skills), but the wasted hunt is a real cost. Investigate
     whether skill-mirroring is gated on the sandbox/bundle path and should also
     fire for `--sandbox none` + a directory `main.bot`. Workaround until then:
     run sandboxed (the sec image mirrors skills), or pre-mirror.
  3. **`load_cache` cosmetic JSON glitch** on an empty cache: `line_count` came
     out `"0 0"` and `cache_path` was single-quoted. Inherited verbatim from
     Depsy; **harmless** (the fields aren't load-bearing — `filter_cached` reads
     `raw_jsonl`, which was correct). Worth a tidy-up across the family.
- **Engine hardening:** none committed from this run — findings #1/#2 want
  focused follow-ups (deterministic cache key; skill-mirror on unsandboxed
  loose-bot runs), tracked here rather than rushed.
- **Lessons for next run:**
  - Build/publish `iterion-sandbox-sec` (with the new Layer 5) and run
    **sandboxed** to exercise the real `iterion-jsxray` + the trivy/osv CVE
    floor + npm-audit, and to get skills mirrored automatically.
  - Don't trust cross-run dedup until `enumerate_deps` output is normalized
    deterministically — until then a re-run re-pays the LLM review.
  - The CVE sibling (supply-shield-cve / Vulny) was **not** run live yet
    (needs trivy/osv from the sec image); validate it once the image is built.
