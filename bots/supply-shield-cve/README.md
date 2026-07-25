# supply-shield-cve (Vulny) — global supply-chain CVE shield

A PR / push-driven, **diff-scoped** CVE gate for dependency changes — the
CVE-focused sibling of `supply-shield` (Shieldy). Same pipeline, but the
analysis axis is **known vulnerabilities**, not malware: it inspects only
the dependency versions a change adds or upgrades, matches each against
the advisory databases, validates applicability, reuses a **shared
cache**, and **reports back onto the PR** (merge request on GitLab)
via the native forge API.

## What it does

1. **Diff-scopes** the run — diffs the changed lockfiles between
   `base..head` and inspects only the newly added/upgraded packages
   (`scope_mode=full` for a whole-tree CVE baseline).
2. **Universal CVE floor** — `trivy fs --scanners vuln` **and**
   `osv-scanner`, both over lockfiles, cross-ecosystem, **no install**
   needed. Plus per-ecosystem SCA (`npm audit` / `pip-audit` /
   `govulncheck`) when an installed tree / module graph is present.
3. **Anti-façade gate** — `coverage_gate` hard-fails when the CVE floor
   did not run, so a missing scanner never reads as "0 CVEs".
4. **Applicability review** — an LLM validates each advisory against the
   resolved version (affected range / fixed version / Go reachability)
   and de-dups cross-scanner duplicates.
5. **Shared store** — verdicts cache as `kind: cve` lines with a short TTL
   + `advisory_db_date` (a clean-today version can gain a CVE tomorrow).
6. **Reports back** — sticky PR comment + inline review + SARIF /
   code-scanning, plus board cards labelled `source:supply-shield-cve`.

## Run

```bash
# Diff-scope a PR working tree against main:
devbox run -- iterion run bots/supply-shield-cve/main.bot \
  --var workspace_dir=$(pwd) --var base_ref=origin/main

# Whole-tree CVE baseline:
devbox run -- iterion run bots/supply-shield-cve/main.bot \
  --var workspace_dir=$(pwd) --var scope_mode=full

# On-demand from the board / chat:  /cve <scope notes>
```

## Sandbox image, cache, forge reporting, triggers

Identical mechanics to `supply-shield` — see
[`../supply-shield/README.md`](../supply-shield/README.md). Pins
`ghcr.io/socialgouv/iterion-sandbox-sec:edge` (ships trivy + osv-scanner +
the per-ecosystem SCA tools); the shared cache + `kind` discriminator are
documented in [`skills/package-cache.md`](skills/package-cache.md); forge
endpoints in [`skills/forge-report.md`](skills/forge-report.md). V1 ships
the on-demand `/cve` command; automatic PR/push triggers are a follow-on.

## Skills

`supply-shield-cve` (playbook), `diff-scope`, `cve-scanning`,
`forge-report`, `malware-signals`, `package-cache`, `lang-js` / `lang-py`
/ `lang-go` / `lang-generic`, `iterion-board`. The `lang-*` /
`malware-signals` / `package-cache` skills are shared with
`sec-audit-deps` / `supply-shield` and duplicated per bundle (iterion has
no skill-sharing primitive yet — keep them in sync).
