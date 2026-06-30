# supply-shield (Shieldy) — run bilans

Newest first. See [README](README.md) for the template.

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
